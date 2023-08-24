// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go's map type.
//
// A map is just a hash table. The data is arranged
// into an array of buckets. Each bucket contains up to
// 8 key/elem pairs. The low-order bits of the hash are
// used to select a bucket. Each bucket contains a few
// high-order bits of each hash to distinguish the entries
// within a single bucket.
//
// If more than 8 keys hash to a bucket, we chain on
// extra buckets.
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big. Buckets are incrementally
// copied from the old bucket array to the new bucket array.
//
// Map iterators walk through the array of buckets and
// return the keys in walk order (bucket #, then overflow
// chain order, then bucket index).  To maintain iteration
// semantics, we never move keys within their bucket (if
// we did, keys might be returned 0 or 2 times).  When
// growing the table, iterators remain iterating through the
// old table and must check the new table if the bucket
// they are iterating through has been moved ("evacuated")
// to the new table.

// Picking loadFactor: too large and we have lots of overflow
// buckets, too small and we waste a lot of space. I wrote
// a simple program to check some stats for different loads:
// (64-bit, 8 byte keys and elems)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/elem pair
// hitprobe    = # of entries to check when looking up a present key
// missprobe   = # of entries to check when looking up an absent key
//
// Keep in mind this data is for maximally loaded tables, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	// 桶容量
	// Maximum number of key/elem pairs a bucket can hold.
	bucketCntBits = abi.MapBucketCountBits
	bucketCnt     = abi.MapBucketCount

	// 最大平均负载
	// Maximum average load of a bucket that triggers growth is bucketCnt*13/16 (about 80% full)
	// Because of minimum alignment rules, bucketCnt is known to be at least 8.
	// Represent as loadFactorNum/loadFactorDen, to allow integer math.
	loadFactorDen = 2
	loadFactorNum = (bucketCnt * 13 / 16) * loadFactorDen

	// Maximum key or elem size to keep inline (instead of mallocing per element).
	// Must fit in a uint8.
	// Fast versions cannot handle big elems - the cutoff size for
	// fast versions in cmd/compile/internal/gc/walk.go must be at most this elem.
	maxKeySize  = abi.MapMaxKeyBytes
	maxElemSize = abi.MapMaxElemBytes

	// 桶的数据针对桶起始地址的偏移量
	// 由于内存大小不确定
	// 所以这里申请了内存但是没有成员指向
	// 每次操作需要指针指向操作
	// data offset should be the size of the bmap struct, but needs to be
	// aligned correctly. For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// tophash 标记值
	// 每个桶包括溢出桶都会是松散状态
	// 除了正在写入的调用 evacuate 方法
	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	emptyRest      = 0 // this cell is empty, and there are no more non-empty cells at higher indexes or overflows.
	emptyOne       = 1 // this cell is empty
	evacuatedX     = 2 // key/elem is valid.  Entry has been evacuated to first half of larger table.
	evacuatedY     = 3 // same as above, but evacuated to second half of larger table.
	evacuatedEmpty = 4 // cell is empty, bucket is evacuated.
	minTopHash     = 5 // minimum tophash for a normal filled cell.

	// map 状态标志位
	// flags
	iterator     = 1 // there may be an iterator using buckets
	oldIterator  = 2 // there may be an iterator using oldbuckets
	hashWriting  = 4 // a goroutine is writing to the map
	sameSizeGrow = 8 // the current map growth is to a new map of the same size

	// 用于迭代器不需要检测标志
	// sentinel bucket ID for iterator checks
	noCheck = 1<<(8*goarch.PtrSize) - 1
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// A header for a Go map.
type hmap struct {
	// 元素个数
	// 标志位
	// log2 桶的个数
	// 溢出桶的近似值
	// hash 种子
	// Note: the format of the hmap is also encoded in cmd/compile/internal/reflectdata/reflect.go.
	// Make sure this stays in sync with the compiler's definition.
	count     int // # live cells == size of map.  Must be first (used by len() builtin)
	flags     uint8
	B         uint8  // log_2 of # of buckets (can hold up to loadFactor * 2^B items)
	noverflow uint16 // approximate number of overflow buckets; see incrnoverflow for details
	hash0     uint32 // hash seed

	// 当前桶地址
	// 旧桶地址
	// 疏散计数器
	buckets    unsafe.Pointer // array of 2^B Buckets. may be nil if count==0.
	oldbuckets unsafe.Pointer // previous bucket array of half the size, non-nil only when growing
	nevacuate  uintptr        // progress counter for evacuation (buckets less than this have been evacuated)

	// 额外信息 溢出桶 可选字段
	extra *mapextra // optional fields
}

// mapextra holds fields that are not present on all maps.
type mapextra struct {
	// If both key and elem do not contain pointers and are inline, then we mark bucket
	// type as containing no pointers. This avoids scanning such maps.
	// However, bmap.overflow is a pointer. In order to keep overflow buckets
	// alive, we store pointers to all overflow buckets in hmap.extra.overflow and hmap.extra.oldoverflow.
	// overflow and oldoverflow are only used if key and elem do not contain pointers.
	// overflow contains overflow buckets for hmap.buckets.
	// oldoverflow contains overflow buckets for hmap.oldbuckets.
	// The indirection allows to store a pointer to the slice in hiter.
	overflow    *[]*bmap
	oldoverflow *[]*bmap

	// nextOverflow holds a pointer to a free overflow bucket.
	nextOverflow *bmap
}

/*
bmap 内存分布:
	tophashData uintptr
	tophashLen  int
	key1 		keyType
	key2 		keyType
	key3 		keyType
	key4 		keyType
	key5 		keyType
	key6 		keyType
	key7 		keyType
	key8 		keyType
	value1		valueType
	value2		valueType
	value3		valueType
	value4		valueType
	value5		valueType
	value6		valueType
	value7		valueType
	value8		valueType
	overflow	*bmap
*/
// A bucket for a Go map.
type bmap struct {
	// tophash generally contains the top byte of the hash value
	// for each key in this bucket. If tophash[0] < minTopHash,
	// tophash[0] is a bucket evacuation state instead.
	tophash [bucketCnt]uint8
	// Followed by bucketCnt keys and then bucketCnt elems.
	// NOTE: packing all the keys together and then all the elems together makes the
	// code a bit more complicated than alternating key/elem/key/elem/... but it allows
	// us to eliminate padding which would be needed for, e.g., map[int64]int8.
	// Followed by an overflow pointer.
}

// hiter 保证 32bit 和 64bit 都是 12 字节
// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/reflectdata/reflect.go
// and reflect/value.go to match the layout of this structure.
type hiter struct {
	key         unsafe.Pointer // Must be in first position.  Write nil to indicate iteration end (see cmd/compile/internal/walk/range.go).
	elem        unsafe.Pointer // Must be in second position (see cmd/compile/internal/walk/range.go).
	t           *maptype
	h           *hmap
	buckets     unsafe.Pointer // bucket ptr at hash_iter initialization time
	bptr        *bmap          // current bucket
	overflow    *[]*bmap       // keeps overflow buckets of hmap.buckets alive
	oldoverflow *[]*bmap       // keeps overflow buckets of hmap.oldbuckets alive
	startBucket uintptr        // bucket iteration started at
	offset      uint8          // intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1)
	wrapped     bool           // already wrapped around from end of bucket array to beginning
	B           uint8
	i           uint8
	bucket      uintptr
	checkBucket uintptr
}

// 通过 b 找到指定偏移的地址
// bucketShift returns 1<<b, optimized for code generation.
func bucketShift(b uint8) uintptr {
	// 1 << (b & 0x3F)
	// Masking the shift amount allows overflow checks to be elided.
	return uintptr(1) << (b & (goarch.PtrSize*8 - 1))
}

// 获取 shift 的掩码
// bucketMask returns 1<<b - 1, optimized for code generation.
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

//
// tophash calculates the tophash value for hash.
func tophash(hash uintptr) uint8 {
	// uint8(hash >> 56) 即取hash高8位
	top := uint8(hash >> (goarch.PtrSize*8 - 8))
	if top < minTopHash {
		// top 必须大于最小标记 否则会与标记冲突
		top += minTopHash
	}
	return top
}

// 是否需要重新整理
// tophash 有值 但是 没超过最小数 表示该桶是松散状态
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	return h > emptyOne && h < minTopHash
}

// 获取 b 的下一个溢出桶的地址
func (b *bmap) overflow(t *maptype) *bmap {
	// &b + bucketsize - 8
	// 即返回下一个bmap的地址
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize))
}

// 将 b 指向下个溢出桶的指针设置为 ovf
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize)) = ovf
}

func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// 增加溢出桶数
// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth.
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16.
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	if h.B < 16 {
		// B 小于 16 直接增加溢出次数
		h.noverflow++
		return
	}
	// 增长概率 1/(1<<(h.B-15))
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and fastrand & 7 == 0 with probability 1/8.
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

// 新建溢出桶
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// 有溢出桶可以用
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		ovf = h.extra.nextOverflow
		// 如果是连续内存 即预申请的空间 则溢出桶指向都是 nil
		// 如果不是连续内存 则溢出桶指向下一段溢出桶
		if ovf.overflow(t) == nil {
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.BucketSize)))
		} else {
			// 最后一个预分配的溢出桶
			// This is the last preallocated overflow bucket.
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value.
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		// 没有溢出桶可以用 就新建一个
		ovf = (*bmap)(newobject(t.Bucket))
	}
	// 尝试增加溢出桶数
	h.incrnoverflow()
	if t.Bucket.PtrBytes == 0 {
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf)
	}
	// 往 b 后面挂上 ovf
	b.setoverflow(t, ovf)
	return ovf
}

// 创建溢出桶容器
// 防止空指针异常
func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		// 32位系统溢出
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap_small implements Go map creation for make(map[k]v) and
// make(map[k]v, hint) when hint is known to be at most bucketCnt
// at compile time and the map needs to be allocated on the heap.
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand()
	return h
}

// makemap implements Go map creation for make(map[k]v, hint).
// If the compiler has determined that the map or the first bucket
// can be created on the stack, h and/or bucket may be non-nil.
// If h != nil, the map can be created directly in h.
// If h.buckets != nil, bucket pointed to can be used as the first bucket.
func makemap(t *maptype, hint int, h *hmap) *hmap {
	mem, overflow := math.MulUintptr(uintptr(hint), t.Bucket.Size_)
	if overflow || mem > maxAlloc {
		// 如果 hint 溢出或者超出最大申请 则重置 hint
		hint = 0
	}

	// initialize Hmap
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = fastrand()

	// 根据 hint 调整 B 到合适的值
	// Find the size parameter B which will hold the requested # of elements.
	// For hint < 0 overLoadFactor returns false since hint < bucketCnt.
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// 初始化 hash table
	// 如果 B 为 0 则懒式初始化
	// 如果 hint 很大 则会很慢
	// allocate initial hash table
	// if B == 0, the buckets field is allocated lazily later (in mapassign)
	// If hint is large zeroing this memory could take a while.
	if h.B != 0 {
		var nextOverflow *bmap
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			// 有溢出桶 就标记
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// 初始化桶 并返回下一个溢出桶的地址
// 1<<b 表示最小分配桶数
// dirtyalloc 应该是 nil 或之前由 makeBucketArray 分配的具有相同 t 和 b 参数的存储桶数组
// 如果 dirtyalloc 为 nil 则新分配一个数组
// 否则 dirtyalloc 会被置空并且复用
// makeBucketArray initializes a backing array for map buckets.
// 1<<b is the minimum number of buckets to allocate.
// dirtyalloc should either be nil or a bucket array previously
// allocated by makeBucketArray with the same t and b parameters.
// If dirtyalloc is nil a new backing array will be alloced and
// otherwise dirtyalloc will be cleared and reused as backing array.
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	base := bucketShift(b)
	// 桶的个数
	nbuckets := base
	// b 值小的话 不太需要溢出桶
	// For small b, overflow buckets are unlikely.
	// Avoid the overhead of the calculation.
	if b >= 4 {
		// 总桶数 = 需要桶数 + 溢出桶的个数
		// Add on the estimated number of overflow buckets
		// required to insert the median number of elements
		// used with this value of b.
		nbuckets += bucketShift(b - 4)
		// 计算所有桶的大小
		sz := t.Bucket.Size_ * nbuckets
		// 向上字节对齐
		up := roundupsize(sz)
		if up != sz {
			nbuckets = up / t.Bucket.Size_
		}
	}

	if dirtyalloc == nil {
		// 新建桶 申请内存
		buckets = newarray(t.Bucket, int(nbuckets))
	} else {
		// dirtyalloc was previously generated by
		// the above newarray(t.Bucket, int(nbuckets))
		// but may not be empty.
		buckets = dirtyalloc
		size := t.Bucket.Size_ * nbuckets
		if t.Bucket.PtrBytes != 0 {
			// 有指针数据 则清理指针
			memclrHasPointers(buckets, size)
		} else {
			// 无指针数据 则清理堆栈
			memclrNoHeapPointers(buckets, size)
		}
	}

	if base != nbuckets {
		// 申请了溢出桶 则进行标记
		// We preallocated some overflow buckets.
		// To keep the overhead of tracking these overflow buckets to a minimum,
		// we use the convention that if a preallocated overflow bucket's overflow
		// pointer is nil, then there are more available by bumping the pointer.
		// We need a safe non-nil pointer for the last overflow bucket; just use buckets.
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.BucketSize)))
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.BucketSize)))
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// 返回 h[key] 指向的内容 不会返回 nil
// 元素不存在 只会返回零值
// mapaccess1 returns a pointer to h[key].  Never returns nil, instead
// it will return a reference to the zero object for the elem type if
// the key is not in the map.
// NOTE: The returned pointer may keep the whole map live, so don't
// hold onto it for very long.
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		// 返回零值
		return unsafe.Pointer(&zeroVal[0])
	}
	if h.flags&hashWriting != 0 {
		// map 不支持并发读写
		fatal("concurrent map read and map write")
	}
	hash := t.Hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	if c := h.oldbuckets; c != nil {
		// 旧桶不为空
		if !h.sameSizeGrow() {
			// 不是相同大小的增长
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			// oldb 元素比较多 就赋值给 b
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				// 不是当前元素
				if b.tophash[i] == emptyRest {
					// 没有后续元素直接返回
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				// 返回值
				return e
			}
		}
	}
	// 没找到 返回零值
	return unsafe.Pointer(&zeroVal[0])
}

// 类似 mapaccess1
// 加上了返回是否找到
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map read and map write")
	}
	hash := t.Hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// 类似 mapaccess1
// 加上了返回 key value
// returns both key and elem. Used by map iterator.
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		// h 为空 直接返回
		return nil, nil
	}
	hash := t.Hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return k, e
			}
		}
	}
	return nil, nil
}

// 找到直接返回
// 没找到返回指定的默认值
func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

// 类似 mapaccess1_fat
// 多返回了是否找到
func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// 类似 mapaccess1
// 如果不存在则插入一个空slot
// Like mapaccess, but allocates a slot for the key if it is not present in the map.
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}
	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled {
		msanread(key, t.Key.Size_)
	}
	if asanenabled {
		asanread(key, t.Key.Size_)
	}
	if h.flags&hashWriting != 0 {
		// 不能同时写 map
		fatal("concurrent map writes")
	}
	hash := t.Hasher(key, uintptr(h.hash0))

	// t.hasher 可能会panic 所以标记写入必须在之后执行
	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write.
	h.flags ^= hashWriting

	if h.buckets == nil {
		h.buckets = newobject(t.Bucket) // newarray(t.Bucket, 1)
	}

again:
	// 通过 hash 获取桶坐标
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		// 是否需要扩容
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	top := tophash(hash)

	// tophash 地址
	// key 指针
	// elem 指针
	var inserti *uint8
	var insertk unsafe.Pointer
	var elem unsafe.Pointer
bucketloop:
	for {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				// 不是目标值
				if isEmpty(b.tophash[i]) && inserti == nil {
					// 标记空位
					// 只会标记最后一个空位
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				}
				if b.tophash[i] == emptyRest {
					// 后面没有空位 直接退出 loop
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if !t.Key.Equal(key, k) {
				continue
			}
			// already have a mapping for key. Update it.
			if t.NeedKeyUpdate() {
				typedmemmove(t.Key, k, key)
			}
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			goto done
		}
		// 跳转下一个溢出桶
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// Did not find mapping for key. Allocate new cell & add entry.

	// 是否扩容桶
	// If we hit the max load factor or we have too many overflow buckets,
	// and we're not already in the middle of growing, start growing.
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		// 没有扩容 并且 达到了负载因子或太多溢出桶
		// 执行扩容
		hashGrow(t, h)
		// 扩容会使上面获取的值失效 所以需要重新获取
		goto again // Growing the table invalidates everything, so try again
	}

	if inserti == nil {
		// 没有空位 表示当前桶及其溢出桶都满了
		// 申请一个新的溢出桶
		// The current bucket and all the overflow buckets connected to it are full, allocate a new one.
		newb := h.newoverflow(t, b)
		// 获取空位
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, bucketCnt*uintptr(t.KeySize))
	}

	// store new key/elem at insert position
	if t.IndirectKey() {
		kmem := newobject(t.Key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.IndirectElem() {
		vmem := newobject(t.Elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	typedmemmove(t.Key, insertk, key)
	*inserti = top
	h.count++

done:
	if h.flags&hashWriting == 0 {
		// 不能是写状态
		fatal("concurrent map writes")
	}
	// 重置写入标志位
	h.flags &^= hashWriting
	if t.IndirectElem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}

// 删除 key 并检测设置该桶的最后一个 emptyRest
func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		return
	}
	if h.flags&hashWriting != 0 {
		// 不能同时写
		fatal("concurrent map writes")
	}

	hash := t.Hasher(key, uintptr(h.hash0))

	// t.hasher 可能会 panic 所以需要在其后置 写入标记位
	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)
	if h.growing() {
		// 正在扩容 就执行扩容
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	bOrig := b
	top := tophash(hash)
search:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					// 删除不存在的 key 退出查找循环
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			k2 := k
			if t.IndirectKey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.Key.Equal(key, k2) {
				continue
			}
			// 只清理有指针的 key
			// Only clear key if there are pointers in it.
			if t.IndirectKey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.Key.PtrBytes != 0 {
				memclrHasPointers(k, t.Key.Size_)
			}
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			if t.IndirectElem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.Elem.PtrBytes != 0 {
				memclrHasPointers(e, t.Elem.Size_)
			} else {
				memclrNoHeapPointers(e, t.Elem.Size_)
			}
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			if i == bucketCnt-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					// 有溢出桶 跳转 notLast
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					// 后续仍有元素 跳转 notLast
					goto notLast
				}
			}
			// 后续没有元素 就把最后一个非空元素后面的的 tophash 都置为 emptyRest
			for {
				b.tophash[i] = emptyRest
				if i == 0 {
					// 是桶的第一元素
					if b == bOrig {
						// 所有桶都没有元素
						break // beginning of initial bucket, we're done.
					}
					// 找到 b 的前一个桶
					// Find previous bucket, continue at its last entry.
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = bucketCnt - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			h.count--
			// Reset the hash seed to make it more difficult for attackers to
			// repeatedly trigger hash collisions. See issue 25237.
			if h.count == 0 {
				// 如果没有元素 就重置 hash 种子
				h.hash0 = fastrand()
			}
			break search
		}
	}

	// 恢复标志位
	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 初始化 map 迭代器
// mapiterinit initializes the hiter struct used for ranging over maps.
// The hiter struct pointed to by 'it' is allocated on the stack
// by the compilers order pass or on the heap by reflect_mapiterinit.
// Both need to have zeroed hiter since the struct contains pointers.
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiterinit))
	}

	it.t = t
	if h == nil || h.count == 0 {
		// 没有元素直接返回
		return
	}

	// hiter 的大小必须是 12 字节
	if unsafe.Sizeof(hiter{})/goarch.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/reflectdata/reflect.go
	}
	it.h = h

	// grab snapshot of bucket state
	it.B = h.B
	it.buckets = h.buckets
	if t.Bucket.PtrBytes == 0 {
		// Allocate the current slice and remember pointers to both current and old.
		// This preserves all relevant overflow buckets alive even if
		// the table grows and/or overflow buckets are added to the table
		// while we are iterating.
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// decide where to start
	var r uintptr
	if h.B > 31-bucketCntBits {
		r = uintptr(fastrand64())
	} else {
		r = uintptr(fastrand())
	}
	// 起始迭代桶
	it.startBucket = r & bucketMask(h.B)
	// 迭代器偏移 随机值 保证每次遍历顺序不一致
	it.offset = uint8(r >> h.B & (bucketCnt - 1))

	// iterator state
	it.bucket = it.startBucket

	// Remember we have an iterator.
	// Can run concurrently with another mapiterinit().
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		// 旧的标志位没有包含迭代器状态 就置为迭代标志位
		atomic.Or8(&h.flags, iterator|oldIterator)
	}

	mapiternext(it)
}

func mapiternext(it *hiter) {
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiternext))
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map iteration and map write")
	}
	t := it.t
	bucket := it.bucket
	b := it.bptr
	i := it.i
	checkBucket := it.checkBucket

next:
	if b == nil {
		// 当前桶为空
		if bucket == it.startBucket && it.wrapped {
			// 迭代器已迭代完 直接返回
			// end of iteration
			it.key = nil
			it.elem = nil
			return
		}
		if h.growing() && it.B == h.B {
			// 正在扩容 并且 迭代器的桶数 和 map 的桶数相同
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
			if !evacuated(b) {
				// 不需要疏散
				checkBucket = bucket
			} else {
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
				checkBucket = noCheck
			}
		} else {
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
			checkBucket = noCheck
		}
		bucket++
		if bucket == bucketShift(it.B) {
			// 迭代器标记最后一个桶
			bucket = 0
			it.wrapped = true
		}
		i = 0
	}
	// 桶不为空 遍历所有元素
	for ; i < bucketCnt; i++ {
		offi := (i + it.offset) & (bucketCnt - 1)
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// 如果 tophash 为空就跳过
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			continue
		}
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.KeySize))
		if t.IndirectKey() {
			k = *((*unsafe.Pointer)(k))
		}
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(offi)*uintptr(t.ValueSize))
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// bucket 需要检测 并且 不是等大扩容
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			if t.ReflexiveKey() || t.Key.Equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				hash := t.Hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					// 如果旧桶不给新桶迁移 就跳过
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					// 低位都为 1 则跳过
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.ReflexiveKey() || t.Key.Equal(k, k)) {
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			it.key = k
			if t.IndirectElem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// 是疏散的 则需要查找当前 key
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				// key 被删除
				continue // key has been deleted
			}
			it.key = rk
			it.elem = re
		}
		// 修改迭代器记录
		it.bucket = bucket
		if it.bptr != b { // avoid unnecessary write barrier; see issue 14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		// 等待下次迭代
		return
	}
	// 跳转到下一个溢出桶
	b = b.overflow(t)
	i = 0
	goto next
}

// 清空 map 所有的 key
// mapclear deletes all keys from a map.
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	if h == nil || h.count == 0 {
		// 已经为空 直接返回
		return
	}

	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	// 清空所有标志位 标记值
	h.flags ^= hashWriting

	// Mark buckets empty, so existing iterators can be terminated, see issue #59411.
	markBucketsEmpty := func(bucket unsafe.Pointer, mask uintptr) {
		for i := uintptr(0); i <= mask; i++ {
			b := (*bmap)(add(bucket, i*uintptr(t.BucketSize)))
			for ; b != nil; b = b.overflow(t) {
				for i := uintptr(0); i < bucketCnt; i++ {
					b.tophash[i] = emptyRest
				}
			}
		}
	}
	markBucketsEmpty(h.buckets, bucketMask(h.B))
	if oldBuckets := h.oldbuckets; oldBuckets != nil {
		markBucketsEmpty(oldBuckets, h.oldbucketmask())
	}

	h.flags &^= sameSizeGrow
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// 重置 hash 种子
	// Reset the hash seed to make it more difficult for attackers to
	// repeatedly trigger hash collisions. See issue 25237.
	h.hash0 = fastrand()

	// 置空溢出桶
	// Keep the mapextra allocation but clear any extra information.
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// 新建桶
	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// 绑定溢出桶
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow
	}

	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	// 结束写占用
	h.flags &^= hashWriting
}

// 增长一倍扩容或者等大小扩容
func hashGrow(t *maptype, h *hmap) {
	// 超过了负载因子就扩容
	// 不扩容就会有大量溢出桶
	// 没超过负载因子保持当前大小扩容
	// If we've hit the load factor, get bigger.
	// Otherwise, there are too many overflow buckets,
	// so keep the same number of buckets and "grow" laterally.
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		// 没有超过负载因子就相同大小扩容
		bigger = 0
		h.flags |= sameSizeGrow
	}
	// 将桶变为旧桶 新建新桶
	oldbuckets := h.buckets
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 消除迭代标志位
	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		// 如果有桶迭代标志位就置旧桶迭代标志位
		flags |= oldIterator
	}
	// 提交本次增长
	// commit the grow (atomic wrt gc)
	h.B += bigger
	h.flags = flags
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// 将溢出桶设置为旧溢出桶
		// Promote current overflow buckets to the old generation.
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}
	if nextOverflow != nil {
		// 设置新的溢出桶
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// 数据迁移由 growWork() 和 evacuate() 逐步完成
	// the actual copying of the hash table data is done incrementally
	// by growWork() and evacuate().
}

// 判断第 1<<B 桶是否超过 loadFactor
// overLoadFactor reports whether count items placed in 1<<B buckets is over loadFactor.
func overLoadFactor(count int, B uint8) bool {
	// count > 8 && count > 13 * ( bucketShift(B) / 2 )
	// 13 / 2 为负载因子 6.5
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// 判断是否有太多溢出桶
// 溢出桶个数大于等于 1<<B
// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// Note that most of these overflow buckets must be in sparse use;
// if use was dense, then we'd have already triggered regular map growth.
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	return noverflow >= uint16(1)<<(B&15)
}

// 是否需要扩容 可能是大小相同或更大
// 旧桶不为空表示正在扩容
// growing reports whether h is growing. The growth may be to the same size or bigger.
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// 返回是否相同大小的增长
// sameSizeGrow reports whether the current growth is to a map of the same size.
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// 旧桶个数
// noldbuckets calculates the number of buckets prior to the current map growth.
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// 旧桶个数掩码
// oldbucketmask provides a mask that can be applied to calculate n % noldbuckets().
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// 执行扩容
func growWork(t *maptype, h *hmap, bucket uintptr) {
	// 疏散旧桶数
	// make sure we evacuate the oldbucket corresponding
	// to the bucket we're about to use
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	if h.growing() {
		// 旧桶还不为空 就继续执行疏散桶数
		evacuate(t, h, h.nevacuate)
	}
}

// 返回是否是疏散桶
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.BucketSize)))
	return evacuated(b)
}

// 疏散迭代器 表示当前值迁移的目的地
// evacDst is an evacuation destination.
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/elem index into b
	k unsafe.Pointer // pointer to current key storage
	e unsafe.Pointer // pointer to current elem storage
}

// 处理疏散桶
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
	newbit := h.noldbuckets()
	if !evacuated(b) {
		// 如果旧桶没有迭代器 则复用溢出桶 而不是使用新的
		// TODO: reuse overflow buckets instead of using new ones, if there
		// is no iterator using the old buckets.  (If !oldIterator.)

		// xy contains the x and y (low and high) evacuation destinations.
		var xy [2]evacDst
		x := &xy[0]
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.BucketSize)))
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.e = add(x.k, bucketCnt*uintptr(t.KeySize))

		if !h.sameSizeGrow() {
			// 只有在真正扩容时才计算 y 指针
			// 否则 GC 可以识别错误的指针
			// Only calculate y pointers if we're growing bigger.
			// Otherwise GC can see bad pointers.
			y := &xy[1]
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.BucketSize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, bucketCnt*uintptr(t.KeySize))
		}

		// 迁移桶内所有的数据
		for ; b != nil; b = b.overflow(t) {
			k := add(unsafe.Pointer(b), dataOffset)
			e := add(k, bucketCnt*uintptr(t.KeySize))
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.KeySize)), add(e, uintptr(t.ValueSize)) {
				top := b.tophash[i]
				if isEmpty(top) {
					// 如果当前槽标记为空 则标记为 疏散空
					b.tophash[i] = evacuatedEmpty
					continue
				}
				if top < minTopHash {
					// 其他标记都是错误的
					throw("bad map state")
				}
				k2 := k
				if t.IndirectKey() {
					k2 = *((*unsafe.Pointer)(k2))
				}
				var useY uint8
				if !h.sameSizeGrow() {
					// 是真的扩容就重新计算哈希值
					// Compute hash to make our evacuation decision (whether we need
					// to send this key/elem to bucket x or bucket y).
					hash := t.Hasher(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.ReflexiveKey() && !t.Key.Equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						useY = top & 1
						top = tophash(hash)
					} else {
						if hash&newbit != 0 {
							// hash 值与旧桶 bit 为 1 的就使用 Y 桶
							useY = 1
						}
					}
				}

				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				// 标记是否使用 Y 桶
				// dst 表示目标桶
				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // evacuation destination

				if dst.i == bucketCnt {
					// dst 满了 新建溢出桶
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, bucketCnt*uintptr(t.KeySize))
				}
				// 设置 top 值
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // mask dst.i as an optimization, to avoid a bounds check
				if t.IndirectKey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.Key, dst.k, k) // copy elem
				}
				if t.IndirectElem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.Elem, dst.e, e)
				}
				dst.i++
				// These updates might push these pointers past the end of the
				// key or elem arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				dst.k = add(dst.k, uintptr(t.KeySize))
				dst.e = add(dst.e, uintptr(t.ValueSize))
			}
		}
		// Unlink the overflow buckets & clear key/elem to help GC.
		if h.flags&oldIterator == 0 && t.Bucket.PtrBytes != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.BucketSize))
			// Preserve b.tophash because the evacuation
			// state is maintained there.
			ptr := add(b, dataOffset)
			n := uintptr(t.BucketSize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	if oldbucket == h.nevacuate {
		// 旧桶数 等于 疏散桶数 就执行调整疏散值
		advanceEvacuationMark(h, t, newbit)
	}
}

// 批量执行疏散 每次 1024 最多 newbit
func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		// 遍历 oldbucket 累加疏散值
		h.nevacuate++
	}
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// 表明扩容已完成
		// 释放旧桶 旧溢出桶
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		// 重置等大小扩容标志位
		h.flags &^= sameSizeGrow
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.Key.Equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.Key.Size_ > maxKeySize && (!t.IndirectKey() || t.KeySize != uint8(goarch.PtrSize)) ||
		t.Key.Size_ <= maxKeySize && (t.IndirectKey() || t.KeySize != uint8(t.Key.Size_)) {
		throw("key size wrong")
	}
	if t.Elem.Size_ > maxElemSize && (!t.IndirectElem() || t.ValueSize != uint8(goarch.PtrSize)) ||
		t.Elem.Size_ <= maxElemSize && (t.IndirectElem() || t.ValueSize != uint8(t.Elem.Size_)) {
		throw("elem size wrong")
	}
	if t.Key.Align_ > bucketCnt {
		throw("key align too big")
	}
	if t.Elem.Align_ > bucketCnt {
		throw("elem align too big")
	}
	if t.Key.Size_%uintptr(t.Key.Align_) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.Elem.Size_%uintptr(t.Elem.Align_) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.Key.Align_) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.Elem.Align_) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapaccess_faststr reflect.mapaccess_faststr
func reflect_mapaccess_faststr(t *maptype, h *hmap, key string) unsafe.Pointer {
	elem, ok := mapaccess2_faststr(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapassign reflect.mapassign0
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapassign_faststr reflect.mapassign_faststr0
func reflect_mapassign_faststr(t *maptype, h *hmap, key string, elem unsafe.Pointer) {
	p := mapassign_faststr(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapdelete_faststr reflect.mapdelete_faststr
func reflect_mapdelete_faststr(t *maptype, h *hmap, key string) {
	mapdelete_faststr(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap, it *hiter) {
	mapiterinit(t, h, it)
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

//go:linkname reflect_mapclear reflect.mapclear
func reflect_mapclear(t *maptype, h *hmap) {
	mapclear(t, h)
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

const maxZero = 1024 // must match value in reflect/value.go:maxZero cmd/compile/internal/gc/walk.go:zeroValSize
var zeroVal [maxZero]byte

// mapinitnoop is a no-op function known the Go linker; if a given global
// map (of the right size) is determined to be dead, the linker will
// rewrite the relocation (from the package init func) from the outlined
// map init function to this symbol. Defined in assembly so as to avoid
// complications with instrumentation (coverage, etc).
func mapinitnoop()

// mapclone for implementing maps.Clone
//
//go:linkname mapclone maps.clone
func mapclone(m any) any {
	e := efaceOf(&m)
	e.data = unsafe.Pointer(mapclone2((*maptype)(unsafe.Pointer(e._type)), (*hmap)(e.data)))
	return m
}

// moveToBmap moves a bucket from src to dst. It returns the destination bucket or new destination bucket if it overflows
// and the pos that the next key/value will be written, if pos == bucketCnt means needs to written in overflow bucket.
func moveToBmap(t *maptype, h *hmap, dst *bmap, pos int, src *bmap) (*bmap, int) {
	for i := 0; i < bucketCnt; i++ {
		if isEmpty(src.tophash[i]) {
			continue
		}

		for ; pos < bucketCnt; pos++ {
			if isEmpty(dst.tophash[pos]) {
				break
			}
		}

		if pos == bucketCnt {
			dst = h.newoverflow(t, dst)
			pos = 0
		}

		srcK := add(unsafe.Pointer(src), dataOffset+uintptr(i)*uintptr(t.KeySize))
		srcEle := add(unsafe.Pointer(src), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(i)*uintptr(t.ValueSize))
		dstK := add(unsafe.Pointer(dst), dataOffset+uintptr(pos)*uintptr(t.KeySize))
		dstEle := add(unsafe.Pointer(dst), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(pos)*uintptr(t.ValueSize))

		dst.tophash[pos] = src.tophash[i]
		if t.IndirectKey() {
			*(*unsafe.Pointer)(dstK) = *(*unsafe.Pointer)(srcK)
		} else {
			typedmemmove(t.Key, dstK, srcK)
		}
		if t.IndirectElem() {
			*(*unsafe.Pointer)(dstEle) = *(*unsafe.Pointer)(srcEle)
		} else {
			typedmemmove(t.Elem, dstEle, srcEle)
		}
		pos++
		h.count++
	}
	return dst, pos
}

func mapclone2(t *maptype, src *hmap) *hmap {
	dst := makemap(t, src.count, nil)
	dst.hash0 = src.hash0
	dst.nevacuate = 0
	//flags do not need to be copied here, just like a new map has no flags.

	if src.count == 0 {
		return dst
	}

	if src.flags&hashWriting != 0 {
		fatal("concurrent map clone and map write")
	}

	if src.B == 0 {
		dst.buckets = newobject(t.Bucket)
		dst.count = src.count
		typedmemmove(t.Bucket, dst.buckets, src.buckets)
		return dst
	}

	//src.B != 0
	if dst.B == 0 {
		dst.buckets = newobject(t.Bucket)
	}
	dstArraySize := int(bucketShift(dst.B))
	srcArraySize := int(bucketShift(src.B))
	for i := 0; i < dstArraySize; i++ {
		dstBmap := (*bmap)(add(dst.buckets, uintptr(i*int(t.BucketSize))))
		pos := 0
		for j := 0; j < srcArraySize; j += dstArraySize {
			srcBmap := (*bmap)(add(src.buckets, uintptr((i+j)*int(t.BucketSize))))
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
		}
	}

	if src.oldbuckets == nil {
		return dst
	}

	oldB := src.B
	srcOldbuckets := src.oldbuckets
	if !src.sameSizeGrow() {
		oldB--
	}
	oldSrcArraySize := int(bucketShift(oldB))

	for i := 0; i < oldSrcArraySize; i++ {
		srcBmap := (*bmap)(add(srcOldbuckets, uintptr(i*int(t.BucketSize))))
		if evacuated(srcBmap) {
			continue
		}

		if oldB >= dst.B { // main bucket bits in dst is less than oldB bits in src
			dstBmap := (*bmap)(add(dst.buckets, uintptr(i)&bucketMask(dst.B)))
			for dstBmap.overflow(t) != nil {
				dstBmap = dstBmap.overflow(t)
			}
			pos := 0
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
			continue
		}

		for srcBmap != nil {
			// move from oldBlucket to new bucket
			for i := uintptr(0); i < bucketCnt; i++ {
				if isEmpty(srcBmap.tophash[i]) {
					continue
				}

				if src.flags&hashWriting != 0 {
					fatal("concurrent map clone and map write")
				}

				srcK := add(unsafe.Pointer(srcBmap), dataOffset+i*uintptr(t.KeySize))
				if t.IndirectKey() {
					srcK = *((*unsafe.Pointer)(srcK))
				}

				srcEle := add(unsafe.Pointer(srcBmap), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					srcEle = *((*unsafe.Pointer)(srcEle))
				}
				dstEle := mapassign(t, dst, srcK)
				typedmemmove(t.Elem, dstEle, srcEle)
			}
			srcBmap = srcBmap.overflow(t)
		}
	}
	return dst
}

// keys for implementing maps.keys
//
//go:linkname keys maps.keys
func keys(m any, p unsafe.Pointer) {
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)

	if h == nil || h.count == 0 {
		return
	}
	s := (*slice)(p)
	r := int(fastrand())
	offset := uint8(r >> h.B & (bucketCnt - 1))
	if h.B == 0 {
		copyKeys(t, h, (*bmap)(h.buckets), s, offset)
		return
	}
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	for i := 0; i < arraySize; i++ {
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyKeys(t, h, b, s, offset)
	}

	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			if evacuated(b) {
				continue
			}
			copyKeys(t, h, b, s, offset)
		}
	}
	return
}

func copyKeys(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	for b != nil {
		for i := uintptr(0); i < bucketCnt; i++ {
			offi := (i + uintptr(offset)) & (bucketCnt - 1)
			if isEmpty(b.tophash[offi]) {
				continue
			}
			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}
			k := add(unsafe.Pointer(b), dataOffset+offi*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			typedmemmove(t.Key, add(s.array, uintptr(s.len)*uintptr(t.KeySize)), k)
			s.len++
		}
		b = b.overflow(t)
	}
}

// values for implementing maps.values
//
//go:linkname values maps.values
func values(m any, p unsafe.Pointer) {
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)
	if h == nil || h.count == 0 {
		return
	}
	s := (*slice)(p)
	r := int(fastrand())
	offset := uint8(r >> h.B & (bucketCnt - 1))
	if h.B == 0 {
		copyValues(t, h, (*bmap)(h.buckets), s, offset)
		return
	}
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	for i := 0; i < arraySize; i++ {
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyValues(t, h, b, s, offset)
	}

	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			if evacuated(b) {
				continue
			}
			copyValues(t, h, b, s, offset)
		}
	}
	return
}

func copyValues(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	for b != nil {
		for i := uintptr(0); i < bucketCnt; i++ {
			offi := (i + uintptr(offset)) & (bucketCnt - 1)
			if isEmpty(b.tophash[offi]) {
				continue
			}

			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}

			ele := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+offi*uintptr(t.ValueSize))
			if t.IndirectElem() {
				ele = *((*unsafe.Pointer)(ele))
			}
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			typedmemmove(t.Elem, add(s.array, uintptr(s.len)*uintptr(t.ValueSize)), ele)
			s.len++
		}
		b = b.overflow(t)
	}
}
