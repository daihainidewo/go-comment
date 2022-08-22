// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	_WorkbufSize = 2048 // in bytes; larger values result in less contention

	// workbufAlloc is the number of bytes to allocate at a time
	// for new workbufs. This must be a multiple of pageSize and
	// should be a multiple of _WorkbufSize.
	//
	// Larger values reduce workbuf allocation overhead. Smaller
	// values reduce heap fragmentation.
	workbufAlloc = 32 << 10
)

func init() {
	if workbufAlloc%pageSize != 0 || workbufAlloc%_WorkbufSize != 0 {
		throw("bad workbufAlloc")
	}
}

// GC 工作池
// Garbage collector work pool abstraction.
//
// This implements a producer/consumer model for pointers to grey
// objects. A grey object is one that is marked and on a work
// queue. A black object is marked and not on a work queue.
//
// Write barriers, root discovery, stack scanning, and object scanning
// produce pointers to grey objects. Scanning consumes pointers to
// grey objects, thus blackening them, and then scans them,
// potentially producing new pointers to grey objects.

// gcWork 提供生产消费的 GC 功能
// 抢占标记为 disabled
// put 生产
// tryGet 消费
// 重要的是
// 在标记阶段使用 gcWork 可以防止垃圾收集器过渡到标记终止
// 因为 gcWork 可能在本地保存 GC 工作缓冲区
// 这可以通过禁用抢占 systemstack 或 acquirem 来完成
// A gcWork provides the interface to produce and consume work for the
// garbage collector.
//
// A gcWork can be used on the stack as follows:
//
//	(preemption must be disabled)
//	gcw := &getg().m.p.ptr().gcw
//	.. call gcw.put() to produce and gcw.tryGet() to consume ..
//
// It's important that any use of gcWork during the mark phase prevent
// the garbage collector from transitioning to mark termination since
// gcWork may locally hold GC work buffers. This can be done by
// disabling preemption (systemstack or acquirem).
type gcWork struct {
	// wbuf1 和 wbuf2 是主要和次要的工作buffer
	// wbuf1 始终是 push pop 的缓冲
	// wbuf2 是将要丢弃的缓冲
	// 两者要么都为空 要么都不为空
	// wbuf1 and wbuf2 are the primary and secondary work buffers.
	//
	// This can be thought of as a stack of both work buffers'
	// pointers concatenated. When we pop the last pointer, we
	// shift the stack up by one work buffer by bringing in a new
	// full buffer and discarding an empty one. When we fill both
	// buffers, we shift the stack down by one work buffer by
	// bringing in a new empty buffer and discarding a full one.
	// This way we have one buffer's worth of hysteresis, which
	// amortizes the cost of getting or putting a work buffer over
	// at least one buffer of work and reduces contention on the
	// global work lists.
	//
	// wbuf1 is always the buffer we're currently pushing to and
	// popping from and wbuf2 is the buffer that will be discarded
	// next.
	//
	// Invariant: Both wbuf1 and wbuf2 are nil or neither are.
	wbuf1, wbuf2 *workbuf

	// 此 gcWork 上标记变黑的字节
	// 这由 dispose 聚合到 bytesMarked 中
	// Bytes marked (blackened) on this gcWork. This is aggregated
	// into work.bytesMarked by dispose.
	bytesMarked uint64

	// 对此 gcWork 执行的堆扫描工作
	// 这通过 dispose 聚合到 gcController 中
	// 也可以由调用者刷新
	// 其他类型的扫描工作会立即刷新
	// Heap scan work performed on this gcWork. This is aggregated into
	// gcController by dispose and may also be flushed by callers.
	// Other types of scan work are flushed immediately.
	heapScanWork int64

	// flushedWork 表示自上次 gcMarkDone 终止检查以来
	// 非空工作缓冲区已刷新到全局工作列表
	// 具体来说 这表明该 gcWork 可能已将工作传达给另一个 gcWork
	// 由 gcMarkDone 标记为 false
	// flushedWork indicates that a non-empty work buffer was
	// flushed to the global work list since the last gcMarkDone
	// termination check. Specifically, this indicates that this
	// gcWork may have communicated work to another gcWork.
	flushedWork bool
}

// gcWork 的大部分方法都是 go:nowritebarrierrec
// 因为写屏障本身可以调用 gcWork 方法
// 但是这些方法一般是不可重入的
// 因此如果 gcWork 方法在 gcWork 处于不一致状态时调用了写屏障
// 而写屏障又调用了 gcWork 方法
// 它可能会永久破坏 gcWork
// Most of the methods of gcWork are go:nowritebarrierrec because the
// write barrier itself can invoke gcWork methods but the methods are
// not generally re-entrant. Hence, if a gcWork method invoked the
// write barrier while the gcWork was in an inconsistent state, and
// the write barrier in turn invoked a gcWork method, it could
// permanently corrupt the gcWork.

// init 初始化 w
// wbuf1 从 empty 中获取
// wbuf2 优先从 full 中农获取
func (w *gcWork) init() {
	w.wbuf1 = getempty()
	wbuf2 := trygetfull()
	if wbuf2 == nil {
		wbuf2 = getempty()
	}
	w.wbuf2 = wbuf2
}

// put 将指针入队以便 GC 跟踪
// obj 必须指向堆对象或者 oblet
// put enqueues a pointer for the garbage collector to trace.
// obj must point to the beginning of a heap object or an oblet.
//
//go:nowritebarrierrec
func (w *gcWork) put(obj uintptr) {
	flushed := false
	wbuf := w.wbuf1
	// Record that this may acquire the wbufSpans or heap lock to
	// allocate a workbuf.
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans)
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
		// wbuf is empty at this point.
	} else if wbuf.nobj == len(wbuf.obj) {
		// wbuf1 满了 就交换
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1
		if wbuf.nobj == len(wbuf.obj) {
			// wbuf2 页满了 就放入 full 队列
			putfull(wbuf)
			w.flushedWork = true
			// 重新获取空 workbuf
			wbuf = getempty()
			w.wbuf1 = wbuf
			flushed = true
		}
	}

	// 存入对象
	wbuf.obj[wbuf.nobj] = obj
	wbuf.nobj++

	// If we put a buffer on full, let the GC controller know so
	// it can encourage more workers to run. We delay this until
	// the end of put so that w is in a consistent state, since
	// enlistWorker may itself manipulate w.
	if flushed && gcphase == _GCmark {
		// 刷新并且 GC 标记阶段
		// 尝试唤醒一个 GC worker
		gcController.enlistWorker()
	}
}

// putFast 返回对象是否存入 workbuf
// putFast does a put and reports whether it can be done quickly
// otherwise it returns false and the caller needs to call put.
//
//go:nowritebarrierrec
func (w *gcWork) putFast(obj uintptr) bool {
	wbuf := w.wbuf1
	if wbuf == nil || wbuf.nobj == len(wbuf.obj) {
		return false
	}

	wbuf.obj[wbuf.nobj] = obj
	wbuf.nobj++
	return true
}

// putBatch 批量存放指针对象
// putBatch performs a put on every pointer in obj. See put for
// constraints on these pointers.
//
//go:nowritebarrierrec
func (w *gcWork) putBatch(obj []uintptr) {
	if len(obj) == 0 {
		return
	}

	flushed := false
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
	}

	for len(obj) > 0 {
		for wbuf.nobj == len(wbuf.obj) {
			// 将 wbuf1 存入 full 列表
			putfull(wbuf)
			w.flushedWork = true
			// 交换 wbuf1 和 wbuf2
			w.wbuf1, w.wbuf2 = w.wbuf2, getempty()
			wbuf = w.wbuf1
			flushed = true
		}
		// 将 obj 追加进 wbuf 中
		n := copy(wbuf.obj[wbuf.nobj:], obj)
		wbuf.nobj += n
		obj = obj[n:]
	}

	if flushed && gcphase == _GCmark {
		// 刷新且在 GC 标记阶段
		// 尝试启动一个 GC worker
		gcController.enlistWorker()
	}
}

// tryGet 出队一个指针用于 GC 跟踪
// tryGet dequeues a pointer for the garbage collector to trace.
//
// If there are no pointers remaining in this gcWork or in the global
// queue, tryGet returns 0.  Note that there may still be pointers in
// other gcWork instances or other caches.
//
//go:nowritebarrierrec
func (w *gcWork) tryGet() uintptr {
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
		// wbuf is empty at this point.
	}
	if wbuf.nobj == 0 {
		// wbuf1 对象数为 0
		// 交换 wbuf1 和 wbuf2
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1
		if wbuf.nobj == 0 {
			// wbuf2 对象数也为 0
			owbuf := wbuf
			// 尝试从 full 获取
			wbuf = trygetfull()
			if wbuf == nil {
				// 仍然为空 返回
				return 0
			}
			// 归还空的 wbuf
			putempty(owbuf)
			// 设置 wbuf1
			w.wbuf1 = wbuf
		}
	}

	// 返回对象
	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// tryGetFast 类似 tryGet
// wbuf1 有可以用就返回 没有就返回 0
// tryGetFast dequeues a pointer for the garbage collector to trace
// if one is readily available. Otherwise it returns 0 and
// the caller is expected to call tryGet().
//
//go:nowritebarrierrec
func (w *gcWork) tryGetFast() uintptr {
	wbuf := w.wbuf1
	if wbuf == nil || wbuf.nobj == 0 {
		// wbuf1 为空
		return 0
	}

	// 返回一个对象
	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// dispose 返回全局队列中任一缓存之战 重置 gcWork
// 缓冲区被放在完整的队列中
// 这样写屏障就不会在 GC 可以检查它们之前简单地重新获取它们
// 这有助于降低 mutator 在并发标记阶段隐藏指针的能力
// dispose returns any cached pointers to the global queue.
// The buffers are being put on the full queue so that the
// write barriers will not simply reacquire them before the
// GC can inspect them. This helps reduce the mutator's
// ability to hide pointers during the concurrent mark phase.
//
//go:nowritebarrierrec
func (w *gcWork) dispose() {
	if wbuf := w.wbuf1; wbuf != nil {
		// wbuf1 不为空
		if wbuf.nobj == 0 {
			// 没有对象 直接存入 empty
			putempty(wbuf)
		} else {
			// 有对象 直接存入 full
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf1 = nil

		wbuf = w.wbuf2
		if wbuf.nobj == 0 {
			putempty(wbuf)
		} else {
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf2 = nil
	}
	if w.bytesMarked != 0 {
		// 将当前 w 标记完成的字节添加到 work
		// 重置 w 标记字节数
		// dispose happens relatively infrequently. If this
		// atomic becomes a problem, we should first try to
		// dispose less and if necessary aggregate in a per-P
		// counter.
		atomic.Xadd64(&work.bytesMarked, int64(w.bytesMarked))
		w.bytesMarked = 0
	}
	if w.heapScanWork != 0 {
		// 将 w 的扫描工作数添加到 gcController
		// 重置 w 的扫描工作数
		gcController.heapScanWork.Add(w.heapScanWork)
		w.heapScanWork = 0
	}
}

// balance 将本地的额多余的工作移到全局工作列表中
// balance moves some work that's cached in this gcWork back on the
// global queue.
//
//go:nowritebarrierrec
func (w *gcWork) balance() {
	if w.wbuf1 == nil {
		return
	}
	if wbuf := w.wbuf2; wbuf.nobj != 0 {
		// wbuf2 有对象
		// 将对象塞入 full 列表中
		putfull(wbuf)
		w.flushedWork = true
		// 重置 wbuf2
		w.wbuf2 = getempty()
	} else if wbuf := w.wbuf1; wbuf.nobj > 4 {
		// wbuf1 大于 4 个对象
		// 将 wbuf1 一般对象塞入 full 列表中
		w.wbuf1 = handoff(wbuf)
		w.flushedWork = true // handoff did putfull
	} else {
		return
	}
	// We flushed a buffer to the full list, so wake a worker.
	if gcphase == _GCmark {
		// 标记阶段
		// 尝试唤醒一个 GC worker 清理 full
		gcController.enlistWorker()
	}
}

// empty 返回 w 是否有标记工作
// empty reports whether w has no mark work available.
//
//go:nowritebarrierrec
func (w *gcWork) empty() bool {
	return w.wbuf1 == nil || (w.wbuf1.nobj == 0 && w.wbuf2.nobj == 0)
}

// GC 工作池保存在工作缓冲区的数组中
// gcWork 接口缓存一个工作缓冲区直到满（或空）
// 以避免在全局工作缓冲区列表上竞争
// Internally, the GC work pool is kept in arrays in work buffers.
// The gcWork interface caches a work buffer until full (or empty) to
// avoid contending on the global work buffer lists.

type workbufhdr struct {
	node lfnode // must be first
	nobj int
}

// workbuf 填充整个 span 空间
type workbuf struct {
	_ sys.NotInHeap
	workbufhdr
	// account for the above fields
	obj [(_WorkbufSize - unsafe.Sizeof(workbufhdr{})) / goarch.PtrSize]uintptr
}

// workbuf factory routines. These funcs are used to manage the
// workbufs.
// If the GC asks for some work these are the only routines that
// make wbufs available to the GC.

func (b *workbuf) checknonempty() {
	if b.nobj == 0 {
		throw("workbuf is empty")
	}
}

func (b *workbuf) checkempty() {
	if b.nobj != 0 {
		throw("workbuf is not empty")
	}
}

// getempty 从 work.empty 队列弹出空的 workbuf
// 如果没有可用的就新建一个
// getempty pops an empty work buffer off the work.empty list,
// allocating new buffers if none are available.
//
//go:nowritebarrier
func getempty() *workbuf {
	var b *workbuf
	if work.empty != 0 {
		// 从全局 empty 队列中获取
		b = (*workbuf)(work.empty.pop())
		if b != nil {
			b.checkempty()
		}
	}
	// Record that this may acquire the wbufSpans or heap lock to
	// allocate a workbuf.
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans)
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)
	if b == nil {
		// 申请新的
		// Allocate more workbufs.
		var s *mspan
		if work.wbufSpans.free.first != nil {
			// 全局有缓存 workbuf 专用 span
			lock(&work.wbufSpans.lock)
			s = work.wbufSpans.free.first
			if s != nil {
				work.wbufSpans.free.remove(s)
				work.wbufSpans.busy.insert(s)
			}
			unlock(&work.wbufSpans.lock)
		}
		if s == nil {
			// s 仍旧为空
			systemstack(func() {
				// 向 mheap 申请 span
				s = mheap_.allocManual(workbufAlloc/pageSize, spanAllocWorkBuf)
			})
			if s == nil {
				throw("out of memory")
			}
			// 插入 busy 列表
			// Record the new span in the busy list.
			lock(&work.wbufSpans.lock)
			work.wbufSpans.busy.insert(s)
			unlock(&work.wbufSpans.lock)
		}
		// 将申请的 span 划分成多个栈
		// Slice up the span into new workbufs. Return one and
		// put the rest on the empty list.
		for i := uintptr(0); i+_WorkbufSize <= workbufAlloc; i += _WorkbufSize {
			newb := (*workbuf)(unsafe.Pointer(s.base() + i))
			newb.nobj = 0
			// 校验是否是无锁栈
			lfnodeValidate(&newb.node)
			// 第一个返回 剩下的添加进空列表
			if i == 0 {
				b = newb
			} else {
				putempty(newb)
			}
		}
	}
	return b
}

// putempty 将 workbuf 添加进 work.empty 列表
// 放进时 g 拥有 b
// lfstack.push 放弃拥有
// putempty puts a workbuf onto the work.empty list.
// Upon entry this goroutine owns b. The lfstack.push relinquishes ownership.
//
//go:nowritebarrier
func putempty(b *workbuf) {
	b.checkempty()
	work.empty.push(&b.node)
}

// putfull 将 workbuf 添加进 GC work.full 列表
// putfull puts the workbuf on the work.full list for the GC.
// putfull accepts partially full buffers so the GC can avoid competing
// with the mutators for ownership of partially full buffers.
//
//go:nowritebarrier
func putfull(b *workbuf) {
	b.checknonempty()
	work.full.push(&b.node)
}

// trygetfull 尝试从 full 中获取 workbuf
// 没有立即可用的返回 nil
// trygetfull tries to get a full or partially empty workbuffer.
// If one is not immediately available return nil
//
//go:nowritebarrier
func trygetfull() *workbuf {
	b := (*workbuf)(work.full.pop())
	if b != nil {
		b.checknonempty()
		return b
	}
	return b
}

// handoff 移除一半数据到新的 workbuf
//go:nowritebarrier
func handoff(b *workbuf) *workbuf {
	// Make new buffer with half of b's pointers.
	b1 := getempty()
	n := b.nobj / 2
	b.nobj -= n
	b1.nobj = n
	// 拷贝一半数据
	memmove(unsafe.Pointer(&b1.obj[0]), unsafe.Pointer(&b.obj[b.nobj]), uintptr(n)*unsafe.Sizeof(b1.obj[0]))

	// 将 b 添加进 full 列表
	// Put b on full list - let first half of b get stolen.
	putfull(b)
	return b1
}

// prepareFreeWorkbufs 将 busy workbuf span 移动到 free 列表
// 可以释放还给 heap
// 只能在所有 workbuf 都在 empty 列表中才可以调用
// prepareFreeWorkbufs moves busy workbuf spans to free list so they
// can be freed to the heap. This must only be called when all
// workbufs are on the empty list.
func prepareFreeWorkbufs() {
	lock(&work.wbufSpans.lock)
	if work.full != 0 {
		throw("cannot free workbufs when work.full != 0")
	}
	// Since all workbufs are on the empty list, we don't care
	// which ones are in which spans. We can wipe the entire empty
	// list and move all workbuf spans to the free list.
	work.empty = 0
	// 归还 work.busy span
	work.wbufSpans.free.takeAll(&work.wbufSpans.busy)
	unlock(&work.wbufSpans.lock)
}

// freeSomeWbufs 释放 workbuf 还给 heap
// 返回 true 应该继续调用以释放更多
// freeSomeWbufs frees some workbufs back to the heap and returns
// true if it should be called again to free more.
func freeSomeWbufs(preemptible bool) bool {
	const batchSize = 64 // ~1–2 µs per span.
	lock(&work.wbufSpans.lock)
	if gcphase != _GCoff || work.wbufSpans.free.isEmpty() {
		unlock(&work.wbufSpans.lock)
		return false
	}
	systemstack(func() {
		gp := getg().m.curg
		for i := 0; i < batchSize && !(preemptible && gp.preempt); i++ {
			span := work.wbufSpans.free.first
			if span == nil {
				break
			}
			// 将 span 还给 mheap
			work.wbufSpans.free.remove(span)
			mheap_.freeManual(span, spanAllocWorkBuf)
		}
	})
	more := !work.wbufSpans.free.isEmpty()
	unlock(&work.wbufSpans.lock)
	return more
}
