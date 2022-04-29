// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
type RWMutex struct {
	w           Mutex  // held if there are pending writers
	writerSem   uint32 // semaphore for writers to wait for completing readers
	readerSem   uint32 // semaphore for readers to wait for completing writers
	readerCount int32  // number of pending readers 当前读者数
	readerWait  int32  // number of departing readers 写锁等待读者数
}

// 读锁 标记阈值
const rwmutexMaxReaders = 1 << 30

// Happens-before relationships are indicated to the race detector via:
// - Unlock  -> Lock:  readerSem
// - Unlock  -> RLock: readerSem
// - RUnlock -> Lock:  writerSem
//
// The methods below temporarily disable handling of race synchronization
// events in order to provide the more precise model above to the race
// detector.
//
// For example, atomic.AddInt32 in RLock should not appear to provide
// acquire-release semantics, which would incorrectly synchronize racing
// readers, thus potentially missing races.

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if atomic.AddInt32(&rw.readerCount, 1) < 0 {
		// 写锁被锁 添加读锁等待
		// A writer is pending, wait for it.
		runtime_SemacquireMutex(&rw.readerSem, false, 0)
	}
	// 如果没有写锁 直接增加读者数
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// TryRLock tries to lock rw for reading and reports whether it succeeded.
//
// Note that while correct uses of TryRLock do exist, they are rare,
// and use of TryRLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (rw *RWMutex) TryRLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	for {
		c := atomic.LoadInt32(&rw.readerCount)
		if c < 0 {
			// 写锁占用
			if race.Enabled {
				race.Enable()
			}
			return false
		}
		if atomic.CompareAndSwapInt32(&rw.readerCount, c, c+1) {
			// cas 加上读者数
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(&rw.readerSem))
			}
			return true
		}
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// 写锁占用
		// Outlined slow-path to allow the fast-path to be inlined
		rw.rUnlockSlow(r)
	}
	// 没有写锁占用 直接 读者数 减一 返回
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		// 没有被锁
		race.Enable()
		fatal("sync: RUnlock of unlocked RWMutex")
	}
	// A writer is pending.
	if atomic.AddInt32(&rw.readerWait, -1) == 0 {
		// 最后一个等待者 解锁
		// 通知写锁的等待者
		// The last reader unblocks the writer.
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// 先锁住互斥锁
	// First, resolve competition with other writers.
	rw.w.Lock()
	// 暂存当前的读者数
	// Announce to readers there is a pending writer.
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
	// Wait for active readers.
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		// 增加等待者标记 且 等待者不为0 就将当前写锁挂入等待队列
		runtime_SemacquireMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// TryLock tries to lock rw for writing and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (rw *RWMutex) TryLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if !rw.w.TryLock() {
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	if !atomic.CompareAndSwapInt32(&rw.readerCount, 0, -rwmutexMaxReaders) {
		// cas 失败 直接返回
		rw.w.Unlock()
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
	// 没有读者 直接锁住 返回
	return true
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)
	if r >= rwmutexMaxReaders {
		// 解锁为锁住的锁
		race.Enable()
		fatal("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	for i := 0; i < int(r); i++ {
		// 唤醒所有的读者
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// Allow other writers to proceed.
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
