// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dragonfly || freebsd || linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	futexsleep(addr *uint32, val uint32, ns int64)
//		Atomically,
//			if *addr == val { sleep }
//		Might be woken up spuriously; that's allowed.
//		Don't sleep longer than ns; ns < 0 means forever.
//
//	futexwakeup(addr *uint32, cnt uint32)
//		If any procs are sleeping on addr, wake up at most cnt.

const (
	mutex_unlocked = 0
	// 内存原子锁
	mutex_locked = 1
	// futex 锁
	mutex_sleeping = 2

	active_spin     = 4
	active_spin_cnt = 30
	passive_spin    = 1
)

// key32 将 *uintptr 强转成 *uint32
// Possible lock states are mutex_unlocked, mutex_locked and mutex_sleeping.
// mutex_sleeping means that there is presumably at least one sleeping thread.
// Note that there can be spinning threads during all states - they do not
// affect mutex's state.

// We use the uintptr mutex.key and note.key as a uint32.
//
//go:nosplit
func key32(p *uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(p))
}

func mutexContended(l *mutex) bool {
	return atomic.Load(key32(&l.key)) > mutex_locked
}

func lock(l *mutex) {
	lockWithRank(l, getLockRank(l))
}

// lock2 锁住l
func lock2(l *mutex) {
	gp := getg()

	if gp.m.locks < 0 {
		throw("runtime·lock: lock count")
	}
	gp.m.locks++

	// 尝试锁住锁
	// Speculative grab for lock.
	v := atomic.Xchg(key32(&l.key), mutex_locked)
	if v == mutex_unlocked {
		// 从unlock变lock 表示获得锁
		return
	}

	// wait is either MUTEX_LOCKED or MUTEX_SLEEPING
	// depending on whether there is a thread sleeping
	// on this mutex. If we ever change l->key from
	// MUTEX_SLEEPING to some other value, we must be
	// careful to change it back to MUTEX_SLEEPING before
	// returning, to ensure that the sleeping thread gets
	// its wakeup call.
	wait := v

	timer := &lockTimer{lock: l}
	timer.begin()
	// On uniprocessors, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0
	if ncpu > 1 {
		// 多核 启动自旋
		spin = active_spin
	}
	for {
		// 尝试锁spin次
		// Try for lock, spinning.
		for i := 0; i < spin; i++ {
			for l.key == mutex_unlocked {
				// 未锁状态 尝试锁住
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					timer.end()
					return
				}
			}
			// 调用 PAUSE
			procyield(active_spin_cnt)
		}

		// 获取不到锁 就让出CPU
		// Try for lock, rescheduling.
		for i := 0; i < passive_spin; i++ {
			for l.key == mutex_unlocked {
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					timer.end()
					return
				}
			}
			// 拿不到锁 就切换线程
			osyield()
		}

		// Sleep.
		v = atomic.Xchg(key32(&l.key), mutex_sleeping)
		if v == mutex_unlocked {
			// 切换线程后 再次尝试获取锁
			timer.end()
			return
		}
		wait = mutex_sleeping
		futexsleep(key32(&l.key), mutex_sleeping, -1)
	}
}

func unlock(l *mutex) {
	unlockWithRank(l)
}

// unlock2 解锁 l
func unlock2(l *mutex) {
	v := atomic.Xchg(key32(&l.key), mutex_unlocked)
	if v == mutex_unlocked {
		// 已经解锁 panic
		throw("unlock of unlocked lock")
	}
	if v == mutex_sleeping {
		// 解锁 futex
		futexwakeup(key32(&l.key), 1)
	}

	gp := getg()
	gp.m.mLockProfile.recordUnlock(l)
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt { // restore the preemption request in case we've cleared it in newstack
		// m没有锁定的g 且 g可被抢占
		// 则标记g可抢占
		// 防止在 newstack 中清理掉
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
func noteclear(n *note) {
	// 置空
	n.key = 0
}

// 唤醒 note
func notewakeup(n *note) {
	old := atomic.Xchg(key32(&n.key), 1)
	if old != 0 {
		// wakeup 时 n.key 必须是0
		print("notewakeup - double wakeup (", old, ")\n")
		throw("notewakeup - double wakeup")
	}
	futexwakeup(key32(&n.key), 1)
}

// 休眠 note
func notesleep(n *note) {
	gp := getg()
	if gp != gp.m.g0 {
		// 必须是g0
		throw("notesleep not on g0")
	}
	ns := int64(-1)
	if *cgo_yield != nil {
		// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
		ns = 10e6
	}
	for atomic.Load(key32(&n.key)) == 0 {
		// wakeup 时 置1
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		// 休眠
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
	}
}

// notetsleep_internal 休眠 note 具体实现
// May run with m.p==nil if called from notetsleep, so write barriers
// are not allowed.
//
//go:nosplit
//go:nowritebarrier
func notetsleep_internal(n *note, ns int64) bool {
	// 获取 g0
	gp := getg()

	if ns < 0 {
		if *cgo_yield != nil {
			// 固定拦截器查询间隔
			// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
			ns = 10e6
		}
		for atomic.Load(key32(&n.key)) == 0 {
			gp.m.blocked = true
			futexsleep(key32(&n.key), 0, ns)
			if *cgo_yield != nil {
				asmcgocall(*cgo_yield, nil)
			}
			gp.m.blocked = false
		}
		return true
	}

	if atomic.Load(key32(&n.key)) != 0 {
		return true
	}

	deadline := nanotime() + ns
	for {
		if *cgo_yield != nil && ns > 10e6 {
			ns = 10e6
		}
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
		if atomic.Load(key32(&n.key)) != 0 {
			break
		}
		now := nanotime()
		if now >= deadline {
			break
		}
		ns = deadline - now
	}
	return atomic.Load(key32(&n.key)) != 0
}

// notetsleep 休眠 g0
func notetsleep(n *note, ns int64) bool {
	gp := getg()
	if gp != gp.m.g0 && gp.m.preemptoff != "" {
		throw("notetsleep not on g0")
	}

	return notetsleep_internal(n, ns)
}

// notetsleepg 休眠用户 g
// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall.
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}

	entersyscallblock()
	ok := notetsleep_internal(n, ns)
	exitsyscall()
	return ok
}

func beforeIdle(int64, int64) (*g, bool) {
	return nil, false
}

func checkTimeouts() {}
