// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// A WaitGroup is a counting semaphore typically used to wait
// for a group of goroutines to finish.
//
// The main goroutine calls [WaitGroup.Add] to set (or increase) the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls [WaitGroup.Done] when finished. At the same time,
// [WaitGroup.Wait] can be used to block until all goroutines have finished.
//
// This is a typical pattern of WaitGroup usage to
// synchronize 3 goroutines, each calling the function f:
//
//	var wg sync.WaitGroup
//	for range 3 {
//	   wg.Add(1)
//	   go func() {
//	       defer wg.Done()
//	       f()
//	   }()
//	}
//	wg.Wait()
//
// For convenience, the [WaitGroup.Go] method simplifies this pattern to:
//
//	var wg sync.WaitGroup
//	for range 3 {
//	   wg.Go(f)
//	}
//	wg.Wait()
//
// A WaitGroup must not be copied after first use.
//
// In the terminology of [the Go memory model], a call to [WaitGroup.Done]
// “synchronizes before” the return of any Wait call that it unblocks.
//
// [the Go memory model]: https://go.dev/ref/mem
type WaitGroup struct {
	noCopy noCopy

	state atomic.Uint64 // high 32 bits are counter, low 32 bits are waiter count.
	sema  uint32
}

// Add adds delta, which may be negative, to the [WaitGroup] counter.
// If the counter becomes zero, all goroutines blocked on [WaitGroup.Wait] are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
func (wg *WaitGroup) Add(delta int) {
	if race.Enabled {
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	state := wg.state.Add(uint64(delta) << 32)
	v := int32(state >> 32)
	// 等待者个数
	w := uint32(state)
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(&wg.sema))
	}
	if v < 0 {
		// 没有需要等待的
		panic("sync: negative WaitGroup counter")
	}
	if w != 0 && delta > 0 && v == int32(delta) {
		// 同时调用 Add 和 Wait
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	if v > 0 || w == 0 {
		// 计数器不为0或没有等待者
		// 不需要通知 直接返回
		return
	}
	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	if wg.state.Load() != state {
		// 如果有变更说明有并发操作
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	wg.state.Store(0)
	for ; w != 0; w-- {
		// 唤醒所有等待者
		runtime_Semrelease(&wg.sema, false, 0)
	}
}

// Done decrements the [WaitGroup] counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the [WaitGroup] counter is zero.
func (wg *WaitGroup) Wait() {
	if race.Enabled {
		race.Disable()
	}
	for {
		state := wg.state.Load()
		// 计数器
		v := int32(state >> 32)
		// 等待者个数
		w := uint32(state)
		if v == 0 {
			// 计数器为0 直接 Wait 成功
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// Increment waiters count.
		if wg.state.CompareAndSwap(state, state+1) {
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(&wg.sema))
			}
			// 休眠等待被唤醒
			runtime_SemacquireWaitGroup(&wg.sema)
			if wg.state.Load() != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}

// Go calls f in a new goroutine and adds that task to the WaitGroup.
// When f returns, the task is removed from the WaitGroup.
//
// If the WaitGroup is empty, Go must happen before a [WaitGroup.Wait].
// Typically, this simply means Go is called to start tasks before Wait is called.
// If the WaitGroup is not empty, Go may happen at any time.
// This means a goroutine started by Go may itself call Go.
// If a WaitGroup is reused to wait for several independent sets of tasks,
// new Go calls must happen after all previous Wait calls have returned.
//
// In the terminology of [the Go memory model](https://go.dev/ref/mem),
// the return from f "synchronizes before" the return of any Wait call that it unblocks.
func (wg *WaitGroup) Go(f func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
	}()
}
