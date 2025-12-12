// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

// nih 让编译器识别为内部类型
// NOTE: keep in sync with cmd/compile/internal/types.CalcSize
// to make the compiler recognize this as an intrinsic type.
type nih struct{}

// NotInHeap 标志一种不会在堆上分配的对象
// 其他类型可以嵌入 NotInHeap 以使其不在堆中。
// 具体而言，指向这些类型的指针必须始终未通过 runtime.inheap 检查。
// 具体来说：
// 1. 'new（T）'， 'make（[]T）'， 'append（[]T， ...）' 和 T 的隐式堆分配是不允许的。尽管运行时中不允许隐式分配。
// 2. 指向常规类型的指针（除了 unsafe.Pointer） 不能转换为指向非堆类型的指针，即使它们具有相同的基础类型。
// 3. 任何包含非堆类型的类型本身都被视为非堆。
//   - 如果结构体和数组的元素不在堆中，则它们不在堆中。
//   - 不允许包含堆中的无堆类型的映射和通道。
// 4. 可以省略指向非堆类型的指针上的写入屏障。
// 最后一点是 NotInHeap 的真正好处。运行时将其用于低级内部结构，以避免调度程序和内存分配器中的内存屏障是非法的或效率低下的。
// 这种机制相当安全，并且不会影响运行时的可读性。
// NotInHeap is a type must never be allocated from the GC'd heap or on the stack,
// and is called not-in-heap.
//
// Other types can embed NotInHeap to make it not-in-heap. Specifically, pointers
// to these types must always fail the `runtime.inheap` check. The type may be used
// for global variables, or for objects in unmanaged memory (e.g., allocated with
// `sysAlloc`, `persistentalloc`, `fixalloc`, or from a manually-managed span).
//
// Specifically:
//
// 1. `new(T)`, `make([]T)`, `append([]T, ...)` and implicit heap
// allocation of T are disallowed. (Though implicit allocations are
// disallowed in the runtime anyway.)
//
// 2. A pointer to a regular type (other than `unsafe.Pointer`) cannot be
// converted to a pointer to a not-in-heap type, even if they have the
// same underlying type.
//
// 3. Any type that containing a not-in-heap type is itself considered as not-in-heap.
//
// - Structs and arrays are not-in-heap if their elements are not-in-heap.
// - Maps and channels contains no-in-heap types are disallowed.
//
// 4. Write barriers on pointers to not-in-heap types can be omitted.
//
// The last point is the real benefit of NotInHeap. The runtime uses
// it for low-level internal structures to avoid memory barriers in the
// scheduler and the memory allocator where they are illegal or simply
// inefficient. This mechanism is reasonably safe and does not compromise
// the readability of the runtime.
type NotInHeap struct{ _ nih }
