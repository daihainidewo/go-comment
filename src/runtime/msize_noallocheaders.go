// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.allocheaders

// Malloc small size classes.
//
// See malloc.go for overview.
// See also mksizeclasses.go for how we decide what size classes to use.

package runtime

// 针对 size 做内存对齐
// Returns size of the memory block that mallocgc will allocate if you ask for the size.
//
// The noscan argument is purely for compatibility with goexperiment.AllocHeaders.
func roundupsize(size uintptr, noscan bool) uintptr {
	if size < _MaxSmallSize {
		// 符合内存池管理大小
		if size <= smallSizeMax-8 {
			// 小于等于 1024 - 8 的内存对齐
			return uintptr(class_to_size[size_to_class8[divRoundUp(size, smallSizeDiv)]])
		} else {
			// 大于等于 1024 的内存对齐
			return uintptr(class_to_size[size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]])
		}
	}
	// 超出内存池管理内存大小
	if size+_PageSize < size {
		// 溢出 直接返回
		return size
	}
	// 没有溢出 向上对齐 pagesize
	return alignUp(size, _PageSize)
}
