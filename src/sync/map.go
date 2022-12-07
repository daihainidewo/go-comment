// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Map 并发安全的 map[interface{}]interface{}
// 不需要其他的同步操作
// 优化点 以下两种情况会比map减少锁的竞争
// 写入一次 读多次
// 多协程读写覆盖不相交的 key
// 零值即可用 不能被复制
// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
//
// In the terminology of the Go memory model, Map arranges that a write operation
// “synchronizes before” any read operation that observes the effect of the write, where
// read and write operations are defined as follows.
// Load, LoadAndDelete, LoadOrStore are read operations;
// Delete, LoadAndDelete, and Store are write operations;
// and LoadOrStore is a write operation when it returns loaded set to false.
type Map struct {
	mu Mutex

	// 无锁化并发访问安全的 map
	// 不需要获得锁 mu
	//
	// 读总是安全的 写需要获得锁 mu
	//
	// 存储在 read 中的 entry 可以在没有 mu 的情况下同时更新
	// 但更新先前清除的 entry 需要将该 entry 复制到 dirty map
	// 并在持有锁 mu 的情况下取消清除
	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	read atomic.Pointer[readOnly]

	// 有锁化 map
	// 操作前必须先获得 mu
	// 为了确保dirty map可以快速提升为read map
	// 它还包括read map中所有未清除的entry
	//
	// 已删除的 entry 不能存储到 dirty map 中
	// 在 clean map 中的已删除的 entry
	// 在新值存储进去之前必须先标记为未删除并且添加到 dirty map 中
	//
	// 如果 dirty map 为空
	// 下一个写入者将会初始化 dirty map
	// 并且会浅拷贝 clean map
	// 并会删除过期的 entry
	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	dirty map[any]*entry

	// 统计自上次更新 read map 以来需要锁定 mu 以确定密钥是否存在的加载次数
	//
	// 一旦足够数量的 misses 发生 去覆盖 dirty map 复制
	// dirty map 会被转为 read map（未修改状态）
	// 下次存储会生成新的 dirty map 的副本
	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	misses int
}

// readOnly 是不可变的结构 原子存储在 Map.read 中
// readOnly is an immutable struct stored atomically in the Map.read field.
type readOnly struct {
	// m 存储 kv
	// amended 表示是否有 dirty map 包含一些 key 不在 m
	m       map[any]*entry
	amended bool // true if the dirty map contains some key not in m.
}

// expunged 空接口指针 标记 已经从 dirty 中删除
// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
var expunged = new(any)

// An entry is a slot in the map corresponding to a particular key.
type entry struct {
	// p 是指向 interface{} 的指针
	// p == nil 表示已经删除 并且 m.dirty == nil 或 m.dirty[key] = e
	// p == expunged 则已被删除 m.dirty != nil 并且该条目从 m.dirty 中丢失
	// 否则，有效并记录在 m.read.m[key] 中 如果 m.dirty != nil，则记录在 m.dirty[key] 中
	//
	// 如果 entry 关联值可以被原子更新替换的前提是 p != expunged
	// 如果 p == expunged entry 关联值只能在第一次设置 m.dirty[key] = e 之后
	// 以至于使用 dirty 查找 entry
	//
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted, and either m.dirty == nil or
	// m.dirty[key] is e.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	p atomic.Pointer[any]
}

func newEntry(i any) *entry {
	e := &entry{}
	e.p.Store(&i)
	return e
}

func (m *Map) loadReadOnly() readOnly {
	if p := m.read.Load(); p != nil {
		return *p
	}
	return readOnly{}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (m *Map) Load(key any) (value any, ok bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	if !ok && read.amended {
		// 不存在 并且 read 需要修正
		m.mu.Lock()
		// 锁住 在进行二次check
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			// 再查 dirty
			e, ok = m.dirty[key]
			// 不管存在与否 都进行miss标记
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok {
		// 没找到 返回
		return nil, false
	}
	// 找到了 获取内部值
	return e.load()
}

func (e *entry) load() (value any, ok bool) {
	// 载入数据
	p := e.p.Load()
	if p == nil || p == expunged {
		// p 标记为 已删除就返回空
		return nil, false
	}
	return *p, true
}

// Store sets the value for a key.
func (m *Map) Store(key, value any) {
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		// read 中存在
		// 久尝试替换
		// 成功替换 直接返回
		return
	}

	// 不存在 或 被标记删除
	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 将被标记删除的 key 置空
		if e.unexpungeLocked() {
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			m.dirty[key] = e
		}
		// 存储值
		e.storeLocked(&value)
	} else if e, ok := m.dirty[key]; ok {
		// 如果在dirty中 更新 值
		e.storeLocked(&value)
	} else {
		if !read.amended {
			// read 标记为未修正 则执行修正操作
			// 标记 dirty 中有 read 不存在的key
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		// 将新值存入 dirty 中
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
}

// 尝试cas切换存储值
// 直到存储成功 或者被标记为删除
// tryStore stores a value if the entry has not been expunged.
//
// If the entry is expunged, tryStore returns false and leaves the entry
// unchanged.
func (e *entry) tryStore(i *any) bool {
	for {
		p := e.p.Load()
		if p == expunged {
			// p 标记为 已删除 返回 false
			return false
		}
		if e.p.CompareAndSwap(p, i) {
			return true
		}
	}
}

// 移除 e 的 已删除标记
// 如果 e 被标记为已删除 需要在解锁前必须添加进 dirty 中
// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return e.p.CompareAndSwap(expunged, nil)
}

// 原子替换 e 中的值指针
// storeLocked unconditionally stores a value to the entry.
//
// The entry must be known not to be expunged.
func (e *entry) storeLocked(i *any) {
	e.p.Store(i)
}

// LoadOrStore 如果存在返回当前值
// 否则 存储 并返回给定的值
// loaded 表示是否存入新值
// true 表示加载旧值 false 表示存储新值
// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
	// Avoid locking if it's a clean hit.
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 存在值 就尝试获取或设置
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			// 成功直接返回
			return actual, loaded
		}
	}

	// 不存在
	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 二次校验
		if e.unexpungeLocked() {
			// 如果能够移除 已删除标记 将 key 存入 dirty 中
			m.dirty[key] = e
		}
		// 再次尝试获取或设置
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		// 如果在 dirty 中 就尝试获取或设置
		actual, loaded, _ = e.tryLoadOrStore(value)
		// 检测是否切换 read 和 dirty
		m.missLocked()
	} else {
		// 既不在 read 也不在 dirty
		if !read.amended {
			// 标记 dirty 中有 read 不存在的 key
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// 尝试获取 如果没有 就设置
// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i any) (actual any, loaded, ok bool) {
	p := e.p.Load()
	if p == expunged {
		// 被标记 已删除 直接返回
		return nil, false, false
	}
	if p != nil {
		// 找到 且不为空 直接返回
		return *p, true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	ic := i
	for {
		if e.p.CompareAndSwap(nil, &ic) {
			// cas 切换 需要存入的值 成功 直接返回
			return i, false, true
		}
		p = e.p.Load()
		if p == expunged {
			// p 被标记 已删除 直接返回
			return nil, false, false
		}
		if p != nil {
			// 如果已经有值 返回
			return *p, true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	if !ok && read.amended {
		// 不存在 且 需要修正
		m.mu.Lock()
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			// 二次校验
			e, ok = m.dirty[key]
			// 删除 dirty
			// 如果 read 中存在 则置 nil
			delete(m.dirty, key)
			// 不管存在与否 增加 miss
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if ok {
		// read 或 dirty 存在
		// 删除
		return e.delete()
	}
	// 不存在直接返回
	return nil, false
}

// Delete deletes the value for a key.
func (m *Map) Delete(key any) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value any, ok bool) {
	for {
		p := e.p.Load()
		if p == nil || p == expunged {
			// 如果已被删除 直接返回
			return nil, false
		}
		if e.p.CompareAndSwap(p, nil) {
			// cas 置空 并返回原值
			return *p, true
		}
	}
}

// Range 遍历所有 key
// 如果 f 返回 false 则中断遍历
// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently (including by f), Range may reflect any
// mapping for that key from any point during the Range call. Range does not
// block other methods on the receiver; even f itself may call any method on m.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value any) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read := m.loadReadOnly()
	if read.amended {
		// read 需要修正
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read = m.loadReadOnly()
		if read.amended {
			// 切换 升级 dirty 为 read
			read = readOnly{m: m.dirty}
			m.read.Store(&read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 遍历所有 key 执行 f
	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// 增加 miss
// 并且检测 是否需要替换 read 和 dirty
// 即 是否将 dirty 升级为 read
func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		// miss 小于 dirty 直接返回
		return
	}
	// miss 大于 dirty 就将 dirty 升级为 read
	m.read.Store(&readOnly{m: m.dirty})
	// 置空 dirty miss
	m.dirty = nil
	m.misses = 0
}

// 存在 dirty 则不操作
// 不存在 则新建 dirty
// 将 read 中的有效值 拷贝到 dirty
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		// dirty 不为空 直接返回
		return
	}

	read := m.loadReadOnly()
	// 新建 dirty
	m.dirty = make(map[any]*entry, len(read.m))
	// 将 read 中的没有标记为已删除 的拷贝到 dirty 中
	for k, e := range read.m {
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

// e.p 标记为删除
func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := e.p.Load()
	for p == nil {
		// 将 nil 置为 已删除
		if e.p.CompareAndSwap(nil, expunged) {
			return true
		}
		p = e.p.Load()
	}
	// 返回 p 是否被置为删除
	return p == expunged
}
