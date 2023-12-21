// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// poolDequeue 无锁的单生产者多消费者的队列，生产者可以从头push和pop，消费者只能pop尾
// 附加功能是清理没有使用的 slot 避免不必要的对象保留，这点对sync.Pool很重要
// 支持操作 PushHead PopHead PopTail
// poolDequeue is a lock-free fixed-size single-producer,
// multi-consumer queue. The single producer can both push and pop
// from the head, and consumers can pop from the tail.
//
// It has the added feature that it nils out unused slots to avoid
// unnecessary retention of objects. This is important for sync.Pool,
// but not typically a property considered in the literature.
type poolDequeue struct {
	// headTail 是由32位头索引和32位尾索引组成，两者都是模len(vals)-1
	// tail 指向最旧的数据索引，head指向下一个填充的索引
	// [tail, head) 属于消费者
	// head 存储高32bit，可以原子添加，并且溢出无害
	// tail 存在低32bit
	// headTail packs together a 32-bit head index and a 32-bit
	// tail index. Both are indexes into vals modulo len(vals)-1.
	//
	// tail = index of oldest data in queue
	// head = index of next slot to fill
	//
	// Slots in the range [tail, head) are owned by consumers.
	// A consumer continues to own a slot outside this range until
	// it nils the slot, at which point ownership passes to the
	// producer.
	//
	// The head index is stored in the most-significant bits so
	// that we can atomically add to it and the overflow is
	// harmless.
	headTail atomic.Uint64

	// vals 是interface{}的环形队列，长度必须是2的幂次
	// vals[i].typ 为 nil 则 slot 是空的，否则non-nil
	// slot 没有使用标志，尾索引超过它并且typ已经被置为nil
	// 这由消费者原子的置空，并由生产者原子的读
	// vals is a ring buffer of interface{} values stored in this
	// dequeue. The size of this must be a power of 2.
	//
	// vals[i].typ is nil if the slot is empty and non-nil
	// otherwise. A slot is still in use until *both* the tail
	// index has moved beyond it and typ has been set to nil. This
	// is set to nil atomically by the consumer and read
	// atomically by the producer.
	vals []eface
}

type eface struct {
	typ, val unsafe.Pointer
}

const dequeueBits = 32

// dequeueLimit poolDequeue 的最大大小
// dequeueLimit is the maximum size of a poolDequeue.
//
// This must be at most (1<<dequeueBits)/2 because detecting fullness
// depends on wrapping around the ring buffer without wrapping around
// the index. We divide by 4 so this fits in an int on 32-bit.
const dequeueLimit = (1 << dequeueBits) / 4

// dequeueNil 用于表示poolDequeue中的空值，nil表示空slot
// dequeueNil is used in poolDequeue to represent interface{}(nil).
// Since we use nil to represent empty slots, we need a sentinel value
// to represent nil.
type dequeueNil *struct{}

// 解析head和tail
func (d *poolDequeue) unpack(ptrs uint64) (head, tail uint32) {
	const mask = 1<<dequeueBits - 1
	head = uint32((ptrs >> dequeueBits) & mask)
	tail = uint32(ptrs & mask)
	return
}

// 将head和tail封装成一个值
func (d *poolDequeue) pack(head, tail uint32) uint64 {
	const mask = 1<<dequeueBits - 1
	return (uint64(head) << dequeueBits) |
		uint64(tail&mask)
}

// pushHead 添加val到环形队列中，返回false表示队列已满，只能由单一生产者调用，没有加锁
// pushHead adds val at the head of the queue. It returns false if the
// queue is full. It must only be called by a single producer.
func (d *poolDequeue) pushHead(val any) bool {
	ptrs := d.headTail.Load()
	head, tail := d.unpack(ptrs)
	if (tail+uint32(len(d.vals)))&(1<<dequeueBits-1) == head {
		// Queue is full.
		return false
	}
	// 找到插入位置
	// 这里采用的&方式取模，所以 d.vals 的长度必须是2的幂次
	slot := &d.vals[head&uint32(len(d.vals)-1)]

	// 检查slot是否被释放
	// Check if the head slot has been released by popTail.
	typ := atomic.LoadPointer(&slot.typ)
	if typ != nil {
		// 另一个goroutine正在清理尾部，所以实际上队列是满的
		// Another goroutine is still cleaning up the tail, so
		// the queue is actually still full.
		return false
	}

	// val 为 nil 需要改为 dequeueNil 占位
	// The head slot is free, so we own it.
	if val == nil {
		val = dequeueNil(nil)
	}
	*(*any)(unsafe.Pointer(slot)) = val

	// 头指针偏移
	// Increment head. This passes ownership of slot to popTail
	// and acts as a store barrier for writing the slot.
	d.headTail.Add(1 << dequeueBits)
	return true
}

// popHead 移除并返回队列头部元素，如果返回false表示队列为空，只能有一个生产者调用
// popHead removes and returns the element at the head of the queue.
// It returns false if the queue is empty. It must only be called by a
// single producer.
func (d *poolDequeue) popHead() (any, bool) {
	var slot *eface
	for {
		ptrs := d.headTail.Load()
		head, tail := d.unpack(ptrs)
		if tail == head {
			// Queue is empty.
			return nil, false
		}

		// Confirm tail and decrement head. We do this before
		// reading the value to take back ownership of this
		// slot.
		head--
		ptrs2 := d.pack(head, tail)
		if d.headTail.CompareAndSwap(ptrs, ptrs2) {
			// 如果成功修改了值就退出循环
			// 获取弹出的 slot
			// We successfully took back slot.
			slot = &d.vals[head&uint32(len(d.vals)-1)]
			break
		}
	}

	// 获取 slot 的值
	val := *(*any)(unsafe.Pointer(slot))
	if val == dequeueNil(nil) {
		// 表示存储的是空对象
		val = nil
	}
	// 重置 slot
	// Zero the slot. Unlike popTail, this isn't racing with
	// pushHead, so we don't need to be careful here.
	*slot = eface{}
	return val, true
}

// popTail 移除并返回队列尾部元素，返回false表示队列为空，可以由多个消费者调用
// popTail removes and returns the element at the tail of the queue.
// It returns false if the queue is empty. It may be called by any
// number of consumers.
func (d *poolDequeue) popTail() (any, bool) {
	var slot *eface
	for {
		ptrs := d.headTail.Load()
		head, tail := d.unpack(ptrs)
		if tail == head {
			// Queue is empty.
			return nil, false
		}

		// Confirm head and tail (for our speculative check
		// above) and increment tail. If this succeeds, then
		// we own the slot at tail.
		ptrs2 := d.pack(head, tail+1)
		if d.headTail.CompareAndSwap(ptrs, ptrs2) {
			// Success.
			// cas 获取 slot
			slot = &d.vals[tail&uint32(len(d.vals)-1)]
			break
		}
	}

	// 获取 slot 指向的值
	// We now own slot.
	val := *(*any)(unsafe.Pointer(slot))
	if val == dequeueNil(nil) {
		val = nil
	}

	// slot.val 置空表示不会长时间对象引用
	// slot.typ 置空表示已经使用完毕
	// Tell pushHead that we're done with this slot. Zeroing the
	// slot is also important so we don't leave behind references
	// that could keep this object live longer than necessary.
	//
	// We write to val first and then publish that we're done with
	// this slot by atomically writing to typ.
	slot.val = nil
	atomic.StorePointer(&slot.typ, nil)
	// At this point pushHead owns the slot.

	return val, true
}

// poolChain 双端队列版本的 poolDequeue，每个dequeue都是前一个的两倍长度
// 一旦dequeue填满，就会新建一个dequeue，追加到最新的dequeue
// pop从另一端开始，直到dequeue用完，将dequeue从列表中移除
// poolChain is a dynamically-sized version of poolDequeue.
//
// This is implemented as a doubly-linked list queue of poolDequeues
// where each dequeue is double the size of the previous one. Once a
// dequeue fills up, this allocates a new one and only ever pushes to
// the latest dequeue. Pops happen from the other end of the list and
// once a dequeue is exhausted, it gets removed from the list.
type poolChain struct {
	// 指向push的dequeue，只能由生产者访问，所以不需要同步
	// head is the poolDequeue to push to. This is only accessed
	// by the producer, so doesn't need to be synchronized.
	head *poolChainElt

	// 指向pop的dequeue，由消费者访问，所以读写必须是原子操作
	// tail is the poolDequeue to popTail from. This is accessed
	// by consumers, so reads and writes must be atomic.
	tail *poolChainElt
}

// 池链节点
type poolChainElt struct {
	poolDequeue

	// next和prev连接相邻的两个节点元素
	// next由生产者原子的写，消费者原子的读，只能由nil转变成non-nil
	// prev由消费者原子的写，生产者原子的读，只能由non-nil转化为nil
	// next and prev link to the adjacent poolChainElts in this
	// poolChain.
	//
	// next is written atomically by the producer and read
	// atomically by the consumer. It only transitions from nil to
	// non-nil.
	//
	// prev is written atomically by the consumer and read
	// atomically by the producer. It only transitions from
	// non-nil to nil.
	next, prev *poolChainElt
}

// 原子存储内容
func storePoolChainElt(pp **poolChainElt, v *poolChainElt) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(pp)), unsafe.Pointer(v))
}

// 原子载入指向内容
func loadPoolChainElt(pp **poolChainElt) *poolChainElt {
	return (*poolChainElt)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(pp))))
}

// 将 val 存入 c 的队首
// 没锁 不会有冲突
func (c *poolChain) pushHead(val any) {
	d := c.head
	if d == nil {
		// 没有就初始化池链节点，并修改头尾节点
		// Initialize the chain.
		const initSize = 8 // Must be a power of 2
		d = new(poolChainElt)
		d.vals = make([]eface, initSize)
		c.head = d
		storePoolChainElt(&c.tail, d)
	}

	if d.pushHead(val) {
		// 往 poolQueue pushHead 成功直接返回
		return
	}

	// 到这表示d的队列已满，申请一个新的是原来的2倍大小
	// The current dequeue is full. Allocate a new one of twice
	// the size.
	newSize := len(d.vals) * 2
	if newSize >= dequeueLimit {
		// 又上限
		// Can't make it any bigger.
		newSize = dequeueLimit
	}

	// 创建新的池链节点
	d2 := &poolChainElt{prev: d}
	d2.vals = make([]eface, newSize)
	// 切换头指针指向新节点
	c.head = d2
	storePoolChainElt(&d.next, d2)
	d2.pushHead(val)
}

// 弹出队首元素
// 有 就返回
// 没有返回 nil
func (c *poolChain) popHead() (any, bool) {
	d := c.head
	for d != nil {
		if val, ok := d.popHead(); ok {
			// 成功弹出 直接返回
			return val, ok
		}
		// 当前没有，就往前找
		// There may still be unconsumed elements in the
		// previous dequeue, so try backing up.
		d = loadPoolChainElt(&d.prev)
	}
	return nil, false
}

// 弹出队尾元素
func (c *poolChain) popTail() (any, bool) {
	d := loadPoolChainElt(&c.tail)
	if d == nil {
		// 没有 tail 直接返回
		return nil, false
	}

	for {
		// 在pop tail之前记录下一个指针，很有必要
		// d也许短暂为空，但next在pop之前不为空，则d是永久为空，这是唯一安全删除d的条件
		// It's important that we load the next pointer
		// *before* popping the tail. In general, d may be
		// transiently empty, but if next is non-nil before
		// the pop and the pop fails, then d is permanently
		// empty, which is the only condition under which it's
		// safe to drop d from the chain.
		d2 := loadPoolChainElt(&d.next)

		if val, ok := d.popTail(); ok {
			// 当前有元素就直接返回
			return val, ok
		}

		if d2 == nil {
			// 这是唯一的dequeue，但现在为空，可能将来会被push
			// This is the only dequeue. It's empty right
			// now, but could be pushed to in the future.
			return nil, false
		}

		// 链尾已经排空，所以需要移动到下一个dequeue，尝试移除当前dequeue，防止重复查看
		// The tail of the chain has been drained, so move on
		// to the next dequeue. Try to drop it from the chain
		// so the next pop doesn't have to look at the empty
		// dequeue again.
		if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&c.tail)), unsafe.Pointer(d), unsafe.Pointer(d2)) {
			// We won the race. Clear the prev pointer so
			// the garbage collector can collect the empty
			// dequeue and so popHead doesn't back up
			// further than necessary.
			storePoolChainElt(&d2.prev, nil)
		}
		d = d2
	}
}
