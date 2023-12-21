// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"internal/cpu"
	"internal/goarch"
	"internal/goexperiment"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// minPhysPageSize is a lower-bound on the physical page size. The
	// true physical page size may be larger than this. In contrast,
	// sys.PhysPageSize is an upper-bound on the physical page size.
	minPhysPageSize = 4096

	// maxPhysPageSize runtime支持最大的物理页大小
	// maxPhysPageSize is the maximum page size the runtime supports.
	maxPhysPageSize = 512 << 10

	// maxPhysHugePageSize runtime支持最大的huge page大小
	// maxPhysHugePageSize sets an upper-bound on the maximum huge page size
	// that the runtime supports.
	// 4M
	maxPhysHugePageSize = pallocChunkBytes

	// pagesPerReclaimerChunk 指示一次从 pageInUse 位图中扫描多少页，由页面回收器使用
	// 值越大 越减少对扫描的竞争但会增加操作延迟
	// 实验数据 512页限制在100µs
	// 必须是 pageInUse 位图元素大小的倍数，并且还必须平均划分 pagesPerArena。
	// pagesPerReclaimerChunk indicates how many pages to scan from the
	// pageInUse bitmap at a time. Used by the page reclaimer.
	//
	// Higher values reduce contention on scanning indexes (such as
	// h.reclaimIndex), but increase the minimum latency of the
	// operation.
	//
	// The time required to scan this many pages can vary a lot depending
	// on how many spans are actually freed. Experimentally, it can
	// scan for pages at ~300 GB/ms on a 2.6GHz Core i7, but can only
	// free spans at ~32 MB/ms. Using 512 pages bounds this at
	// roughly 100µs.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	pagesPerReclaimerChunk = 512

	// physPageAlignedStacks indicates whether stack allocations must be
	// physical page aligned. This is a requirement for MAP_STACK on
	// OpenBSD.
	physPageAlignedStacks = GOOS == "openbsd"
)

// mheap 内存管理单元
// 所占用内存为堆外内存
// Main malloc heap.
// The heap itself is the "free" and "scav" treaps,
// but all the other global data is here too.
//
// mheap must not be heap-allocated because it contains mSpanLists,
// which must not be heap-allocated.
type mheap struct {
	_ sys.NotInHeap

	// lock must only be acquired on the system stack, otherwise a g
	// could self-deadlock if its stack grows with the lock held.
	lock mutex

	pages pageAlloc // page allocation data structure

	// sweepgen 扫描者 id 生成 在 STW 期间写入
	sweepgen uint32 // sweep generation, see comment in mspan; written during STW

	// allspans is a slice of all mspans ever created. Each mspan
	// appears exactly once.
	//
	// The memory for allspans is manually managed and can be
	// reallocated and move as the heap grows.
	//
	// In general, allspans is protected by mheap_.lock, which
	// prevents concurrent access as well as freeing the backing
	// store. Accesses during STW might not hold the lock, but
	// must ensure that allocation cannot happen around the
	// access (since that may free the backing store).
	allspans []*mspan // all spans out there

	// 比例扫描
	// pagesInUse span 中处于 mSpanInUse 状态的 page 数
	// pagesSwept 在清扫的 page 数
	// pagesSweptBasis 扫描原点
	// sweepPagesPerByte 标记是否完成或者是否开启
	// Proportional sweep
	//
	// These parameters represent a linear function from gcController.heapLive
	// to page sweep count. The proportional sweep system works to
	// stay in the black by keeping the current page sweep count
	// above this line at the current gcController.heapLive.
	//
	// The line has slope sweepPagesPerByte and passes through a
	// basis point at (sweepHeapLiveBasis, pagesSweptBasis). At
	// any given time, the system is at (gcController.heapLive,
	// pagesSwept) in this space.
	//
	// It is important that the line pass through a point we
	// control rather than simply starting at a 0,0 origin
	// because that lets us adjust sweep pacing at any time while
	// accounting for current progress. If we could only adjust
	// the slope, it would create a discontinuity in debt if any
	// progress has already been made.
	pagesInUse         atomic.Uintptr // pages of spans in stats mSpanInUse
	pagesSwept         atomic.Uint64  // pages swept this cycle
	pagesSweptBasis    atomic.Uint64  // pagesSwept to use as the origin of the sweep ratio
	sweepHeapLiveBasis uint64         // value of gcController.heapLive to use as the origin of sweep ratio; written with lock, read without
	sweepPagesPerByte  float64        // proportional sweep ratio; written with lock, read without

	// Page reclaimer state

	// reclaimIndex is the page index in allArenas of next page to
	// reclaim. Specifically, it refers to page (i %
	// pagesPerArena) of arena allArenas[i / pagesPerArena].
	//
	// If this is >= 1<<63, the page reclaimer is done scanning
	// the page marks.
	reclaimIndex atomic.Uint64

	// reclaimCredit is spare credit for extra pages swept. Since
	// the page reclaimer works in large chunks, it may reclaim
	// more than requested. Any spare pages released go to this
	// credit pool.
	reclaimCredit atomic.Uintptr

	_ cpu.CacheLinePad // prevents false-sharing between arenas and preceding variables

	// arenas is the heap arena map. It points to the metadata for
	// the heap for every arena frame of the entire usable virtual
	// address space.
	//
	// Use arenaIndex to compute indexes into this array.
	//
	// For regions of the address space that are not backed by the
	// Go heap, the arena map contains nil.
	//
	// Modifications are protected by mheap_.lock. Reads can be
	// performed without locking; however, a given entry can
	// transition from nil to non-nil at any time when the lock
	// isn't held. (Entries never transitions back to nil.)
	//
	// In general, this is a two-level mapping consisting of an L1
	// map and possibly many L2 maps. This saves space when there
	// are a huge number of arena frames. However, on many
	// platforms (even 64-bit), arenaL1Bits is 0, making this
	// effectively a single-level map. In this case, arenas[0]
	// will never be nil.
	arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena

	// arenasHugePages indicates whether arenas' L2 entries are eligible
	// to be backed by huge pages.
	arenasHugePages bool

	// heapArenaAlloc is pre-reserved space for allocating heapArena
	// objects. This is only used on 32-bit, where we pre-reserve
	// this space to avoid interleaving it with the heap itself.
	heapArenaAlloc linearAlloc

	// arenaHints is a list of addresses at which to attempt to
	// add more heap arenas. This is initially populated with a
	// set of general hint addresses, and grown with the bounds of
	// actual heap arena ranges.
	arenaHints *arenaHint

	// arena is a pre-reserved space for allocating heap arenas
	// (the actual arenas). This is only used on 32-bit.
	arena linearAlloc

	// allArenas is the arenaIndex of every mapped arena. This can
	// be used to iterate through the address space.
	//
	// Access is protected by mheap_.lock. However, since this is
	// append-only and old backing arrays are never freed, it is
	// safe to acquire mheap_.lock, copy the slice header, and
	// then release mheap_.lock.
	allArenas []arenaIdx

	// sweepArenas is a snapshot of allArenas taken at the
	// beginning of the sweep cycle. This can be read safely by
	// simply blocking GC (by disabling preemption).
	sweepArenas []arenaIdx

	// markArenas is a snapshot of allArenas taken at the beginning
	// of the mark cycle. Because allArenas is append-only, neither
	// this slice nor its contents will change during the mark, so
	// it can be read safely.
	markArenas []arenaIdx

	// curArena 是堆当前区域，这应该始终与physPageSize对齐。
	// curArena is the arena that the heap is currently growing
	// into. This should always be physPageSize-aligned.
	curArena struct {
		base, end uintptr
	}

	// central 每个尺寸的空闲列表
	// pad 填充 确保每个 central 都可以完整占用 cacheline
	// central 由尺寸等级索引
	// central free lists for small size classes.
	// the padding makes sure that the mcentrals are
	// spaced CacheLinePadSize bytes apart, so that each mcentral.lock
	// gets its own cache line.
	// central is indexed by spanClass.
	central [numSpanClasses]struct {
		mcentral mcentral
		pad      [(cpu.CacheLinePadSize - unsafe.Sizeof(mcentral{})%cpu.CacheLinePadSize) % cpu.CacheLinePadSize]byte
	}

	// 各种分配器
	// spanalloc mspan 分配器
	// cachealloc mcache 分配器
	// arenaHintAlloc arenaHint 分配器
	spanalloc              fixalloc // allocator for span*
	cachealloc             fixalloc // allocator for mcache*
	specialfinalizeralloc  fixalloc // allocator for specialfinalizer*
	specialprofilealloc    fixalloc // allocator for specialprofile*
	specialReachableAlloc  fixalloc // allocator for specialReachable
	specialPinCounterAlloc fixalloc // allocator for specialPinCounter
	speciallock            mutex    // lock for special record allocators.
	arenaHintAlloc         fixalloc // allocator for arenaHints

	// User arena state.
	//
	// Protected by mheap_.lock.
	userArena struct {
		// arenaHints is a list of addresses at which to attempt to
		// add more heap arenas for user arena chunks. This is initially
		// populated with a set of general hint addresses, and grown with
		// the bounds of actual heap arena ranges.
		arenaHints *arenaHint

		// quarantineList is a list of user arena spans that have been set to fault, but
		// are waiting for all pointers into them to go away. Sweeping handles
		// identifying when this is true, and moves the span to the ready list.
		quarantineList mSpanList

		// readyList is a list of empty user arena spans that are ready for reuse.
		readyList mSpanList
	}

	unused *specialfinalizer // never set, just here to force the specialfinalizer type into DWARF
}

// mheap_ mheap 全局堆内存管理分配器
var mheap_ mheap

// heapArena 管理Go堆内存
// page 大小 8K
// 单个 heapArena 有 8K page
// A heapArena stores metadata for a heap arena. heapArenas are stored
// outside of the Go heap and accessed via the mheap_.arenas index.
type heapArena struct {
	_ sys.NotInHeap

	// heapArenaPtrScalar contains pointer/scalar data about the heap for this heap arena.
	heapArenaPtrScalar

	// spans 基于虚拟地址的页ID对应 mspan 的映射
	// 对于已经分配的 mspan 它们的页映射它们自己
	// 对于空闲的 mspan 只有最低和最高的页映射他们自己
	// 内部的 mspan 可以指向任意 mspan
	// spans maps from virtual address page ID within this arena to *mspan.
	// For allocated spans, their pages map to the span itself.
	// For free spans, only the lowest and highest pages map to the span itself.
	// Internal pages map to an arbitrary span.
	// For pages that have never been allocated, spans entries are nil.
	//
	// Modifications are protected by mheap.lock. Reads can be
	// performed without locking, but ONLY from indexes that are
	// known to contain in-use or stack spans. This means there
	// must not be a safe-point between establishing that an
	// address is live and looking it up in the spans array.
	spans [pagesPerArena]*mspan

	// 标记哪些 mspan 被使用
	// pageInUse is a bitmap that indicates which spans are in
	// state mSpanInUse. This bitmap is indexed by page number,
	// but only the bit corresponding to the first page in each
	// span is used.
	//
	// Reads and writes are atomic.
	pageInUse [pagesPerArena / 8]uint8

	// pageMarks is a bitmap that indicates which spans have any
	// marked objects on them. Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically during marking. Reads are
	// non-atomic and lock-free since they only occur during
	// sweeping (and hence never race with writes).
	//
	// This is used to quickly find whole spans that can be freed.
	//
	// TODO(austin): It would be nice if this was uint64 for
	// faster scanning, but we don't have 64-bit atomic bit
	// operations.
	pageMarks [pagesPerArena / 8]uint8

	// pageSpecials is a bitmap that indicates which spans have
	// specials (finalizers or other). Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically whenever a special is added to
	// a span and whenever the last special is removed from a span.
	// Reads are done atomically to find spans containing specials
	// during marking.
	pageSpecials [pagesPerArena / 8]uint8

	// checkmarks stores the debug.gccheckmark state. It is only
	// used if debug.gccheckmark > 0.
	checkmarks *checkmarksMap

	// 标记当前 heapArena 未使用的基址偏移
	// 单调递增 直到为 heapArenaBytes
	// 使用原子读和 cas 写
	// zeroedBase marks the first byte of the first page in this
	// arena which hasn't been used yet and is therefore already
	// zero. zeroedBase is relative to the arena base.
	// Increases monotonically until it hits heapArenaBytes.
	//
	// This field is sufficient to determine if an allocation
	// needs to be zeroed because the page allocator follows an
	// address-ordered first-fit policy.
	//
	// Read atomically and written with an atomic CAS.
	zeroedBase uintptr
}

// arenaHint 记录堆的增长位置，是一个单链表
// arenaHint is a hint for where to grow the heap arenas. See
// mheap_.arenaHints.
type arenaHint struct {
	_    sys.NotInHeap
	addr uintptr
	down bool
	next *arenaHint
}

// mspan 管理多个 page
// 当 mspan 在空闲树堆中 状态为 mSpanFree
// 并且第一个和最后一个 page 指向 mspan
// 当 mspan 在扫描树堆中 scavenged == true 其他情况都是 false
// 当 mspan 被申请 状态为 mSpanInUse 或 mSpanManual
// 并且区间内所有 page 都指向 mspan
// 每个 mspan 都在一个双向链表中
// 要么在 mheap 的 busy list 或者 m central 的 span list
// An mspan is a run of pages.
//
// When a mspan is in the heap free treap, state == mSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// If the mspan is in the heap scav treap, then in addition to the
// above scavenged == true. scavenged == false in all other cases.
//
// When a mspan is allocated, state == mSpanInUse or mSpanManual
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.

// Every mspan is in one doubly-linked list, either in the mheap's
// busy list or one of the mcentral's span lists.

// mspan 的状态转换
// - 在任何 GC 阶段 mspan
// 		可能会从 free 过渡到 in-use 或 manual
// - 在 GC 清扫阶段 gcphase == _GCoff
// 		mspan 可能被清扫由 in-use 转为 free
// 		也可能是释放栈内存由 manual 转为 free
// - 在 GC 阶段 gcphase != _GCoff
// 		mspan 不能从 manual 或 in-use 转为 free
// 		因为并发 GC 可能会读取一个指针然后查找它的 span
// 		所以 span 状态必须稳定的

// An mspan representing actual memory has state mSpanInUse,
// mSpanManual, or mSpanFree. Transitions between these states are
// constrained as follows:
//
//   - A span may transition from free to in-use or manual during any GC
//     phase.
//
//   - During sweeping (gcphase == _GCoff), a span may transition from
//     in-use to free (as a result of sweeping) or manual to free (as a
//     result of stacks being freed).
//
//   - During GC (gcphase != _GCoff), a span *must not* transition from
//     manual or in-use to free. Because concurrent GC may read a pointer
//     and then look up its span, the span state must be monotonic.
//
// Setting mspan.state to mSpanInUse or mSpanManual must be done
// atomically and only after all other span fields are valid.
// Likewise, if inspecting a span is contingent on it being
// mSpanInUse, the state should be loaded atomically and checked
// before depending on other fields. This allows the garbage collector
// to safely deal with potentially invalid pointers, since resolving
// such pointers may race with a span being allocated.
type mSpanState uint8

// mspan 的状态
const (
	mSpanDead   mSpanState = iota
	mSpanInUse             // allocated for garbage collected heap
	mSpanManual            // allocated for manual management (e.g., stack allocator)
)

// mSpanStateNames are the names of the span states, indexed by
// mSpanState.
var mSpanStateNames = []string{
	"mSpanDead",
	"mSpanInUse",
	"mSpanManual",
}

// mSpanStateBox 提供原子操作 mSpanState 的结构盒子
// mSpanStateBox holds an atomic.Uint8 to provide atomic operations on
// an mSpanState. This is a separate type to disallow accidental comparison
// or assignment with mSpanState.
type mSpanStateBox struct {
	s atomic.Uint8
}

// It is nosplit to match get, below.

//go:nosplit
func (b *mSpanStateBox) set(s mSpanState) {
	b.s.Store(uint8(s))
}

// It is nosplit because it's called indirectly by typedmemclr,
// which must not be preempted.

//go:nosplit
func (b *mSpanStateBox) get() mSpanState {
	return mSpanState(b.s.Load())
}

// mSpanList mspan 链表的头结点
// mSpanList heads a linked list of spans.
type mSpanList struct {
	_     sys.NotInHeap
	first *mspan // first span in list, or nil if none
	last  *mspan // last span in list, or nil if none
}

type mspan struct {
	_ sys.NotInHeap
	// mspan 链表的前后指针和链表头节点
	next *mspan     // next span in list, or nil if none
	prev *mspan     // previous span in list, or nil if none
	list *mSpanList // For debugging.

	// startAddr span 第一个位置的地址
	// npages span 占用的页数
	startAddr uintptr // address of first byte of span aka s.base()
	npages    uintptr // number of pages in span

	// 需要手动管理内存的链表 一般是栈内存
	manualFreeList gclinkptr // list of free objects in mSpanManual spans

	// 标记下一个扫描空闲对象的起始点
	// freeindex 表示 0 到 nelems 之间槽的索引
	// 从该索引开始扫描下一个空闲对象
	// 每个分配都从 freeindex 开始扫描 allocBits
	// 直到遇到表示空闲对象的 0
	// 然后调整 freeindex 以便随后的扫描在新发现的空闲对象之后开始
	// 如果 freeindex == nelem 表示当前 mspan 没有空闲对象
	// allocBits 是 span 对象的位图
	// n >= freeindex 并且 allocBits[n/8]&(1<<(n&7)) == 0 表示对象 n 是空闲的
	// 否则对象 n 被申请出去了
	// 以 nelem 开始的位是未定义的 永远不会被引用
	// 对象 n 的起始地址 n * elemsize + (start << pageShift)
	// freeindex is the slot index between 0 and nelems at which to begin scanning
	// for the next free object in this span.
	// Each allocation scans allocBits starting at freeindex until it encounters a 0
	// indicating a free object. freeindex is then adjusted so that subsequent scans begin
	// just past the newly discovered free object.
	//
	// If freeindex == nelem, this span has no free objects.
	//
	// allocBits is a bitmap of objects in this span.
	// If n >= freeindex and allocBits[n/8] & (1<<(n%8)) is 0
	// then object n is free;
	// otherwise, object n is allocated. Bits starting at nelem are
	// undefined and should never be referenced.
	//
	// Object n starts at address n*elemsize + (start << pageShift).
	freeindex uint16
	// span 有多少元素
	// TODO: Look up nelems from sizeclass and remove this field if it
	// helps performance.
	nelems uint16 // number of object in the span.
	// freeIndexForScan is like freeindex, except that freeindex is
	// used by the allocator whereas freeIndexForScan is used by the
	// GC scanner. They are two fields so that the GC sees the object
	// is allocated only when the object and the heap bits are
	// initialized (see also the assignment of freeIndexForScan in
	// mallocgc, and issue 54596).
	freeIndexForScan uint16

	// 按位标记是否分配出去了 startAddr 相对的地址
	// allocBits 的补码 用于快速查找内存中未被使用的内存
	// allocCache 可能包含超出 s.nelems 的位 调用者必须忽略这些
	// Cache of the allocBits at freeindex. allocCache is shifted
	// such that the lowest bit corresponds to the bit freeindex.
	// allocCache holds the complement of allocBits, thus allowing
	// ctz (count trailing zero) to use it directly.
	// allocCache may contain bits beyond s.nelems; the caller must ignore
	// these.
	allocCache uint64

	// allocBits 和 gcmarkBits 标记内存占用和回收情况
	// 指针按照 8 字节对齐
	// - free 不在访问且能被重复使用的脏数据区域
	// - next 保存要在下一个 GC 使用的信息
	// - current 当前 GC 中使用的信息
	// - previous 上一个 GC 中使用的信息
	// 一个新的 GC 由调用 finishsweep_m 开始
	// finishsweep_m 将 previous 移到 free
	// current 移到 previous
	// next 移到 current
	// next 由 span 请求内存以保存下一个 GC 周期的 gcmarkBits 以及 allocBits 为新申请的 span 时填充
	// allocBits and gcmarkBits hold pointers to a span's mark and
	// allocation bits. The pointers are 8 byte aligned.
	// There are three arenas where this data is held.
	// free: Dirty arenas that are no longer accessed
	//       and can be reused.
	// next: Holds information to be used in the next GC cycle.
	// current: Information being used during this GC cycle.
	// previous: Information being used during the last GC cycle.
	// A new GC cycle starts with the call to finishsweep_m.
	// finishsweep_m moves the previous arena to the free arena,
	// the current arena to the previous arena, and
	// the next arena to the current arena.
	// The next arena is populated as the spans request
	// memory to hold gcmarkBits for the next GC cycle as well
	// as allocBits for newly allocated spans.
	//
	// The pointer arithmetic is done "by hand" instead of using
	// arrays to avoid bounds checks along critical performance
	// paths.
	// The sweep will free the old allocBits and set allocBits to the
	// gcmarkBits. The gcmarkBits are replaced with a fresh zeroed
	// out memory.
	allocBits  *gcBits
	gcmarkBits *gcBits
	pinnerBits *gcBits // bitmap for pinned objects; accessed atomically

	// sweepgen 擦除标识
	// h->sweepgen - 2 	需要被清理
	// h->sweepgen - 1 	正在被清理
	// h->sweepgen		已被清理等待被使用
	// h->sweepgen + 1	扫描开始之前被缓存并且仍然被缓存并且需要清理
	// h->sweepgen + 3	已被清理并且一直被缓存
	// h->sweepgen 每次 GC 后 +2
	// sweep generation:
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	// if sweepgen == h->sweepgen + 1, the span was cached before sweep began and is still cached, and needs sweeping
	// if sweepgen == h->sweepgen + 3, the span was swept and then cached and is still cached
	// h->sweepgen is incremented by 2 after every GC

	// sweeepgen 清理者 id
	// spanclass 标记当前所属的尺寸组
	// state 标记当前的状态
	// needzero 在申请之前是否需要清零
	// elemsize 元素大小
	// limit span 中最后一个数据
	// speciallock 保护 specials
	// specials 特殊记录链表 基于 offset 排序
	sweepgen              uint32
	divMul                uint32        // for divide by elemsize
	allocCount            uint16        // number of allocated objects
	spanclass             spanClass     // size class and noscan (uint8)
	state                 mSpanStateBox // mSpanInUse etc; accessed atomically (get/set methods)
	needzero              uint8         // needs to be zeroed before allocation
	isUserArenaChunk      bool          // whether or not this span represents a user arena
	allocCountBeforeCache uint16        // a copy of allocCount that is stored just before this span is cached
	elemsize              uintptr       // computed from sizeclass or from npages
	limit                 uintptr       // end of data in span
	speciallock           mutex         // guards specials list and changes to pinnerBits
	specials              *special      // linked list of special records sorted by offset.
	userArenaChunkFree    addrRange     // interval for managing chunk allocation
	largeType             *_type        // malloc header for large objects.
}

// base 返回 s 的基址
func (s *mspan) base() uintptr {
	return s.startAddr
}

// layout 返回 元素大小 元素个数 总大小
func (s *mspan) layout() (size, n, total uintptr) {
	total = s.npages << _PageShift
	size = s.elemsize
	if size > 0 {
		n = total / size
	}
	return
}

// recordspan adds a newly allocated span to h.allspans.
//
// This only happens the first time a span is allocated from
// mheap.spanalloc (it is not called when a span is reused).
//
// Write barriers are disallowed here because it can be called from
// gcWork when allocating new workbufs. However, because it's an
// indirect call from the fixalloc initializer, the compiler can't see
// this.
//
// The heap lock must be held.
//
//go:nowritebarrierrec
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) {
	h := (*mheap)(vh)
	s := (*mspan)(p)

	assertLockHeld(&h.lock)

	if len(h.allspans) >= cap(h.allspans) {
		n := 64 * 1024 / goarch.PtrSize
		if n < cap(h.allspans)*3/2 {
			n = cap(h.allspans) * 3 / 2
		}
		var new []*mspan
		sp := (*slice)(unsafe.Pointer(&new))
		sp.array = sysAlloc(uintptr(n)*goarch.PtrSize, &memstats.other_sys)
		if sp.array == nil {
			throw("runtime: cannot allocate memory")
		}
		sp.len = len(h.allspans)
		sp.cap = n
		if len(h.allspans) > 0 {
			copy(new, h.allspans)
		}
		oldAllspans := h.allspans
		*(*notInHeapSlice)(unsafe.Pointer(&h.allspans)) = *(*notInHeapSlice)(unsafe.Pointer(&new))
		if len(oldAllspans) != 0 {
			sysFree(unsafe.Pointer(&oldAllspans[0]), uintptr(cap(oldAllspans))*unsafe.Sizeof(oldAllspans[0]), &memstats.other_sys)
		}
	}
	h.allspans = h.allspans[:len(h.allspans)+1]
	h.allspans[len(h.allspans)-1] = s
}

// spanClass 代表不同大小的尺寸等级
// 每个等级都有 scan 和 noscan 两个 spanClass
// noscan 仅包含不需要扫描的对象 即不包含指针 不需要被 GC 扫描
// A spanClass represents the size class and noscan-ness of a span.
//
// Each size class has a noscan spanClass and a scan spanClass. The
// noscan spanClass contains only noscan objects, which do not contain
// pointers and thus do not need to be scanned by the garbage
// collector.
type spanClass uint8

const (
	numSpanClasses = _NumSizeClasses << 1
	tinySpanClass  = spanClass(tinySizeClass<<1 | 1)
)

// makeSpanClass 创建指定等级且是否扫描的等级
func makeSpanClass(sizeclass uint8, noscan bool) spanClass {
	return spanClass(sizeclass<<1) | spanClass(bool2int(noscan))
}

// sizeclass 返回尺寸等级
//go:nosplit
func (sc spanClass) sizeclass() int8 {
	return int8(sc >> 1)
}

// noscan 判断是否需要扫描
// 奇数 noscan 偶数 scan
//go:nosplit
func (sc spanClass) noscan() bool {
	return sc&1 != 0
}

// arenaIndex 地址索引
// 地址减去全局基址再除以每个 heapArena 的大小
// 就可以获取在哪个 arenaIndex
// arenaIndex returns the index into mheap_.arenas of the arena
// containing metadata for p. This index combines of an index into the
// L1 map and an index into the L2 map and should be used as
// mheap_.arenas[ai.l1()][ai.l2()].
//
// If p is outside the range of valid heap addresses, either l1() or
// l2() will be out of bounds.
//
// It is nosplit because it's called by spanOf and several other
// nosplit functions.
//
//go:nosplit
func arenaIndex(p uintptr) arenaIdx {
	// p 减去基址 然后 除以 每个 heapArena 的大小 就能获取在哪个 heapArena id
	return arenaIdx((p - arenaBaseOffset) / heapArenaBytes)
}

// arenaBase 返回指定 arena 的基址
// arenaBase returns the low address of the region covered by heap
// arena i.
func arenaBase(i arenaIdx) uintptr {
	return uintptr(i)*heapArenaBytes + arenaBaseOffset
}

// arenaIdx heapArena 索引
type arenaIdx uint

// 返回地址区域的一级索引
// l1 returns the "l1" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit
// functions.
//
//go:nosplit
func (i arenaIdx) l1() uint {
	if arenaL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		return 0
	} else {
		return uint(i) >> arenaL1Shift
	}
}

// 返回地址区域的二级索引
// l2 returns the "l2" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit funcs.
// functions.
//
//go:nosplit
func (i arenaIdx) l2() uint {
	if arenaL1Bits == 0 {
		return uint(i)
	} else {
		return uint(i) & (1<<arenaL2Bits - 1)
	}
}

// inheap 返回 b 指向空间是否在堆上
// b 指向 mSpanManual 即栈空间 返回 false
// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into mSpanManual spans.
// Non-preemptible because it is used by write barriers.
//
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool {
	return spanOfHeap(b) != nil
}

// inHeapOrStack 类似 inheap 指向栈空间也返回 true
// inHeapOrStack is a variant of inheap that returns true for pointers
// into any allocated heap span.
//
//go:nowritebarrier
//go:nosplit
func inHeapOrStack(b uintptr) bool {
	s := spanOf(b)
	if s == nil || b < s.base() {
		return false
	}
	switch s.state.get() {
	case mSpanInUse, mSpanManual:
		return b < s.limit
	default:
		return false
	}
}

// spanOf 根据地址p返回p所在的span，如果p是堆外内存则返回nil
// spanOf returns the span of p. If p does not point into the heap
// arena or no span has ever contained p, spanOf returns nil.
//
// If p does not point to allocated memory, this may return a non-nil
// span that does *not* contain p. If this is a possibility, the
// caller should either call spanOfHeap or check the span bounds
// explicitly.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOf(p uintptr) *mspan {
	// This function looks big, but we use a lot of constant
	// folding around arenaL1Bits to get it under the inlining
	// budget. Also, many of the checks here are safety checks
	// that Go needs to do anyway, so the generated code is quite
	// short.
	ri := arenaIndex(p)
	if arenaL1Bits == 0 {
		// If there's no L1, then ri.l1() can't be out of bounds but ri.l2() can.
		if ri.l2() >= uint(len(mheap_.arenas[0])) {
			return nil
		}
	} else {
		// If there's an L1, then ri.l1() can be out of bounds but ri.l2() can't.
		if ri.l1() >= uint(len(mheap_.arenas)) {
			return nil
		}
	}
	l2 := mheap_.arenas[ri.l1()]
	if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1.
		return nil
	}
	ha := l2[ri.l2()]
	if ha == nil {
		return nil
	}
	return ha.spans[(p/pageSize)%pagesPerArena]
}

// spanOfUnchecked 和spanOf相同，但调用者必须确保 p 指向的是已申请的 Go 堆内存
// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into an allocated heap arena.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfUnchecked(p uintptr) *mspan {
	ai := arenaIndex(p)
	return mheap_.arenas[ai.l1()][ai.l2()].spans[(p/pageSize)%pagesPerArena]
}

// spanOfHeap 和spanOf类似，如果p没有指向堆对象，返回nil
// spanOfHeap is like spanOf, but returns nil if p does not point to a
// heap object.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfHeap(p uintptr) *mspan {
	s := spanOf(p)
	// s is nil if it's never been allocated. Otherwise, we check
	// its state first because we don't trust this pointer, so we
	// have to synchronize with span initialization. Then, it's
	// still possible we picked up a stale span pointer, so we
	// have to check the span's bounds.
	if s == nil || s.state.get() != mSpanInUse || p < s.base() || p >= s.limit {
		return nil
	}
	return s
}

// pageIndexOf 返回地址p所在的 arena， page index，page mask，调用者确保p在Go堆上
// pageIndexOf returns the arena, page index, and page mask for pointer p.
// The caller must ensure p is in the heap.
func pageIndexOf(p uintptr) (arena *heapArena, pageIdx uintptr, pageMask uint8) {
	ai := arenaIndex(p)
	arena = mheap_.arenas[ai.l1()][ai.l2()]
	pageIdx = ((p / pageSize) / 8) % uintptr(len(arena.pageInUse))
	pageMask = byte(1 << ((p / pageSize) % 8))
	return
}

// init 初始化 Go 堆
// Initialize the heap.
func (h *mheap) init() {
	lockInit(&h.lock, lockRankMheap)
	lockInit(&h.speciallock, lockRankMheapSpecial)

	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys)
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)
	h.specialReachableAlloc.init(unsafe.Sizeof(specialReachable{}), nil, nil, &memstats.other_sys)
	h.specialPinCounterAlloc.init(unsafe.Sizeof(specialPinCounter{}), nil, nil, &memstats.other_sys)
	h.arenaHintAlloc.init(unsafe.Sizeof(arenaHint{}), nil, nil, &memstats.other_sys)

	// Don't zero mspan allocations. Background sweeping can
	// inspect a span concurrently with allocating it, so it's
	// important that the span's sweepgen survive across freeing
	// and re-allocating a span to prevent background sweeping
	// from improperly cas'ing it from 0.
	//
	// This is safe because mspan contains no heap pointers.
	h.spanalloc.zero = false

	// h->mapcache needs no init

	// 初始化每个 central
	for i := range h.central {
		h.central[i].mcentral.init(spanClass(i))
	}

	h.pages.init(&h.lock, &memstats.gcMiscSys, false)
}

// reclaim sweeps and reclaims at least npage pages into the heap.
// It is called before allocating npage pages to keep growth in check.
//
// reclaim implements the page-reclaimer half of the sweeper.
//
// h.lock must NOT be held.
func (h *mheap) reclaim(npage uintptr) {
	// TODO(austin): Half of the time spent freeing spans is in
	// locking/unlocking the heap (even with low contention). We
	// could make the slow path here several times faster by
	// batching heap frees.

	// Bail early if there's no more reclaim work.
	if h.reclaimIndex.Load() >= 1<<63 {
		return
	}

	// Disable preemption so the GC can't start while we're
	// sweeping, so we can read h.sweepArenas, and so
	// traceGCSweepStart/Done pair on the P.
	mp := acquirem()

	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	arenas := h.sweepArenas
	locked := false
	for npage > 0 {
		// Pull from accumulated credit first.
		if credit := h.reclaimCredit.Load(); credit > 0 {
			take := credit
			if take > npage {
				// Take only what we need.
				take = npage
			}
			if h.reclaimCredit.CompareAndSwap(credit, credit-take) {
				npage -= take
			}
			continue
		}

		// Claim a chunk of work.
		idx := uintptr(h.reclaimIndex.Add(pagesPerReclaimerChunk) - pagesPerReclaimerChunk)
		if idx/pagesPerArena >= uintptr(len(arenas)) {
			// Page reclaiming is done.
			h.reclaimIndex.Store(1 << 63)
			break
		}

		if !locked {
			// Lock the heap for reclaimChunk.
			lock(&h.lock)
			locked = true
		}

		// Scan this chunk.
		nfound := h.reclaimChunk(arenas, idx, pagesPerReclaimerChunk)
		if nfound <= npage {
			npage -= nfound
		} else {
			// Put spare pages toward global credit.
			h.reclaimCredit.Add(nfound - npage)
			npage = 0
		}
	}
	if locked {
		unlock(&h.lock)
	}

	trace = traceAcquire()
	if trace.ok() {
		trace.GCSweepDone()
		traceRelease(trace)
	}
	releasem(mp)
}

// reclaimChunk sweeps unmarked spans that start at page indexes [pageIdx, pageIdx+n).
// It returns the number of pages returned to the heap.
//
// h.lock must be held and the caller must be non-preemptible. Note: h.lock may be
// temporarily unlocked and re-locked in order to do sweeping or if tracing is
// enabled.
func (h *mheap) reclaimChunk(arenas []arenaIdx, pageIdx, n uintptr) uintptr {
	// The heap lock must be held because this accesses the
	// heapArena.spans arrays using potentially non-live pointers.
	// In particular, if a span were freed and merged concurrently
	// with this probing heapArena.spans, it would be possible to
	// observe arbitrary, stale span pointers.
	assertLockHeld(&h.lock)

	n0 := n
	var nFreed uintptr
	sl := sweep.active.begin()
	if !sl.valid {
		return 0
	}
	for n > 0 {
		ai := arenas[pageIdx/pagesPerArena]
		ha := h.arenas[ai.l1()][ai.l2()]

		// Get a chunk of the bitmap to work on.
		arenaPage := uint(pageIdx % pagesPerArena)
		inUse := ha.pageInUse[arenaPage/8:]
		marked := ha.pageMarks[arenaPage/8:]
		if uintptr(len(inUse)) > n/8 {
			inUse = inUse[:n/8]
			marked = marked[:n/8]
		}

		// Scan this bitmap chunk for spans that are in-use
		// but have no marked objects on them.
		for i := range inUse {
			inUseUnmarked := atomic.Load8(&inUse[i]) &^ marked[i]
			if inUseUnmarked == 0 {
				continue
			}

			for j := uint(0); j < 8; j++ {
				if inUseUnmarked&(1<<j) != 0 {
					s := ha.spans[arenaPage+uint(i)*8+j]
					if s, ok := sl.tryAcquire(s); ok {
						npages := s.npages
						unlock(&h.lock)
						if s.sweep(false) {
							nFreed += npages
						}
						lock(&h.lock)
						// Reload inUse. It's possible nearby
						// spans were freed when we dropped the
						// lock and we don't want to get stale
						// pointers from the spans array.
						inUseUnmarked = atomic.Load8(&inUse[i]) &^ marked[i]
					}
				}
			}
		}

		// Advance.
		pageIdx += uintptr(len(inUse) * 8)
		n -= uintptr(len(inUse) * 8)
	}
	sweep.active.end(sl)
	trace := traceAcquire()
	if trace.ok() {
		unlock(&h.lock)
		// Account for pages scanned but not reclaimed.
		trace.GCSweepSpan((n0 - nFreed) * pageSize)
		traceRelease(trace)
		lock(&h.lock)
	}

	assertLockHeld(&h.lock) // Must be locked on return.
	return nFreed
}

// spanAllocType represents the type of allocation to make, or
// the type of allocation to be freed.
type spanAllocType uint8

const (
	// spanAllocHeap          堆 span
	// spanAllocStack         栈 span
	// spanAllocPtrScalarBits GC prog 位图 span
	// spanAllocWorkBuf       workbuf span
	spanAllocHeap          spanAllocType = iota // heap span
	spanAllocStack                              // stack span
	spanAllocPtrScalarBits                      // unrolled GC prog bitmap span
	spanAllocWorkBuf                            // work buf span
)

// manual 是否是手动管理的内存
// manual returns true if the span allocation is manually managed.
func (s spanAllocType) manual() bool {
	return s != spanAllocHeap
}

// alloc 从 Go 堆申请包含 npages 页的 span
// 里面元素标记大小尺寸为 spanclass
// alloc allocates a new span of npage pages from the GC'd heap.
//
// spanclass indicates the span's size class and scannability.
//
// Returns a span that has been fully initialized. span.needzero indicates
// whether the span has been zeroed. Note that it may not be.
func (h *mheap) alloc(npages uintptr, spanclass spanClass) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	var s *mspan
	systemstack(func() {
		// To prevent excessive heap growth, before allocating n pages
		// we need to sweep and reclaim at least n pages.
		if !isSweepDone() {
			h.reclaim(npages)
		}
		s = h.allocSpan(npages, spanAllocHeap, spanclass)
	})
	return s
}

// allocManual 手动申请 span
// allocManual allocates a manually-managed span of npage pages.
// allocManual returns nil if allocation fails.
//
// allocManual adds the bytes used to *stat, which should be a
// memstats in-use field. Unlike allocations in the GC'd heap, the
// allocation does *not* count toward heapInUse.
//
// The memory backing the returned span may not be zeroed if
// span.needzero is set.
//
// allocManual must be called on the system stack because it may
// acquire the heap lock via allocSpan. See mheap for details.
//
// If new code is written to call allocManual, do NOT use an
// existing spanAllocType value and instead declare a new one.
//
//go:systemstack
func (h *mheap) allocManual(npages uintptr, typ spanAllocType) *mspan {
	if !typ.manual() {
		// 手动管理的内存不能是堆内存
		throw("manual span allocation called with non-manually-managed type")
	}
	return h.allocSpan(npages, typ, 0)
}

// setSpans modifies the span map so [spanOf(base), spanOf(base+npage*pageSize))
// is s.
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
	p := base / pageSize
	ai := arenaIndex(base)
	ha := h.arenas[ai.l1()][ai.l2()]
	for n := uintptr(0); n < npage; n++ {
		i := (p + n) % pagesPerArena
		if i == 0 {
			ai = arenaIndex(base + n*pageSize)
			ha = h.arenas[ai.l1()][ai.l2()]
		}
		ha.spans[i] = s
	}
}

// allocNeedsZero checks if the region of address space [base, base+npage*pageSize),
// assumed to be allocated, needs to be zeroed, updating heap arena metadata for
// future allocations.
//
// This must be called each time pages are allocated from the heap, even if the page
// allocator can otherwise prove the memory it's allocating is already zero because
// they're fresh from the operating system. It updates heapArena metadata that is
// critical for future page allocations.
//
// There are no locking constraints on this method.
func (h *mheap) allocNeedsZero(base, npage uintptr) (needZero bool) {
	for npage > 0 {
		ai := arenaIndex(base)
		ha := h.arenas[ai.l1()][ai.l2()]

		zeroedBase := atomic.Loaduintptr(&ha.zeroedBase)
		arenaBase := base % heapArenaBytes
		if arenaBase < zeroedBase {
			// We extended into the non-zeroed part of the
			// arena, so this region needs to be zeroed before use.
			//
			// zeroedBase is monotonically increasing, so if we see this now then
			// we can be sure we need to zero this memory region.
			//
			// We still need to update zeroedBase for this arena, and
			// potentially more arenas.
			needZero = true
		}
		// We may observe arenaBase > zeroedBase if we're racing with one or more
		// allocations which are acquiring memory directly before us in the address
		// space. But, because we know no one else is acquiring *this* memory, it's
		// still safe to not zero.

		// Compute how far into the arena we extend into, capped
		// at heapArenaBytes.
		arenaLimit := arenaBase + npage*pageSize
		if arenaLimit > heapArenaBytes {
			arenaLimit = heapArenaBytes
		}
		// Increase ha.zeroedBase so it's >= arenaLimit.
		// We may be racing with other updates.
		for arenaLimit > zeroedBase {
			if atomic.Casuintptr(&ha.zeroedBase, zeroedBase, arenaLimit) {
				break
			}
			zeroedBase = atomic.Loaduintptr(&ha.zeroedBase)
			// Double check basic conditions of zeroedBase.
			if zeroedBase <= arenaLimit && zeroedBase > arenaBase {
				// The zeroedBase moved into the space we were trying to
				// claim. That's very bad, and indicates someone allocated
				// the same region we did.
				throw("potentially overlapping in-use allocations detected")
			}
		}

		// Move base forward and subtract from npage to move into
		// the next arena, or finish.
		base += arenaLimit - arenaBase
		npage -= (arenaLimit - arenaBase) / pageSize
	}
	return
}

// tryAllocMSpan 尝试从p获取mspan
// tryAllocMSpan attempts to allocate an mspan object from
// the P-local cache, but may fail.
//
// h.lock need not be held.
//
// This caller must ensure that its P won't change underneath
// it during this function. Currently to ensure that we enforce
// that the function is run on the system stack, because that's
// the only place it is used now. In the future, this requirement
// may be relaxed if its use is necessary elsewhere.
//
//go:systemstack
func (h *mheap) tryAllocMSpan() *mspan {
	pp := getg().m.p.ptr()
	// If we don't have a p or the cache is empty, we can't do
	// anything here.
	if pp == nil || pp.mspancache.len == 0 {
		return nil
	}
	// 获取最后一个mspan
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// allocMSpanLocked allocates an mspan object.
//
// h.lock must be held.
//
// allocMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
//go:systemstack
func (h *mheap) allocMSpanLocked() *mspan {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	if pp == nil {
		// We don't have a p so just do the normal thing.
		return (*mspan)(h.spanalloc.alloc())
	}
	// Refill the cache if necessary.
	if pp.mspancache.len == 0 {
		const refillCount = len(pp.mspancache.buf) / 2
		for i := 0; i < refillCount; i++ {
			pp.mspancache.buf[i] = (*mspan)(h.spanalloc.alloc())
		}
		pp.mspancache.len = refillCount
	}
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// freeMSpanLocked free an mspan object.
//
// h.lock must be held.
//
// freeMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
//go:systemstack
func (h *mheap) freeMSpanLocked(s *mspan) {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	// First try to free the mspan directly to the cache.
	if pp != nil && pp.mspancache.len < len(pp.mspancache.buf) {
		pp.mspancache.buf[pp.mspancache.len] = s
		pp.mspancache.len++
		return
	}
	// Failing that (or if we don't have a p), just free it to
	// the heap.
	h.spanalloc.free(unsafe.Pointer(s))
}

// allocSpan 申请 npages 页内存
// allocSpan allocates an mspan which owns npages worth of memory.
//
// If typ.manual() == false, allocSpan allocates a heap span of class spanclass
// and updates heap accounting. If manual == true, allocSpan allocates a
// manually-managed span (spanclass is ignored), and the caller is
// responsible for any accounting related to its use of the span. Either
// way, allocSpan will atomically add the bytes in the newly allocated
// span to *sysStat.
//
// The returned span is fully initialized.
//
// h.lock must not be held.
//
// allocSpan must be called on the system stack both because it acquires
// the heap lock and because it must block GC transitions.
//
//go:systemstack
func (h *mheap) allocSpan(npages uintptr, typ spanAllocType, spanclass spanClass) (s *mspan) {
	// Function-global state.
	gp := getg()
	base, scav := uintptr(0), uintptr(0)
	growth := uintptr(0)

	// false on linux
	// On some platforms we need to provide physical page aligned stack
	// allocations. Where the page size is less than the physical page
	// size, we already manage to do this by default.
	needPhysPageAlign := physPageAlignedStacks && typ == spanAllocStack && pageSize < physPageSize

	// 如果申请少量page，则尝试从p.pcache申请
	// If the allocation is small enough, try the page cache!
	// The page cache does not support aligned allocations, so we cannot use
	// it if we need to provide a physical page aligned stack allocation.
	pp := gp.m.p.ptr()
	// 获取少量page，npages < 16
	if !needPhysPageAlign && pp != nil && npages < pageCachePages/4 {
		// 不需要物理页对齐
		// 没有 p
		// npages 小于 16
		c := &pp.pcache

		// If the cache is empty, refill it.
		if c.empty() {
			// 填充cache
			lock(&h.lock)
			// 获取cache
			*c = h.pages.allocToCache()
			unlock(&h.lock)
		}

		// 获取 page
		// Try to allocate from the cache.
		base, scav = c.alloc(npages)
		if base != 0 {
			s = h.tryAllocMSpan()
			// 如果获取 mspan 成功
			if s != nil {
				goto HaveSpan
			}
			// We have a base but no mspan, so we need
			// to lock the heap.
		}
	}

	// For one reason or another, we couldn't get the
	// whole job done without the heap lock.
	lock(&h.lock)

	if needPhysPageAlign {
		// Overallocate by a physical page to allow for later alignment.
		extraPages := physPageSize / pageSize

		// Find a big enough region first, but then only allocate the
		// aligned portion. We can't just allocate and then free the
		// edges because we need to account for scavenged memory, and
		// that's difficult with alloc.
		//
		// Note that we skip updates to searchAddr here. It's OK if
		// it's stale and higher than normal; it'll operate correctly,
		// just come with a performance cost.
		base, _ = h.pages.find(npages + extraPages)
		if base == 0 {
			var ok bool
			growth, ok = h.grow(npages + extraPages)
			if !ok {
				unlock(&h.lock)
				return nil
			}
			base, _ = h.pages.find(npages + extraPages)
			if base == 0 {
				throw("grew heap, but no adequate free space found")
			}
		}
		base = alignUp(base, physPageSize)
		scav = h.pages.allocRange(base, npages)
	}

	// 如果 alloc 没有获取页
	if base == 0 {
		// Try to acquire a base address.
		base, scav = h.pages.alloc(npages)
		if base == 0 {
			var ok bool
			growth, ok = h.grow(npages)
			if !ok {
				unlock(&h.lock)
				return nil
			}
			base, scav = h.pages.alloc(npages)
			if base == 0 {
				throw("grew heap, but no adequate free space found")
			}
		}
	}
	if s == nil {
		// We failed to get an mspan earlier, so grab
		// one now that we have the heap lock.
		s = h.allocMSpanLocked()
	}
	unlock(&h.lock)

HaveSpan:
	// Decide if we need to scavenge in response to what we just allocated.
	// Specifically, we track the maximum amount of memory to scavenge of all
	// the alternatives below, assuming that the maximum satisfies *all*
	// conditions we check (e.g. if we need to scavenge X to satisfy the
	// memory limit and Y to satisfy heap-growth scavenging, and Y > X, then
	// it's fine to pick Y, because the memory limit is still satisfied).
	//
	// It's fine to do this after allocating because we expect any scavenged
	// pages not to get touched until we return. Simultaneously, it's important
	// to do this before calling sysUsed because that may commit address space.
	bytesToScavenge := uintptr(0)
	forceScavenge := false
	if limit := gcController.memoryLimit.Load(); !gcCPULimiter.limiting() {
		// Assist with scavenging to maintain the memory limit by the amount
		// that we expect to page in.
		inuse := gcController.mappedReady.Load()
		// Be careful about overflow, especially with uintptrs. Even on 32-bit platforms
		// someone can set a really big memory limit that isn't maxInt64.
		if uint64(scav)+inuse > uint64(limit) {
			bytesToScavenge = uintptr(uint64(scav) + inuse - uint64(limit))
			forceScavenge = true
		}
	}
	if goal := scavenge.gcPercentGoal.Load(); goal != ^uint64(0) && growth > 0 {
		// We just caused a heap growth, so scavenge down what will soon be used.
		// By scavenging inline we deal with the failure to allocate out of
		// memory fragments by scavenging the memory fragments that are least
		// likely to be re-used.
		//
		// Only bother with this because we're not using a memory limit. We don't
		// care about heap growths as long as we're under the memory limit, and the
		// previous check for scaving already handles that.
		if retained := heapRetained(); retained+uint64(growth) > goal {
			// The scavenging algorithm requires the heap lock to be dropped so it
			// can acquire it only sparingly. This is a potentially expensive operation
			// so it frees up other goroutines to allocate in the meanwhile. In fact,
			// they can make use of the growth we just created.
			todo := growth
			if overage := uintptr(retained + uint64(growth) - goal); todo > overage {
				todo = overage
			}
			if todo > bytesToScavenge {
				bytesToScavenge = todo
			}
		}
	}
	// There are a few very limited circumstances where we won't have a P here.
	// It's OK to simply skip scavenging in these cases. Something else will notice
	// and pick up the tab.
	var now int64
	if pp != nil && bytesToScavenge > 0 {
		// Measure how long we spent scavenging and add that measurement to the assist
		// time so we can track it for the GC CPU limiter.
		//
		// Limiter event tracking might be disabled if we end up here
		// while on a mark worker.
		start := nanotime()
		track := pp.limiterEvent.start(limiterEventScavengeAssist, start)

		// Scavenge, but back out if the limiter turns on.
		released := h.pages.scavenge(bytesToScavenge, func() bool {
			return gcCPULimiter.limiting()
		}, forceScavenge)

		mheap_.pages.scav.releasedEager.Add(released)

		// Finish up accounting.
		now = nanotime()
		if track {
			pp.limiterEvent.stop(limiterEventScavengeAssist, now)
		}
		scavenge.assistTime.Add(now - start)
	}

	// Initialize the span.
	h.initSpan(s, typ, spanclass, base, npages)

	// Commit and account for any scavenged memory that the span now owns.
	nbytes := npages * pageSize
	if scav != 0 {
		// sysUsed all the pages that are actually available
		// in the span since some of them might be scavenged.
		sysUsed(unsafe.Pointer(base), nbytes, scav)
		gcController.heapReleased.add(-int64(scav))
	}
	// Update stats.
	gcController.heapFree.add(-int64(nbytes - scav))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.committed, int64(scav))
	atomic.Xaddint64(&stats.released, -int64(scav))
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, int64(nbytes))
	}
	memstats.heapStats.release()

	pageTraceAlloc(pp, now, base, npages)
	return s
}

// initSpan initializes a blank span s which will represent the range
// [base, base+npages*pageSize). typ is the type of span being allocated.
func (h *mheap) initSpan(s *mspan, typ spanAllocType, spanclass spanClass, base, npages uintptr) {
	// At this point, both s != nil and base != 0, and the heap
	// lock is no longer held. Initialize the span.
	s.init(base, npages)
	if h.allocNeedsZero(base, npages) {
		s.needzero = 1
	}
	nbytes := npages * pageSize
	if typ.manual() {
		s.manualFreeList = 0
		s.nelems = 0
		s.limit = s.base() + s.npages*pageSize
		s.state.set(mSpanManual)
	} else {
		// We must set span properties before the span is published anywhere
		// since we're not holding the heap lock.
		s.spanclass = spanclass
		if sizeclass := spanclass.sizeclass(); sizeclass == 0 {
			s.elemsize = nbytes
			s.nelems = 1
			s.divMul = 0
		} else {
			s.elemsize = uintptr(class_to_size[sizeclass])
			if goexperiment.AllocHeaders && !s.spanclass.noscan() && heapBitsInSpan(s.elemsize) {
				// In the allocheaders experiment, reserve space for the pointer/scan bitmap at the end.
				s.nelems = uint16((nbytes - (nbytes / goarch.PtrSize / 8)) / s.elemsize)
			} else {
				s.nelems = uint16(nbytes / s.elemsize)
			}
			s.divMul = class_to_divmagic[sizeclass]
		}

		// Initialize mark and allocation structures.
		s.freeindex = 0
		s.freeIndexForScan = 0
		s.allocCache = ^uint64(0) // all 1s indicating all free.
		s.gcmarkBits = newMarkBits(uintptr(s.nelems))
		s.allocBits = newAllocBits(uintptr(s.nelems))

		// It's safe to access h.sweepgen without the heap lock because it's
		// only ever updated with the world stopped and we run on the
		// systemstack which blocks a STW transition.
		atomic.Store(&s.sweepgen, h.sweepgen)

		// Now that the span is filled in, set its state. This
		// is a publication barrier for the other fields in
		// the span. While valid pointers into this span
		// should never be visible until the span is returned,
		// if the garbage collector finds an invalid pointer,
		// access to the span may race with initialization of
		// the span. We resolve this race by atomically
		// setting the state after the span is fully
		// initialized, and atomically checking the state in
		// any situation where a pointer is suspect.
		s.state.set(mSpanInUse)
	}

	// Publish the span in various locations.

	// This is safe to call without the lock held because the slots
	// related to this span will only ever be read or modified by
	// this thread until pointers into the span are published (and
	// we execute a publication barrier at the end of this function
	// before that happens) or pageInUse is updated.
	h.setSpans(s.base(), npages, s)

	if !typ.manual() {
		// Mark in-use span in arena page bitmap.
		//
		// This publishes the span to the page sweeper, so
		// it's imperative that the span be completely initialized
		// prior to this line.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.Or8(&arena.pageInUse[pageIdx], pageMask)

		// Update related page sweeper stats.
		h.pagesInUse.Add(npages)
	}

	// Make sure the newly allocated span will be observed
	// by the GC before pointers into the span are published.
	publicationBarrier()
}

// grow 尝试向操作系统索要npage页内存，返回是否成功
// Try to add at least npage pages of memory to the heap,
// returning how much the heap grew by and whether it worked.
//
// h.lock must be held.
func (h *mheap) grow(npage uintptr) (uintptr, bool) {
	assertLockHeld(&h.lock)

	// We must grow the heap in whole palloc chunks.
	// We call sysMap below but note that because we
	// round up to pallocChunkPages which is on the order
	// of MiB (generally >= to the huge page size) we
	// won't be calling it too much.
	// 向上取整需要的内存量
	ask := alignUp(npage, pallocChunkPages) * pageSize

	totalGrowth := uintptr(0)
	// This may overflow because ask could be very large
	// and is otherwise unrelated to h.curArena.base.
	// 当前基址加上需要的得到 end 可能溢出
	end := h.curArena.base + ask
	// 向上取整物理页大小
	nBase := alignUp(end, physPageSize)
	if nBase > h.curArena.end || /* overflow */ end < h.curArena.base {
		// 检测溢出后
		// Not enough room in the current arena. Allocate more
		// arena space. This may not be contiguous with the
		// current arena, so we have to request the full ask.
		av, asize := h.sysAlloc(ask, &h.arenaHints, true)
		if av == nil {
			inUse := gcController.heapFree.load() + gcController.heapReleased.load() + gcController.heapInUse.load()
			print("runtime: out of memory: cannot allocate ", ask, "-byte block (", inUse, " in use)\n")
			return 0, false
		}

		if uintptr(av) == h.curArena.end {
			// The new space is contiguous with the old
			// space, so just extend the current space.
			h.curArena.end = uintptr(av) + asize
		} else {
			// The new space is discontiguous. Track what
			// remains of the current space and switch to
			// the new space. This should be rare.
			if size := h.curArena.end - h.curArena.base; size != 0 {
				// Transition this space from Reserved to Prepared and mark it
				// as released since we'll be able to start using it after updating
				// the page allocator and releasing the lock at any time.
				sysMap(unsafe.Pointer(h.curArena.base), size, &gcController.heapReleased)
				// Update stats.
				stats := memstats.heapStats.acquire()
				atomic.Xaddint64(&stats.released, int64(size))
				memstats.heapStats.release()
				// Update the page allocator's structures to make this
				// space ready for allocation.
				h.pages.grow(h.curArena.base, size)
				totalGrowth += size
			}
			// Switch to the new space.
			h.curArena.base = uintptr(av)
			h.curArena.end = uintptr(av) + asize
		}

		// Recalculate nBase.
		// We know this won't overflow, because sysAlloc returned
		// a valid region starting at h.curArena.base which is at
		// least ask bytes in size.
		nBase = alignUp(h.curArena.base+ask, physPageSize)
	}

	// Grow into the current arena.
	v := h.curArena.base
	h.curArena.base = nBase

	// Transition the space we're going to use from Reserved to Prepared.
	//
	// The allocation is always aligned to the heap arena
	// size which is always > physPageSize, so its safe to
	// just add directly to heapReleased.
	sysMap(unsafe.Pointer(v), nBase-v, &gcController.heapReleased)

	// The memory just allocated counts as both released
	// and idle, even though it's not yet backed by spans.
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.released, int64(nBase-v))
	memstats.heapStats.release()

	// Update the page allocator's structures to make this
	// space ready for allocation.
	h.pages.grow(v, nBase-v)
	totalGrowth += nBase - v
	return totalGrowth, true
}

// Free the span back into the heap.
func (h *mheap) freeSpan(s *mspan) {
	systemstack(func() {
		pageTraceFree(getg().m.p.ptr(), 0, s.base(), s.npages)

		lock(&h.lock)
		if msanenabled {
			// Tell msan that this entire span is no longer in use.
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			msanfree(base, bytes)
		}
		if asanenabled {
			// Tell asan that this entire span is no longer in use.
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			asanpoison(base, bytes)
		}
		h.freeSpanLocked(s, spanAllocHeap)
		unlock(&h.lock)
	})
}

// freeManual 释放由 allocManual 申请手动管理的 mspan
// freeManual frees a manually-managed span returned by allocManual.
// typ must be the same as the spanAllocType passed to the allocManual that
// allocated s.
//
// This must only be called when gcphase == _GCoff. See mSpanState for
// an explanation.
//
// freeManual must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack
func (h *mheap) freeManual(s *mspan, typ spanAllocType) {
	pageTraceFree(getg().m.p.ptr(), 0, s.base(), s.npages)

	s.needzero = 1
	lock(&h.lock)
	h.freeSpanLocked(s, typ)
	unlock(&h.lock)
}

func (h *mheap) freeSpanLocked(s *mspan, typ spanAllocType) {
	assertLockHeld(&h.lock)

	switch s.state.get() {
	case mSpanManual:
		if s.allocCount != 0 {
			throw("mheap.freeSpanLocked - invalid stack free")
		}
	case mSpanInUse:
		if s.isUserArenaChunk {
			throw("mheap.freeSpanLocked - invalid free of user arena chunk")
		}
		if s.allocCount != 0 || s.sweepgen != h.sweepgen {
			print("mheap.freeSpanLocked - span ", s, " ptr ", hex(s.base()), " allocCount ", s.allocCount, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("mheap.freeSpanLocked - invalid free")
		}
		h.pagesInUse.Add(-s.npages)

		// Clear in-use bit in arena page bitmap.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	default:
		throw("mheap.freeSpanLocked - invalid span state")
	}

	// Update stats.
	//
	// Mirrors the code in allocSpan.
	nbytes := s.npages * pageSize
	gcController.heapFree.add(int64(nbytes))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(-int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, -int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, -int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, -int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, -int64(nbytes))
	}
	memstats.heapStats.release()

	// Mark the space as free.
	h.pages.free(s.base(), s.npages)

	// Free the span structure. We no longer have a use for it.
	s.state.set(mSpanDead)
	h.freeMSpanLocked(s)
}

// scavengeAll acquires the heap lock (blocking any additional
// manipulation of the page allocator) and iterates over the whole
// heap, scavenging every free page available.
//
// Must run on the system stack because it acquires the heap lock.
//
//go:systemstack
func (h *mheap) scavengeAll() {
	// Disallow malloc or panic while holding the heap lock. We do
	// this here because this is a non-mallocgc entry-point to
	// the mheap API.
	gp := getg()
	gp.m.mallocing++

	// Force scavenge everything.
	released := h.pages.scavenge(^uintptr(0), nil, true)

	gp.m.mallocing--

	if debug.scavtrace > 0 {
		printScavTrace(0, released, true)
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	GC()
	systemstack(func() { mheap_.scavengeAll() })
}

// Initialize a new span with the given start and npages.
func (span *mspan) init(base uintptr, npages uintptr) {
	// span is *not* zeroed.
	span.next = nil
	span.prev = nil
	span.list = nil
	span.startAddr = base
	span.npages = npages
	span.allocCount = 0
	span.spanclass = 0
	span.elemsize = 0
	span.speciallock.key = 0
	span.specials = nil
	span.needzero = 0
	span.freeindex = 0
	span.freeIndexForScan = 0
	span.allocBits = nil
	span.gcmarkBits = nil
	span.pinnerBits = nil
	span.state.set(mSpanDead)
	lockInit(&span.speciallock, lockRankMspanSpecial)
}

func (span *mspan) inList() bool {
	return span.list != nil
}

// Initialize an empty doubly-linked list.
func (list *mSpanList) init() {
	list.first = nil
	list.last = nil
}

// remove 从 list 中移除 span
func (list *mSpanList) remove(span *mspan) {
	if span.list != list {
		print("runtime: failed mSpanList.remove span.npages=", span.npages,
			" span=", span, " prev=", span.prev, " span.list=", span.list, " list=", list, "\n")
		throw("mSpanList.remove")
	}
	// 从链表中摘除 span
	if list.first == span {
		list.first = span.next
	} else {
		span.prev.next = span.next
	}
	if list.last == span {
		list.last = span.prev
	} else {
		span.next.prev = span.prev
	}
	span.next = nil
	span.prev = nil
	span.list = nil
}

// isEmpty 链表是否为空
func (list *mSpanList) isEmpty() bool {
	return list.first == nil
}

// insert 头插一个 span
func (list *mSpanList) insert(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insert", span, span.next, span.prev, span.list)
		throw("mSpanList.insert")
	}
	span.next = list.first
	if list.first != nil {
		// 链表不为空 直接头插
		// 保证链表尾不能修改
		// The list contains at least one span; link it in.
		// The last span in the list doesn't change.
		list.first.prev = span
	} else {
		// 链表为空 直接替换
		// The list contains no spans, so this is also the last span.
		list.last = span
	}
	list.first = span
	span.list = list
}

func (list *mSpanList) insertBack(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insertBack", span, span.next, span.prev, span.list)
		throw("mSpanList.insertBack")
	}
	span.prev = list.last
	if list.last != nil {
		// The list contains at least one span.
		list.last.next = span
	} else {
		// The list contains no spans, so this is also the first span.
		list.first = span
	}
	list.last = span
	span.list = list
}

// takeAll 将 other 中的所有 span 头插到 list 上
// takeAll removes all spans from other and inserts them at the front
// of list.
func (list *mSpanList) takeAll(other *mSpanList) {
	if other.isEmpty() {
		return
	}

	// Reparent everything in other to list.
	for s := other.first; s != nil; s = s.next {
		s.list = list
	}

	// Concatenate the lists.
	if list.isEmpty() {
		*list = *other
	} else {
		// Neither list is empty. Put other before list.
		other.last.next = list.first
		list.first.prev = other.last
		list.first = other.first
	}

	other.first, other.last = nil, nil
}

const (
	_KindSpecialFinalizer = 1
	_KindSpecialProfile   = 2
	// _KindSpecialReachable is a special used for tracking
	// reachability during testing.
	_KindSpecialReachable = 3
	// _KindSpecialPinCounter is a special used for objects that are pinned
	// multiple times
	_KindSpecialPinCounter = 4
	// Note: The finalizer special must be first because if we're freeing
	// an object, a finalizer special will cause the freeing operation
	// to abort, and we want to keep the other special records around
	// if that happens.
)

// special 链表节点
// 表示对象对 span 的偏移量和 special 的种类
type special struct {
	_      sys.NotInHeap
	next   *special // linked list in span
	offset uint16   // span offset of object
	kind   byte     // kind of special
}

// spanHasSpecials marks a span as having specials in the arena bitmap.
func spanHasSpecials(s *mspan) {
	arenaPage := (s.base() / pageSize) % pagesPerArena
	ai := arenaIndex(s.base())
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.Or8(&ha.pageSpecials[arenaPage/8], uint8(1)<<(arenaPage%8))
}

// spanHasNoSpecials marks a span as having no specials in the arena bitmap.
func spanHasNoSpecials(s *mspan) {
	arenaPage := (s.base() / pageSize) % pagesPerArena
	ai := arenaIndex(s.base())
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.And8(&ha.pageSpecials[arenaPage/8], ^(uint8(1) << (arenaPage % 8)))
}

// Adds the special record s to the list of special records for
// the object p. All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
// already exists.)
func addspecial(p unsafe.Pointer, s *special) bool {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()
	kind := s.kind

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	iter, exists := span.specialFindSplicePoint(offset, kind)
	if !exists {
		// Splice in record, fill in offset.
		s.offset = uint16(offset)
		s.next = *iter
		*iter = s
		spanHasSpecials(span)
	}

	unlock(&span.speciallock)
	releasem(mp)
	return !exists // already exists
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
func removespecial(p unsafe.Pointer, kind uint8) *special {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()

	var result *special
	lock(&span.speciallock)

	iter, exists := span.specialFindSplicePoint(offset, kind)
	if exists {
		s := *iter
		*iter = s.next
		result = s
	}
	if span.specials == nil {
		spanHasNoSpecials(span)
	}
	unlock(&span.speciallock)
	releasem(mp)
	return result
}

// Find a splice point in the sorted list and check for an already existing
// record. Returns a pointer to the next-reference in the list predecessor.
// Returns true, if the referenced item is an exact match.
func (span *mspan) specialFindSplicePoint(offset uintptr, kind byte) (**special, bool) {
	// Find splice point, check for existing record.
	iter := &span.specials
	found := false
	for {
		s := *iter
		if s == nil {
			break
		}
		if offset == uintptr(s.offset) && kind == s.kind {
			found = true
			break
		}
		if offset < uintptr(s.offset) || (offset == uintptr(s.offset) && kind < s.kind) {
			break
		}
		iter = &s.next
	}
	return iter, found
}

// The described object has a finalizer set for it.
//
// specialfinalizer is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.
type specialfinalizer struct {
	_       sys.NotInHeap
	special special
	fn      *funcval // May be a heap pointer.
	nret    uintptr
	fint    *_type   // May be a heap pointer, but always live.
	ot      *ptrtype // May be a heap pointer, but always live.
}

// Adds a finalizer to the object p. Returns true if it succeeded.
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		if gcphase != _GCoff {
			base, span, _ := findObject(uintptr(p), 0, 0)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			if !span.spanclass.noscan() {
				scanobject(base, gcw)
			}
			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			scanblock(uintptr(unsafe.Pointer(&s.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
func removefinalizer(p unsafe.Pointer) {
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
	}
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object is being heap profiled.
type specialprofile struct {
	_       sys.NotInHeap
	special special
	b       *bucket
}

// Set the heap profile bucket associated with addr to b.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialProfile
	s.b = b
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// specialReachable tracks whether an object is reachable on the next
// GC cycle. This is used by testing.
type specialReachable struct {
	special   special
	done      bool
	reachable bool
}

// specialPinCounter tracks whether an object is pinned multiple times.
type specialPinCounter struct {
	special special
	counter uintptr
}

// specialsIter helps iterate over specials lists.
type specialsIter struct {
	pprev **special
	s     *special
}

// newSpecialsIter 返回 span 中的 specials 迭代器
func newSpecialsIter(span *mspan) specialsIter {
	return specialsIter{&span.specials, span.specials}
}

func (i *specialsIter) valid() bool {
	return i.s != nil
}

func (i *specialsIter) next() {
	i.pprev = &i.s.next
	i.s = *i.pprev
}

// unlinkAndNext removes the current special from the list and moves
// the iterator to the next special. It returns the unlinked special.
func (i *specialsIter) unlinkAndNext() *special {
	cur := i.s
	i.s = cur.next
	*i.pprev = i.s
	return cur
}

// freeSpecial performs any cleanup on special s and deallocates it.
// s must already be unlinked from the specials list.
func freeSpecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		sp := (*specialprofile)(unsafe.Pointer(s))
		mProf_Free(sp.b, size)
		lock(&mheap_.speciallock)
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	case _KindSpecialReachable:
		sp := (*specialReachable)(unsafe.Pointer(s))
		sp.done = true
		// The creator frees these.
	case _KindSpecialPinCounter:
		lock(&mheap_.speciallock)
		mheap_.specialPinCounterAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	default:
		throw("bad special kind")
		panic("not reached")
	}
}

// gcBits 用于标记的位图
// gcBits is an alloc/mark bitmap. This is always used as gcBits.x.
type gcBits struct {
	_ sys.NotInHeap
	x uint8
}

// bytep 返回第 n 个字节的数据指针
// bytep returns a pointer to the n'th byte of b.
func (b *gcBits) bytep(n uintptr) *uint8 {
	return addb(&b.x, n)
}

// bitp 返回第 n 个比特位的地址和掩码
// bitp returns a pointer to the byte containing bit n and a mask for
// selecting that bit from *bytep.
func (b *gcBits) bitp(n uintptr) (bytep *uint8, mask uint8) {
	return b.bytep(n / 8), 1 << (n % 8)
}

const gcBitsChunkBytes = uintptr(64 << 10)
const gcBitsHeaderBytes = unsafe.Sizeof(gcBitsHeader{})

type gcBitsHeader struct {
	free uintptr // free is the index into bits of the next free byte.
	next uintptr // *gcBits triggers recursive type bug. (issue 14620)
}

type gcBitsArena struct {
	_ sys.NotInHeap
	// gcBitsHeader // side step recursive type bug (issue 14620) by including fields by hand.
	free uintptr // free is the index into bits of the next free byte; read/write atomically
	next *gcBitsArena
	bits [gcBitsChunkBytes - gcBitsHeaderBytes]gcBits
}

var gcBitsArenas struct {
	lock     mutex
	free     *gcBitsArena
	next     *gcBitsArena // Read atomically. Write atomically under lock.
	current  *gcBitsArena
	previous *gcBitsArena
}

// tryAlloc allocates from b or returns nil if b does not have enough room.
// This is safe to call concurrently.
func (b *gcBitsArena) tryAlloc(bytes uintptr) *gcBits {
	if b == nil || atomic.Loaduintptr(&b.free)+bytes > uintptr(len(b.bits)) {
		return nil
	}
	// Try to allocate from this block.
	end := atomic.Xadduintptr(&b.free, bytes)
	if end > uintptr(len(b.bits)) {
		return nil
	}
	// There was enough room.
	start := end - bytes
	return &b.bits[start]
}

// newMarkBits returns a pointer to 8 byte aligned bytes
// to be used for a span's mark bits.
func newMarkBits(nelems uintptr) *gcBits {
	blocksNeeded := (nelems + 63) / 64
	bytesNeeded := blocksNeeded * 8

	// Try directly allocating from the current head arena.
	head := (*gcBitsArena)(atomic.Loadp(unsafe.Pointer(&gcBitsArenas.next)))
	if p := head.tryAlloc(bytesNeeded); p != nil {
		return p
	}

	// There's not enough room in the head arena. We may need to
	// allocate a new arena.
	lock(&gcBitsArenas.lock)
	// Try the head arena again, since it may have changed. Now
	// that we hold the lock, the list head can't change, but its
	// free position still can.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate a new arena. This may temporarily drop the lock.
	fresh := newArenaMayUnlock()
	// If newArenaMayUnlock dropped the lock, another thread may
	// have put a fresh arena on the "next" list. Try allocating
	// from next again.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		// Put fresh back on the free list.
		// TODO: Mark it "already zeroed"
		fresh.next = gcBitsArenas.free
		gcBitsArenas.free = fresh
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate from the fresh arena. We haven't linked it in yet, so
	// this cannot race and is guaranteed to succeed.
	p := fresh.tryAlloc(bytesNeeded)
	if p == nil {
		throw("markBits overflow")
	}

	// Add the fresh arena to the "next" list.
	fresh.next = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), unsafe.Pointer(fresh))

	unlock(&gcBitsArenas.lock)
	return p
}

// newAllocBits returns a pointer to 8 byte aligned bytes
// to be used for this span's alloc bits.
// newAllocBits is used to provide newly initialized spans
// allocation bits. For spans not being initialized the
// mark bits are repurposed as allocation bits when
// the span is swept.
func newAllocBits(nelems uintptr) *gcBits {
	return newMarkBits(nelems)
}

// nextMarkBitArenaEpoch establishes a new epoch for the arenas
// holding the mark bits. The arenas are named relative to the
// current GC cycle which is demarcated by the call to finishweep_m.
//
// All current spans have been swept.
// During that sweep each span allocated room for its gcmarkBits in
// gcBitsArenas.next block. gcBitsArenas.next becomes the gcBitsArenas.current
// where the GC will mark objects and after each span is swept these bits
// will be used to allocate objects.
// gcBitsArenas.current becomes gcBitsArenas.previous where the span's
// gcAllocBits live until all the spans have been swept during this GC cycle.
// The span's sweep extinguishes all the references to gcBitsArenas.previous
// by pointing gcAllocBits into the gcBitsArenas.current.
// The gcBitsArenas.previous is released to the gcBitsArenas.free list.
func nextMarkBitArenaEpoch() {
	lock(&gcBitsArenas.lock)
	if gcBitsArenas.previous != nil {
		if gcBitsArenas.free == nil {
			gcBitsArenas.free = gcBitsArenas.previous
		} else {
			// Find end of previous arenas.
			last := gcBitsArenas.previous
			for last = gcBitsArenas.previous; last.next != nil; last = last.next {
			}
			last.next = gcBitsArenas.free
			gcBitsArenas.free = gcBitsArenas.previous
		}
	}
	gcBitsArenas.previous = gcBitsArenas.current
	gcBitsArenas.current = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), nil) // newMarkBits calls newArena when needed
	unlock(&gcBitsArenas.lock)
}

// newArenaMayUnlock allocates and zeroes a gcBits arena.
// The caller must hold gcBitsArena.lock. This may temporarily release it.
func newArenaMayUnlock() *gcBitsArena {
	var result *gcBitsArena
	if gcBitsArenas.free == nil {
		unlock(&gcBitsArenas.lock)
		result = (*gcBitsArena)(sysAlloc(gcBitsChunkBytes, &memstats.gcMiscSys))
		if result == nil {
			throw("runtime: cannot allocate memory")
		}
		lock(&gcBitsArenas.lock)
	} else {
		result = gcBitsArenas.free
		gcBitsArenas.free = gcBitsArenas.free.next
		memclrNoHeapPointers(unsafe.Pointer(result), gcBitsChunkBytes)
	}
	result.next = nil
	// If result.bits is not 8 byte aligned adjust index so
	// that &result.bits[result.free] is 8 byte aligned.
	if unsafe.Offsetof(gcBitsArena{}.bits)&7 == 0 {
		result.free = 0
	} else {
		result.free = 8 - (uintptr(unsafe.Pointer(&result.bits[0])) & 7)
	}
	return result
}
