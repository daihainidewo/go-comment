// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Put(x) “synchronizes before”
// a call to Get returning that same value x.
// Similarly, a call to New returning x “synchronizes before”
// a call to Get returning that same value x.
type Pool struct {
	// copy标志位
	noCopy noCopy

	// 本地队列指针，索引为P的ID
	// 无锁化 即获取当期 p 的资源
	// 如果需要偷取其他 p 的资源 则仍需要加锁
	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	localSize uintptr        // size of the local array

	// 轮回的队列指针
	// 用于 GC 清理脏数据
	// 对象太多 GC 频次变高 GC 时间变长
	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// 新建对象方法
	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	New func() any
}

// poolLocalInternal 当前P的池子
// Pool local 和 victim 指向节点 单链表
// Local per-P Pool appendix.
type poolLocalInternal struct {
	private any       // Can be used only by the respective P.
	shared  poolChain // Local P can pushHead/popHead; any P can popTail.
}

// poolLocal cache line 填充
type poolLocal struct {
	poolLocalInternal

	// 对齐 cache line
	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrandn(n uint32) uint32

var poolRaceHash [128]uint64

// poolRaceAddr 用于竞态检测
// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x any) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
func (p *Pool) Put(x any) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrandn(4) == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	// 获取 poolLocal
	l, _ := p.pin()
	if l.private == nil {
		// 优先置为私有
		// 没有设置私有，则将当前的设置为私有
		l.private = x
	} else {
		// 如果有私有则将其存放到share的头部
		l.shared.pushHead(x)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
func (p *Pool) Get() any {
	if race.Enabled {
		race.Disable()
	}
	// 获取 poolLocal 和 当前 pid
	l, pid := p.pin()
	x := l.private
	l.private = nil
	if x == nil {
		// 没有私有的则取共享的拿去头部
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		x, _ = l.shared.popHead()
		if x == nil {
			// poolLocal 没有就去偷其他的
			x = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil {
		// 都没有就新建一个
		x = p.New()
	}
	return x
}

// 从其他 pid 偷取 或 从 victim 中偷取
func (p *Pool) getSlow(pid int) any {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	for i := 0; i < int(size); i++ {
		// 先尝试偷取其他 pid 的 队尾元素
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			// 从其他共享链表中弹出元素，成功则直接返回
			return x
		}
	}

	// 没能从其他P偷到
	// 从victim中获取，写在从primary中偷取后面是希望victim中的元素能够尽快老去
	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		// 当前的 pid 大于 victim 的 size 直接返回
		// 防止 panic
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		// 获取victim中的private，有就直接返回
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ {
		// 偷其他 pid 的元素
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			// 从victim链表中弹出元素
			return x
		}
	}

	// 走到这表示没有 victim 有可能被并发偷完了
	// 如果都没有表示当前victim为空，则将victim的size置为0
	// Mark the victim cache as empty for future gets don't bother
	// with it.
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin 返回当前P的本地队列对象和当前P的ID
// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
func (p *Pool) pin() (*poolLocal, int) {
	// Check whether p is nil to get a panic.
	// Otherwise the nil dereference happens while the m is pinned,
	// causing a fatal error rather than a panic.
	if p == nil {
		panic("nil Pool")
	}

	// 获取当前P的ID
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	if uintptr(pid) < s {
		// 队列拥有缓存的数据
		// 当前队列长度 s 大于当前执行的 pid
		// 直接返回当前 pid 指向的 poolLocal 的地址
		return indexLocal(l, pid), pid
	}
	// 当前队列没有 pid 的 poolLocal
	// 则创建
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	// 获取锁可能占用长时间
	// 所以先解除禁止抢占
	runtime_procUnpin()
	// 获取全局锁
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 得到锁后 标记禁止抢占
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		// 如果已经存在直接返回
		return indexLocal(l, pid), pid
	}
	// p.local 为空表示新建的pool，存放到全局pool列表中
	if p.local == nil {
		// 新建的 p 则将其加入 all 中
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// 创建当前P个大小的队列初始化本地缓存
	// 重置 p 的 local
	size := runtime.GOMAXPROCS(0)
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// 该函数调用与STW期间，在垃圾回收开始时，不能调用申请和运行时函数
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// 删除所有victim的缓存
	// Drop victim caches from all pools.
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// 将primary移到victim
	// Move primary cache to victim cache.
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// 将当前的池对象置为旧的池对象，新池对象置空
	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil
}

var (
	// 全局pool锁
	allPoolsMu Mutex

	// 所有pool的集合，非空主要缓存，由 allPoolsMu 和 pin 或者 STW 保护
	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	allPools []*Pool

	// 旧缓存，由 STW 保护
	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	// 注册清理缓冲池中的对象 由 GC 开始时触发
	runtime_registerPoolCleanup(poolCleanup)
}

// indexLocal 通过指针计算返回poolLocal的地址
// 计算第i个 poolLocal 的地址
func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func()) // 将cleanup设置到GC前执行
func runtime_procPin() int                       // 标记禁止抢占，返回P的ID
func runtime_procUnpin()                         // 解除禁止抢占

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

// 获取ptr的值
//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

// 向ptr存储值val
//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
