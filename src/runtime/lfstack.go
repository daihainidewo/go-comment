// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Lock-free stack.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// lfstack 是无锁栈的头
// 零值表示空链表
// 这个栈具有侵入性 节点必须嵌入 lfnode 作为第一个字段
// 栈不保留指向节点的 GC 可见指针
// 因此调用者负责确保节点不会被垃圾回收
// 通常通过从手动管理的内存中分配它们
// lfstack is the head of a lock-free stack.
//
// The zero value of lfstack is an empty list.
//
// This stack is intrusive. Nodes must embed lfnode as the first field.
//
// The stack does not keep GC-visible pointers to nodes, so the caller
// must ensure the nodes are allocated outside the Go heap.
type lfstack uint64

// push 将 node 插入到 lfstack 中
func (head *lfstack) push(node *lfnode) {
	node.pushcnt++
	new := lfstackPack(node, node.pushcnt)
	if node1 := lfstackUnpack(new); node1 != node {
		print("runtime: lfstack.push invalid packing: node=", node, " cnt=", hex(node.pushcnt), " packed=", hex(new), " -> node=", node1, "\n")
		throw("lfstack.push")
	}
	for {
		old := atomic.Load64((*uint64)(head))
		node.next = old
		if atomic.Cas64((*uint64)(head), old, new) {
			break
		}
	}
}

// pop 从 lfstack 中弹出一个节点
func (head *lfstack) pop() unsafe.Pointer {
	for {
		old := atomic.Load64((*uint64)(head))
		if old == 0 {
			return nil
		}
		node := lfstackUnpack(old)
		next := atomic.Load64(&node.next)
		if atomic.Cas64((*uint64)(head), old, next) {
			return unsafe.Pointer(node)
		}
	}
}

// empty 返回 lfstack 是否为空
func (head *lfstack) empty() bool {
	return atomic.Load64((*uint64)(head)) == 0
}

// lfnodeValidate 判断 node 是否是 lfstack.push 的合法地址
// 在 node 被申请时调用
// lfnodeValidate panics if node is not a valid address for use with
// lfstack.push. This only needs to be called when node is allocated.
func lfnodeValidate(node *lfnode) {
	if base, _, _ := findObject(uintptr(unsafe.Pointer(node)), 0, 0); base != 0 {
		throw("lfstack node allocated from the heap")
	}
	if lfstackUnpack(lfstackPack(node, ^uintptr(0))) != node {
		printlock()
		println("runtime: bad lfnode address", hex(uintptr(unsafe.Pointer(node))))
		throw("bad lfnode address")
	}
}

func lfstackPack(node *lfnode, cnt uintptr) uint64 {
	return uint64(taggedPointerPack(unsafe.Pointer(node), cnt))
}

func lfstackUnpack(val uint64) *lfnode {
	return (*lfnode)(taggedPointer(val).pointer())
}
