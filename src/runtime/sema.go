// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
//
// See Mullender and Cox, ``Semaphores in Plan 9,''
// https://swtch.com/semaphore.pdf

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

// Asynchronous semaphore for sync.Mutex.

// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
// See golang.org/issue/17953 for a program that worked badly
// before we introduced the second level of list, and
// BenchmarkSemTable/OneAddrCollision/* for a benchmark that exercises this.
type semaRoot struct {
	// lock 锁
	// treap 树堆 存等待的g tree-heap 小根堆
	// nwait 等待者的个数
	lock  mutex
	treap *sudog        // root of balanced tree of unique waiters.
	nwait atomic.Uint32 // Number of waiters. Read w/o the lock.
}

// 信号表
var semtable semTable

// Prime to not correlate with any user patterns.
// 为啥是251
const semTabSize = 251

// 信号表 固定大小
type semTable [semTabSize]struct {
	root semaRoot
	pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}

// 获取地址所属根结点
func (t *semTable) rootFor(addr *uint32) *semaRoot {
	return &t[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
	semrelease1(addr, handoff, skipframes)
}

//go:linkname sync_runtime_SemacquireMutex sync.runtime_SemacquireMutex
func sync_runtime_SemacquireMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncMutexLock)
}

//go:linkname sync_runtime_SemacquireRWMutexR sync.runtime_SemacquireRWMutexR
func sync_runtime_SemacquireRWMutexR(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexRLock)
}

//go:linkname sync_runtime_SemacquireRWMutex sync.runtime_SemacquireRWMutex
func sync_runtime_SemacquireRWMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexLock)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
	semrelease(addr)
}

// 唤醒sudog
func readyWithTime(s *sudog, traceskip int) {
	if s.releasetime != 0 {
		// 设置唤醒时间
		s.releasetime = cputicks()
	}
	goready(s.g, traceskip)
}

// 信号阻塞标志
type semaProfileFlags int

const (
	semaBlockProfile semaProfileFlags = 1 << iota
	semaMutexProfile
)

// Called from runtime.
func semacquire(addr *uint32) {
	semacquire1(addr, false, 0, 0, waitReasonSemacquire)
}

// 信号量处理 等待被唤醒
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
	gp := getg()
	if gp != gp.m.curg {
		throw("semacquire not on the G stack")
	}

	// Easy case.
	if cansemacquire(addr) {
		// 如果有正在执行 semrelease1 直接获取信号量 返回
		return
	}

	// Harder case:
	//	increment waiter count
	//	try cansemacquire one more time, return if succeeded
	//	enqueue itself as a waiter
	//	sleep
	//	(waiter descriptor is dequeued by signaler)
	s := acquireSudog()
	root := semtable.rootFor(addr)
	t0 := int64(0)
	s.releasetime = 0
	s.acquiretime = 0
	s.ticket = 0
	if profile&semaBlockProfile != 0 && blockprofilerate > 0 {
		// 阻塞事件
		t0 = cputicks()
		// 阻塞没有释放时间
		s.releasetime = -1
	}
	if profile&semaMutexProfile != 0 && mutexprofilerate > 0 {
		// 锁事件
		if t0 == 0 {
			t0 = cputicks()
		}
		// 记录获取时间
		s.acquiretime = t0
	}
	for {
		// 锁住
		lockWithRank(&root.lock, lockRankRoot)
		// Add ourselves to nwait to disable "easy case" in semrelease.
		// 添加root的等待者
		root.nwait.Add(1)
		// Check cansemacquire to avoid missed wakeup.
		if cansemacquire(addr) {
			// 再次检测是否有正在释放的信号量
			root.nwait.Add(-1)
			unlock(&root.lock)
			break
		}
		// 入队 休眠
		// Any semrelease after the cansemacquire knows we're waiting
		// (we set nwait above), so go to sleep.
		root.queue(addr, s, lifo)
		goparkunlock(&root.lock, reason, traceBlockSync, 4+skipframes)
		if s.ticket != 0 || cansemacquire(addr) {
			// 已经获取过 或 addr 还可以获取 sema
			break
		}
	}
	// 被唤醒后是否记录阻塞事件
	if s.releasetime > 0 {
		// 尝试记录阻塞事件
		blockevent(s.releasetime-t0, 3+skipframes)
	}
	releaseSudog(s)
}

func semrelease(addr *uint32) {
	semrelease1(addr, false, 0)
}

// 解锁单个 addr 的等待 g 并唤醒该 g
func semrelease1(addr *uint32, handoff bool, skipframes int) {
	// 取根结点
	root := semtable.rootFor(addr)
	// 增加信号量
	atomic.Xadd(addr, 1)

	// 检测操作必须在 xadd 后面
	// 避免错过唤醒
	// 这边加上 那边获取
	// Easy case: no waiters?
	// This check must happen after the xadd, to avoid a missed wakeup
	// (see loop in semacquire).
	if root.nwait.Load() == 0 {
		// 没有等待的 g 直接返回
		return
	}

	// Harder case: search for a waiter and wake it.
	lockWithRank(&root.lock, lockRankRoot)
	if root.nwait.Load() == 0 {
		// 二次检测
		// The count is already consumed by another goroutine,
		// so no need to wake up another goroutine.
		unlock(&root.lock)
		return
	}
	s, t0, tailtime := root.dequeue(addr)
	if s != nil {
		// 弹出有效
		root.nwait.Add(-1)
	}
	unlock(&root.lock)
	if s != nil { // May be slow or even yield, so unlock first
		acquiretime := s.acquiretime
		if acquiretime != 0 {
			// Charge contention that this (delayed) unlock caused.
			// If there are N more goroutines waiting beyond the
			// one that's waking up, charge their delay as well, so that
			// contention holding up many goroutines shows up as
			// more costly than contention holding up a single goroutine.
			// It would take O(N) time to calculate how long each goroutine
			// has been waiting, so instead we charge avg(head-wait, tail-wait)*N.
			// head-wait is the longest wait and tail-wait is the shortest.
			// (When we do a lifo insertion, we preserve this property by
			// copying the old head's acquiretime into the inserted new head.
			// In that case the overall average may be slightly high, but that's fine:
			// the average of the ends is only an approximation to the actual
			// average anyway.)
			// The root.dequeue above changed the head and tail acquiretime
			// to the current time, so the next unlock will not re-count this contention.
			dt0 := t0 - acquiretime
			dt := dt0
			if s.waiters != 0 {
				dtail := t0 - tailtime
				dt += (dtail + dt0) / 2 * int64(s.waiters)
			}
			mutexevent(dt, 3+skipframes)
		}
		if s.ticket != 0 {
			throw("corrupted semaphore ticket")
		}
		if handoff && cansemacquire(addr) {
			// 只有外部需要让出调度时才会获取信号量
			// 标记下次执行 并且 addr还可以被获取
			s.ticket = 1
		}
		// 唤醒s 加入就绪队列
		readyWithTime(s, 5+skipframes)
		if s.ticket == 1 && getg().m.locks == 0 {
			// 标记为1 并且 m 没有被其他 g 锁住
			// 让出 m 等待下次调度
			// 即直接调度 s 对应的 g
			// 请注意，继承了我们的时间片：这是可取的，以避免一个高度竞争的信号量无限期地占用 P
			// goyield 类似于 Gosched，但它会发出一个“抢占式”跟踪事件
			// 更重要的是，将当前 G 放在本地 runq 而不是全局 runq 上
			// 我们只在饥饿状态下执行此操作（handoff=true）
			// 因为在非饥饿情况下，在我们让出调度时，其他服务员可能会获取信号量，这将是一种浪费
			// 相反，我们等待进入饥饿状态，然后我们开始直接切换票和 P
			// Direct G handoff
			// readyWithTime has added the waiter G as runnext in the
			// current P; we now call the scheduler so that we start running
			// the waiter G immediately.
			// Note that waiter inherits our time slice: this is desirable
			// to avoid having a highly contended semaphore hog the P
			// indefinitely. goyield is like Gosched, but it emits a
			// "preempted" trace event instead and, more importantly, puts
			// the current G on the local runq instead of the global one.
			// We only do this in the starving regime (handoff=true), as in
			// the non-starving case it is possible for a different waiter
			// to acquire the semaphore while we are yielding/scheduling,
			// and this would be wasteful. We wait instead to enter starving
			// regime, and then we start to do direct handoffs of ticket and
			// P.
			// See issue 33747 for discussion.
			goyield()
		}
	}
}

// 检测锁是否可获取
func cansemacquire(addr *uint32) bool {
	for {
		v := atomic.Load(addr)
		if v == 0 {
			// 0 表示没有
			return false
		}
		if atomic.Cas(addr, v, v-1) {
			// 表示正在释放信号量
			return true
		}
	}
}

// 将当前的 s 入队
// 只有新插入的 addr 才会生成 ticket
// queue adds s to the blocked goroutines in semaRoot.
func (root *semaRoot) queue(addr *uint32, s *sudog, lifo bool) {
	s.g = getg()
	s.elem = unsafe.Pointer(addr)
	s.next = nil
	s.prev = nil
	s.waiters = 0

	// 先查找有没有相同锁地址 有就按规则插入后返回
	var last *sudog
	// var pt **sudog
	pt := &root.treap
	for t := *pt; t != nil; t = *pt {
		// 遍历所有节点 查找 addr 是否已经入 treap
		if t.elem == unsafe.Pointer(addr) {
			// Already have addr in list.
			if lifo {
				// 插入队首
				// Substitute s in t's place in treap.
				// 赋值pt
				*pt = s
				// 交接二叉树链接信息
				s.ticket = t.ticket
				s.acquiretime = t.acquiretime // preserve head acquiretime as oldest time
				s.parent = t.parent
				s.prev = t.prev
				s.next = t.next
				if s.prev != nil {
					s.prev.parent = s
				}
				if s.next != nil {
					s.next.parent = s
				}
				// 修正队首尾
				// Add t first in s's wait list.
				s.waitlink = t
				s.waittail = t.waittail
				if s.waittail == nil {
					s.waittail = t
				}
				s.waiters = t.waiters
				if s.waiters+1 != 0 {
					s.waiters++
				}
				t.parent = nil
				t.prev = nil
				t.next = nil
				t.waittail = nil
			} else {
				// 直接插入队尾
				// Add s to end of t's wait list.
				if t.waittail == nil {
					t.waitlink = s
				} else {
					t.waittail.waitlink = s
				}
				t.waittail = s
				s.waitlink = nil
				if t.waiters+1 != 0 {
					t.waiters++
				}
			}
			// 修改后 就返回
			return
		}
		// 向下个节点偏移 根据地址 决定向前向后
		last = t
		if uintptr(unsafe.Pointer(addr)) < uintptr(t.elem) {
			pt = &t.prev
		} else {
			pt = &t.next
		}
	}

	// 新的锁就新建一个二叉树节点
	// ticket 有与0作比较 所以必须大于0
	// Add s as new leaf in tree of unique addrs.
	// The balanced tree is a treap using ticket as the random heap priority.
	// That is, it is a binary tree ordered according to the elem addresses,
	// but then among the space of possible binary trees respecting those
	// addresses, it is kept balanced on average by maintaining a heap ordering
	// on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
	// https://en.wikipedia.org/wiki/Treap
	// https://faculty.washington.edu/aragon/pubs/rst89.pdf
	//
	// s.ticket compared with zero in couple of places, therefore set lowest bit.
	// It will not affect treap's quality noticeably.
	s.ticket = fastrand() | 1
	// last 作为新叶子节点的父节点
	s.parent = last
	// 将 s 置入 pt 插入新节点
	*pt = s

	// 插入新节点 需要保持优先级的平衡性
	// 利用 ticket 确定优先级
	// Rotate up into tree according to ticket (priority).
	for s.parent != nil && s.parent.ticket > s.ticket {
		if s.parent.prev == s {
			// s 是左子树 就右旋
			root.rotateRight(s.parent)
		} else {
			if s.parent.next != s {
				panic("semaRoot queue")
			}
			// s 是右子树 就左旋
			root.rotateLeft(s.parent)
		}
	}
}

// 出队单个sudog
// 如果正在分析 sudog 则 dequeue 将返回唤醒它的时间
// 否则返为0
// dequeue searches for and finds the first goroutine
// in semaRoot blocked on addr.
// If the sudog was being profiled, dequeue returns the time
// at which it was woken up as now. Otherwise now is 0.
// If there are additional entries in the wait list, dequeue
// returns tailtime set to the last entry's acquiretime.
// Otherwise tailtime is found.acquiretime.
func (root *semaRoot) dequeue(addr *uint32) (found *sudog, now, tailtime int64) {
	ps := &root.treap
	s := *ps
	for ; s != nil; s = *ps {
		if s.elem == unsafe.Pointer(addr) {
			// 找到才执行出队操作
			goto Found
		}
		if uintptr(unsafe.Pointer(addr)) < uintptr(s.elem) {
			ps = &s.prev
		} else {
			ps = &s.next
		}
	}
	return nil, 0, 0

Found:
	now = int64(0)
	if s.acquiretime != 0 {
		now = cputicks()
	}
	if t := s.waitlink; t != nil {
		// 有链表元素 直接移除队首元素 不用删树节点
		// Substitute t, also waiting on addr, for s in root tree of unique addrs.
		*ps = t
		t.ticket = s.ticket
		t.parent = s.parent
		t.prev = s.prev
		if t.prev != nil {
			t.prev.parent = t
		}
		t.next = s.next
		if t.next != nil {
			t.next.parent = t
		}
		if t.waitlink != nil {
			t.waittail = s.waittail
		} else {
			t.waittail = nil
		}
		t.waiters = s.waiters
		if t.waiters > 1 {
			t.waiters--
		}
		// Set head and tail acquire time to 'now',
		// because the caller will take care of charging
		// the delays before now for all entries in the list.
		t.acquiretime = now
		tailtime = s.waittail.acquiretime
		s.waittail.acquiretime = now
		s.waitlink = nil
		s.waittail = nil
	} else {
		// 没有后续链表元素 需要删除树节点 进行树的再平衡
		// 向下旋转为叶子节点 然后删除
		// Rotate s down to be leaf of tree for removal, respecting priorities.
		for s.next != nil || s.prev != nil {
			// 有非空子树 就进行旋转 将 s 旋转为叶子节点
			if s.next == nil || s.prev != nil && s.prev.ticket < s.next.ticket {
				// 左右子树皆不为空时 需要判断 左右节点的 ticket 保证优先级高度
				root.rotateRight(s)
			} else {
				// 否则 左旋
				root.rotateLeft(s)
			}
		}
		// 左右子树皆为空 即叶子节点 可以安全删除
		// Remove s, now a leaf.
		if s.parent != nil {
			// 有父节点 移除父节点指向
			if s.parent.prev == s {
				s.parent.prev = nil
			} else {
				s.parent.next = nil
			}
		} else {
			// 没有父节点 说明是最后一个节点
			root.treap = nil
		}
		tailtime = s.acquiretime
	}
	// 清理 s 无关数据
	s.parent = nil
	s.elem = nil
	s.next = nil
	s.prev = nil
	// 出队清空 ticket
	s.ticket = 0
	return s, now, tailtime
}

/*

  x                  y
 / \                / \
a   y      -->     x   c
   / \            / \
  b   c          a   b

*/

// rotateLeft rotates the tree rooted at node x.
// turning (x a (y b c)) into (y (x a b) c).
func (root *semaRoot) rotateLeft(x *sudog) {
	// p -> (x a (y b c))
	p := x.parent
	y := x.next
	b := y.prev

	y.prev = x
	x.parent = y
	x.next = b
	if b != nil {
		b.parent = x
	}

	y.parent = p
	if p == nil {
		root.treap = y
	} else if p.prev == x {
		p.prev = y
	} else {
		if p.next != x {
			throw("semaRoot rotateLeft")
		}
		p.next = y
	}
}

/*

    y              x
   / \            / \
  x   c    -->   a   y
 / \                / \
a   b              b   c

*/
// rotateRight rotates the tree rooted at node y.
// turning (y (x a b) c) into (x a (y b c)).
func (root *semaRoot) rotateRight(y *sudog) {
	// p -> (y (x a b) c)
	p := y.parent
	x := y.prev
	b := x.next

	x.next = y
	y.parent = x
	y.prev = b
	if b != nil {
		b.parent = y
	}

	x.parent = p
	if p == nil {
		root.treap = x
	} else if p.prev == y {
		p.prev = x
	} else {
		if p.next != y {
			throw("semaRoot rotateRight")
		}
		p.next = x
	}
}

// 用于 sync.Cond 的通知列表
// wait 为下一个等待者序号
// notify 为已经通知过的等待者序号
// notifyList is a ticket-based notification list used to implement sync.Cond.
//
// It must be kept in sync with the sync package.
type notifyList struct {
	// 下一个等待者的序号
	// wait is the ticket number of the next waiter. It is atomically
	// incremented outside the lock.
	wait atomic.Uint32

	// 下一个被通知的等待者序号 可以不用锁读但是必须锁写
	// wait 和 notify 可能会重复 但目前不太可能
	// notify is the ticket number of the next waiter to be notified. It can
	// be read outside the lock, but is only written to with lock held.
	//
	// Both wait & notify can wrap around, and such cases will be correctly
	// handled as long as their "unwrapped" difference is bounded by 2^31.
	// For this not to be the case, we'd need to have 2^31+ goroutines
	// blocked on the same condvar, which is currently not possible.
	notify uint32

	// List of parked waiters.
	lock mutex
	head *sudog
	tail *sudog
}

// 防止溢出
// less checks if a < b, considering a & b running counts that may overflow the
// 32-bit range, and that their "unwrapped" difference is always less than 2^31.
func less(a, b uint32) bool {
	return int32(a-b) < 0
}

// 生成等待者id
// notifyListAdd adds the caller to a notify list such that it can receive
// notifications. The caller must eventually call notifyListWait to wait for
// such a notification, passing the returned ticket number.
//
//go:linkname notifyListAdd sync.runtime_notifyListAdd
func notifyListAdd(l *notifyList) uint32 {
	// This may be called concurrently, for example, when called from
	// sync.Cond.Wait while holding a RWMutex in read mode.
	return l.wait.Add(1) - 1
}

// 添加进等待队列 l 中
// notifyListWait waits for a notification. If one has been sent since
// notifyListAdd was called, it returns immediately. Otherwise, it blocks.
//
//go:linkname notifyListWait sync.runtime_notifyListWait
func notifyListWait(l *notifyList, t uint32) {
	lockWithRank(&l.lock, lockRankNotifyList)

	// 如果 t 小于 notify 表示已经通知过了 直接返回
	// Return right away if this ticket has already been notified.
	if less(t, l.notify) {
		unlock(&l.lock)
		return
	}

	// 入队 插入队尾
	// Enqueue itself.
	s := acquireSudog()
	s.g = getg()
	s.ticket = t
	s.releasetime = 0
	t0 := int64(0)
	if blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s
	goparkunlock(&l.lock, waitReasonSyncCondWait, traceBlockCondWait, 3)
	if t0 != 0 {
		blockevent(s.releasetime-t0, 2)
	}
	releaseSudog(s)
}

// 唤醒 l 中所有等待者
// notifyListNotifyAll notifies all entries in the list.
//
//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
func notifyListNotifyAll(l *notifyList) {
	// 已经唤醒过 直接返回
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock.
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	// 获取锁 拷贝 l.head 减少占用锁时间 置处理标记 解锁
	// Pull the list out into a local variable, waiters will be readied
	// outside the lock.
	lockWithRank(&l.lock, lockRankNotifyList)
	s := l.head
	l.head = nil
	l.tail = nil

	// 将notify置为wait
	// Update the next ticket to be notified. We can set it to the current
	// value of wait because any previous waiters are already in the list
	// or will notice that they have already been notified when trying to
	// add themselves to the list.
	atomic.Store(&l.notify, l.wait.Load())
	unlock(&l.lock)

	// 唤醒所有的等待者
	// Go through the local list and ready all waiters.
	for s != nil {
		next := s.next
		s.next = nil
		readyWithTime(s, 4)
		s = next
	}
}

// 唤醒 l 第一个等待者
// notifyListNotifyOne notifies one entry in the list.
//
//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
func notifyListNotifyOne(l *notifyList) {
	// 已经唤醒过 直接返回
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock at all.
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	lockWithRank(&l.lock, lockRankNotifyList)

	// 二次校验是否已经唤醒过
	// Re-check under the lock if we need to do anything.
	t := l.notify
	if t == l.wait.Load() {
		unlock(&l.lock)
		return
	}

	// 增加唤醒标记
	// Update the next notify ticket number.
	atomic.Store(&l.notify, t+1)

	// 尝试查找需要唤醒的 sudog
	// 如果来不及插入此列表就被唤醒 则遍历 sudog 列表不会触发唤醒 但是在插入的时候会直接唤醒
	// 不存在表示被唤醒过
	// Try to find the g that needs to be notified.
	// If it hasn't made it to the list yet we won't find it,
	// but it won't park itself once it sees the new notify number.
	//
	// This scan looks linear but essentially always stops quickly.
	// Because g's queue separately from taking numbers,
	// there may be minor reorderings in the list, but we
	// expect the g we're looking for to be near the front.
	// The g has others in front of it on the list only to the
	// extent that it lost the race, so the iteration will not
	// be too long. This applies even when the g is missing:
	// it hasn't yet gotten to sleep and has lost the race to
	// the (few) other g's that we find on the list.
	for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
		if s.ticket == t {
			// 从队列中摘除 s
			n := s.next
			if p != nil {
				p.next = n
			} else {
				l.head = n
			}
			if n == nil {
				l.tail = p
			}
			unlock(&l.lock)
			s.next = nil
			// 找到了 唤醒s
			readyWithTime(s, 4)
			return
		}
	}
	unlock(&l.lock)
}

//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
	if sz != unsafe.Sizeof(notifyList{}) {
		print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
		throw("bad notifyList size")
	}
}

//go:linkname sync_nanotime sync.runtime_nanotime
func sync_nanotime() int64 {
	return nanotime()
}
