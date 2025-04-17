// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

// fixalloc 是一个简单的固定大小对象的空闲链表分配器
// 由 sysAlloc 的 FixAlloc 管理 mcache 和 mspan 的对象
//
// fixalloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
//
// Consider marking fixalloc'd types not in heap by embedding
// internal/runtime/sys.NotInHeap.
type fixalloc struct {
	size   uintptr
	first  func(arg, p unsafe.Pointer) // called first time p is returned
	arg    unsafe.Pointer
	list   *mlink
	chunk  uintptr // use uintptr instead of unsafe.Pointer to avoid write barriers
	nchunk uint32  // bytes remaining in current chunk
	nalloc uint32  // size of new chunks in bytes
	inuse  uintptr // in-use bytes now
	stat   *sysMemStat
	zero   bool // zero allocations
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
type mlink struct {
	_    sys.NotInHeap
	next *mlink
}

// init 初始化 f 已分配给定大小的对象 使用分配器获取内存块
// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *sysMemStat) {
	if size > _FixAllocChunk {
		throw("runtime: fixalloc size too large")
	}
	// size 不能低于最小值
	size = max(size, unsafe.Sizeof(mlink{}))

	f.size = size
	f.first = first
	f.arg = arg
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.nalloc = uint32(_FixAllocChunk / size * size) // Round _FixAllocChunk down to an exact multiple of size to eliminate tail waste
	f.inuse = 0
	f.stat = stat
	f.zero = true
}

// alloc 申请指定大小的内存
func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	if f.list != nil {
		// 链表不为空 直接返回头结点
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		if f.zero {
			// 清空内存
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}
	if uintptr(f.nchunk) < f.size {
		// 获取一块持久内存
		f.chunk = uintptr(persistentalloc(uintptr(f.nalloc), 0, f.stat))
		f.nchunk = f.nalloc
	}

	v := unsafe.Pointer(f.chunk)
	if f.first != nil {
		// 执行指定的第一次操作
		f.first(f.arg, v)
	}
	// 修改记录数据
	f.chunk = f.chunk + f.size
	f.nchunk -= uint32(f.size)
	f.inuse += f.size
	return v
}

// free 释放 p
// 将 p 头插到 f.list 上
func (f *fixalloc) free(p unsafe.Pointer) {
	f.inuse -= f.size
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
