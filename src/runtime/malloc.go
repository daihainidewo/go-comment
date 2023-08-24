// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/* 内存分配器
小于等于32k的内存会被分为大约70个类（目前68个类），每个都有自己固定大小内存的集合。

分配器数据结构：
    fixalloc：用于固定大小的堆外对象的自由列表分配器，用于管理分配器使用的存储。
    mheap：malloc堆栈，以页大小（8192字节）为粒度管理
    mspan：内存管理分配单元
    mcentral：所有span的集合
    mcache：P中的空闲mspan缓存
    mstats：分配信息统计

小对象缓存方案：
    1. 将大小四舍五入为较小的类之一，然后在此P的mcache中查看相应的mspan。
        扫描mspan的可用位图以找到可用插槽。如果有空闲插槽，请分配它。
        无需获取锁即可完成所有操作。
    2. 如果mspan没有可用插槽，请从mcentral具有可用空间的所需大小类的mspan列表中获取一个新的mspan。
        获取mspan会锁定mcentral。
    3. 如果mcentral的mspan列表为空，从mheap获取当做mspan的页面。
    4. 如果mheap为空或没有足够大的页面运行，请从操作系统中分配一组新的页面（至少1MB）。
        分配大量页面将分摊与操作系统进行通信的成本。

扫描mspan并释放对象操作：
    1. 如果响应响应分配而扫描了mspan，则将其返回到mcache以满足分配。
    2. 否则，如果mspan仍在其中分配了对象，则将其放在mspan的size类的中心空闲列表中。
    3. 否则，如果mspan中的所有对象都空闲，则mspan的页面将返回到mheap，并且mspan现在已失效。

分配大对象直接调用mheap，跳过mcache和mcentral。

如果mspan.needzero为false，则mspan中的可用对象插槽已被清零。
否则，如果needzero为true，则在分配对象时将其清零。
通过这种方式延迟归零有很多好处：
    1. 堆栈帧分配可以完全避免归零。
    2. 由于程序可能即将写入内存，因此它具有更好的时间局部性。
    3. 我们不会将永远不会被重用的页面归零。

虚拟内存分布：
    堆栈由一些区块构成，64位机器大小为64M，32位机器大小为4M。每个区块首地址字节对齐。
    每个区块都有一个关联的 heapArena 对象。
    该对象存储该区块的元数据：区块中所有位置的bitmap和区块中所有页面的span map。
    它们本身是堆外分配的。
    由于区块是内存对齐的，因此可以将地址空间视为一系列区块帧。
    区块映射（mheap_.arenas）从区块帧号映射到heapArena，对于不由Go堆支持的部分地址空间，则映射为nil。
    区块映射的结构为两维数组，由一个L1区块映射和许多L2区块映射组成；
    但是，由于区块很大，因此在许多体系结构上，区块映射都由单个大型L2地图组成。
    区块映射覆盖了整个可能的地址空间，从而允许Go堆使用地址空间的任何部分。
    分配器尝试使区块保持连续，以便大跨度（大对象）可以越过竞技场。
*/
// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap objects,
//		used to manage storage used by the allocator.
//	mheap: the malloc heap, managed at page (8192-byte) granularity.
//	mspan: a run of in-use pages managed by the mheap.
//	mcentral: collects all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it
//	   is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it,
//	   it is placed on the mcentral free list for the mspan's size
//	   class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan's
//	   pages are returned to the mheap and the mspan is now dead.
//
// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
//
// If mspan.needzero is false, then free object slots in the mspan are
// already zeroed. Otherwise if needzero is true, objects are zeroed as
// they are allocated. There are various benefits to delaying zeroing
// this way:
//
//	1. Stack frame allocation can avoid zeroing altogether.
//
//	2. It exhibits better temporal locality, since the program is
//	   probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.

// Virtual memory layout
//
// The heap consists of a set of arenas, which are 64MB on 64-bit and
// 4MB on 32-bit (heapArenaBytes). Each arena's start address is also
// aligned to the arena size.
//
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena. heapArena objects are
// themselves allocated off-heap.
//
// Since arenas are aligned, the address space can be viewed as a
// series of arena frames. The arena map (mheap_.arenas) maps from
// arena frame number to *heapArena, or nil for parts of the address
// space not backed by the Go heap. The arena map is structured as a
// two-level array consisting of a "L1" arena map and many "L2" arena
// maps; however, since arenas are large, on many architectures, the
// arena map consists of a single, large L2 map.
//
// The arena map covers the entire possible address space, allowing
// the Go heap to use any part of the address space. The allocator
// attempts to keep arenas contiguous so that large spans (and hence
// large objects) can cross arenas.

package runtime

import (
	"internal/goarch"
	"internal/goos"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	maxTinySize   = _TinySize      // 16
	tinySizeClass = _TinySizeClass // 2
	maxSmallSize  = _MaxSmallSize  // 32768

	pageShift = _PageShift // 13
	pageSize  = _PageSize  // 8192

	concurrentSweep = _ConcurrentSweep // true

	_PageSize = 1 << _PageShift // 8192
	_PageMask = _PageSize - 1   // 8191

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	_FixAllocChunk = 16 << 10 // Chunk size for FixAlloc

	// Per-P, per order stack segment cache size.
	_StackCacheSize = 32 * 1024

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - goarch.PtrSize/4*goos.IsWindows - 1*goos.IsPlan9

	// heapAddrBits 堆地址中的位数，地址总线48位
	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// On ios/arm64, although 64-bit pointers are presumably
	// available, pointers are truncated to 33 bits in iOS <14.
	// Furthermore, only the top 4 GiB of the address space are
	// actually available to the application. In iOS >=14, more
	// of the address space is available, and the OS can now
	// provide addresses outside of those 33 bits. Pick 40 bits
	// as a reasonable balance between address space usage by the
	// page allocator, and flexibility for what mmap'd regions
	// we'll accept for the heap. We can't just move to the full
	// 48 bits because this uses too much address space for older
	// iOS versions.
	// TODO(mknyszek): Once iOS <14 is deprecated, promote ios/arm64
	// to a 48-bit address space like every other arm64 platform.
	//
	// WebAssembly currently has a limit of 4GB linear memory.
	// value 48
	heapAddrBits = (_64bit*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64))*48 + (1-_64bit+goarch.IsWasm)*(32-(goarch.IsMips+goarch.IsMipsle)) + 40*goos.IsIos*goarch.IsArm64

	// maxAlloc 理论上可以管理的内存，即地址总线能够标址的内存空间
	// maxAlloc is the maximum size of an allocation. On 64-bit,
	// it's theoretically possible to allocate 1<<heapAddrBits bytes. On
	// 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit
	// in a uintptr.
	// 1<<48 256T
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//      ios/arm64         33         4MB           1  2048  (8KB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes 表示一个arena包含内存大小
	// 堆由 heapArenaBytes 的映射组成，并基于 heapArenaBytes 对齐
	// heapArenaBytes is the size of a heap arena. The heap
	// consists of mappings of size heapArenaBytes, aligned to
	// heapArenaBytes. The initial heap mapping is one arena.
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows. We use smaller arenas on Windows
	// because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.
	// 1 << 26 = 64M
	heapArenaBytes = 1 << logHeapArenaBytes

	heapArenaWords = heapArenaBytes / goarch.PtrSize

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	// value 26
	logHeapArenaBytes = (6+20)*(_64bit*(1-goos.IsWindows)*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64)) + (2+20)*(_64bit*goos.IsWindows) + (2+20)*(1-_64bit) + (2+20)*goarch.IsWasm + (2+20)*goos.IsIos*goarch.IsArm64

	// heapArenaBitmapWords is the size of each heap arena's bitmap in uintptrs.
	heapArenaBitmapWords = heapArenaWords / (8 * goarch.PtrSize)

	// 8K
	pagesPerArena = heapArenaBytes / pageSize

	// arenaL1Bits L1大小的bit位数
	// arenaL1Bits is the number of bits of the arena number
	// covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	// 0
	arenaL1Bits = 6 * (_64bit * goos.IsWindows)

	// arenaL2Bits L2覆盖bit位数
	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	// 48 - 26 - 0 = 22
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	// 22
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	// 0 + 22
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned).
	//
	// On aix/ppc64, this offset allows to keep the heapAddrBits to
	// 48. Otherwise, it would be 60 in order to handle mmap addresses
	// (in range 0x0a00000000000000 - 0x0afffffffffffff). But in this
	// case, the memory reserved in (s *pageAlloc).init for chunks
	// is causing important slowdowns.
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	// value 0xffff800000000000
	arenaBaseOffset = 0xffff800000000000*goarch.IsAmd64 + 0x0a00000000000000*goos.IsAix
	// A typed version of this constant that will make it into DWARF (for viewcore).
	// 0xffff800000000000
	arenaBaseOffsetUintptr = uintptr(arenaBaseOffset)

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	minLegalPointer uintptr = 4096

	// minHeapForMetadataHugePages sets a threshold on when certain kinds of
	// heap metadata, currently the arenas map L2 entries and page alloc bitmap
	// mappings, are allowed to be backed by huge pages. If the heap goal ever
	// exceeds this threshold, then huge pages are enabled.
	//
	// These numbers are chosen with the assumption that huge pages are on the
	// order of a few MiB in size.
	//
	// The kind of metadata this applies to has a very low overhead when compared
	// to address space used, but their constant overheads for small heaps would
	// be very high if they were to be backed by huge pages (e.g. a few MiB makes
	// a huge difference for an 8 MiB heap, but barely any difference for a 1 GiB
	// heap). The benefit of huge pages is also not worth it for small heaps,
	// because only a very, very small part of the metadata is used for small heaps.
	//
	// N.B. If the heap goal exceeds the threshold then shrinks to a very small size
	// again, then huge pages will still be enabled for this mapping. The reason is that
	// there's no point unless we're also returning the physical memory for these
	// metadata mappings back to the OS. That would be quite complex to do in general
	// as the heap is likely fragmented after a reduction in heap size.
	minHeapForMetadataHugePages = 1 << 30
)

// physPageSize 是操作系统物理页面的大小（以字节为单位）。
// physPageSize is the size in bytes of the OS's physical pages.
// Mapping and unmapping operations must be done at multiples of
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before
// mallocinit.
// 4096
var physPageSize uintptr

// physHugePageSize 操作系统huge页，对应用程序不透明 假设为2的幂次
// physHugePageSize == 1 << physHugePageShift
// physHugePageSize is the size in bytes of the OS's default physical huge
// page size whose allocation is opaque to the application. It is assumed
// and verified to be a power of two.
//
// If set, this must be set by the OS init code (typically in osinit) before
// mallocinit. However, setting it at all is optional, and leaving the default
// value is always safe (though potentially less efficient).
//
// Since physHugePageSize is always assumed to be a power of two,
// physHugePageShift is defined as physHugePageSize == 1 << physHugePageShift.
// The purpose of physHugePageShift is to avoid doing divisions in
// performance critical functions.
var (
	physHugePageSize  uintptr
	physHugePageShift uint
)

// 操作系统内存管理抽象层
// 运行时管理的地址空间只能是4种状态
// 1）None，未映射的区域，默认状态
// 2）Reserved，由运行时拥有，但访问会出错，不计入进程内存使用
// 3）Prepared，保留，可有效地过渡到Ready，访问内存会出现未知错误（也许失败，也许返回意外的零等）
// 4）Ready，可以安全访问

// 每个操作系统都有辅助函数，用于状态的转换
// sysAlloc，将操作系统选择的内存区域从 None 转换为 Ready
// 		更具体地说，它从操作系统获得一大块清零内存，通常大约为 100KB 或 1MB 字节
// 		该内存始终可以立即使用。
// sysFree，将内存区域从任何状态转换为 None
// 		因此，它无条件地返回内存
// 		如果在分配的中途检测到内存不足错误或划出地址空间的对齐部分，则使用它
// 		仅当 sysReserve 总是返回一个与堆分配器的对齐限制对齐的内存区域时，sysFree 是空操作
// sysReserve，将内存区域从 None 转换为 Reserved
// 		它以这样一种方式保留地址空间，即在访问时会导致致命错误（通过权限或不提交内存）
// 		因此，这种预留永远不会得到物理内存的支持
// 		如果传递给它的指针非零，调用者希望在那里保留，但如果那个位置不可用，sysReserve 仍然可以选择另一个位置
// 		注意：sysReserve 返回 OS 对齐的内存，但堆分配器可能会使用更大的对齐方式，因此调用者必须小心重新对齐 sysReserve 获得的内存
// sysMap，将内存区域从 Reserved 转换为 Prepared
// 		它确保内存区域可以有效地过渡到 Ready
// sysUsed，将内存区域从 Prepared 转换为 Ready
// 		它通知操作系统需要该内存区域并确保可以安全访问该区域
// 		这在没有明确提交步骤和硬性过度提交限制的系统上通常是空操作，但例如在 Windows 上至关重要。
// sysUnused，将内存区域从 Ready 转换为 Prepared
// 		它通知操作系统不再需要支持该内存区域的物理页面，可以将其重用于其他目的
// 		sysUnused 内存区域的内容被认为是无效的，并且在调用 sysUsed 之前不得再次访问该区域。
// sysFault，将内存区域从 Ready 或 Prepared 转换为 Reserved
// 		它标记了一个区域，以便在访问时它总是会出错
// 		仅用于调试运行时

func mallocinit() {
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	if heapArenaBitmapWords&(heapArenaBitmapWords-1) != 0 {
		// heapBits expects modular arithmetic on bitmap
		// addresses to work.
		throw("heapArenaBitmapWords not a power of 2")
	}

	// Check physPageSize.
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		throw("failed to get system page size")
	}
	if physPageSize > maxPhysPageSize {
		print("system page size (", physPageSize, ") is larger than maximum page size (", maxPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}
	if physHugePageSize&(physHugePageSize-1) != 0 {
		print("system huge page size (", physHugePageSize, ") must be a power of 2\n")
		throw("bad system huge page size")
	}
	if physHugePageSize > maxPhysHugePageSize {
		// physHugePageSize is greater than the maximum supported huge page size.
		// Don't throw here, like in the other cases, since a system configured
		// in this way isn't wrong, we just don't have the code to support them.
		// Instead, silently set the huge page size to zero.
		physHugePageSize = 0
	}
	if physHugePageSize != 0 {
		// Since physHugePageSize is a power of 2, it suffices to increase
		// physHugePageShift until 1<<physHugePageShift == physHugePageSize.
		for 1<<physHugePageShift != physHugePageSize {
			physHugePageShift++
		}
	}
	if pagesPerArena%pagesPerSpanRoot != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerSpanRoot (", pagesPerSpanRoot, ")\n")
		throw("bad pagesPerSpanRoot")
	}
	if pagesPerArena%pagesPerReclaimerChunk != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerReclaimerChunk (", pagesPerReclaimerChunk, ")\n")
		throw("bad pagesPerReclaimerChunk")
	}

	if minTagBits > taggedPointerBits {
		throw("taggedPointerbits too small")
	}

	// Initialize the heap.
	mheap_.init()
	mcache0 = allocmcache()
	lockInit(&gcBitsArenas.lock, lockRankGcBitsArenas)
	lockInit(&profInsertLock, lockRankProfInsert)
	lockInit(&profBlockLock, lockRankProfBlock)
	lockInit(&profMemActiveLock, lockRankProfMemActive)
	for i := range profMemFutureLock {
		lockInit(&profMemFutureLock[i], lockRankProfMemFuture)
	}
	lockInit(&globalAlloc.mutex, lockRankGlobalAlloc)

	// Create initial arena growth hints.
	if goarch.PtrSize == 8 {
		// On a 64-bit machine, we pick the following hints
		// because:
		//
		// 1. Starting from the middle of the address space
		// makes it easier to grow out a contiguous range
		// without running in to some other mapping.
		//
		// 2. This makes Go heap addresses more easily
		// recognizable when debugging.
		//
		// 3. Stack scanning in gccgo is still conservative,
		// so it's important that addresses be distinguishable
		// from other data.
		//
		// Starting at 0x00c0 means that the valid memory addresses
		// will begin 0x00c0, 0x00c1, ...
		// In little-endian, that's c0 00, c1 00, ... None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible. If that fails, we try other 0xXXc0
		// addresses. An earlier attempt to use 0x11f8 caused out of memory errors
		// on OS X during thread allocations.  0x00c0 causes conflicts with
		// AddressSanitizer which reserves all memory up to 0x0100.
		// These choices reduce the odds of a conservative garbage collector
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On ios/arm64, the address space is even smaller.
		//
		// On AIX, mmaps starts at 0x0A00000000000000 for 64-bit.
		// processes.
		//
		// Space mapped for user arenas comes immediately after the range
		// originally reserved for the regular heap when race mode is not
		// enabled because user arena chunks can never be used for regular heap
		// allocations and we want to avoid fragmenting the address space.
		//
		// In race mode we have no choice but to just use the same hints because
		// the race detector requires that the heap be mapped contiguously.
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case raceenabled:
				// The TSAN runtime requires the heap
				// to be in the range [0x00c000000000,
				// 0x00e000000000).
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			case GOARCH == "arm64" && GOOS == "ios":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					continue
				}
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			default:
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			// Switch to generating hints for user arenas if we've gone
			// through about half the hints. In race mode, take only about
			// a quarter; we don't have very much space to work with.
			hintList := &mheap_.arenaHints
			if (!raceenabled && i > 0x3f) || (raceenabled && i > 0x5f) {
				hintList = &mheap_.userArena.arenaHints
			}
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			hint.addr = p
			hint.next, *hintList = *hintList, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize, true)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = alignUp(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
		}
		for _, arenaSize := range arenaSizes {
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				mheap_.arena.init(uintptr(a), size, false)
				p = mheap_.arena.end // For hint below
				break
			}
		}
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint

		// Place the hint for user arenas just after the large reservation.
		//
		// While this potentially competes with the hint above, in practice we probably
		// aren't going to be getting this far anyway on 32-bit platforms.
		userArenaHint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		userArenaHint.addr = p
		userArenaHint.next, mheap_.userArena.arenaHints = mheap_.userArena.arenaHints, userArenaHint
	}
	// Initialize the memory limit here because the allocator is going to look at it
	// but we haven't called gcinit yet and we're definitely going to allocate memory before then.
	gcController.memoryLimit.Store(maxInt64)
}

// sysAlloc 至少申请n自己的堆空间，返回的指针始终是heapArenaBytes对齐的
// 并由h.arenas元数据支持
// 返回的大小始终是heapArenaBytes的倍数
// sysAlloc失败时返回nil
// sysAlloc allocates heap arena space for at least n bytes. The
// returned pointer is always heapArenaBytes-aligned and backed by
// h.arenas metadata. The returned size is always a multiple of
// heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// hintList is a list of hint addresses for where to allocate new
// heap arenas. It must be non-nil.
//
// register indicates whether the heap arena should be registered
// in allArenas.
//
// sysAlloc returns a memory region in the Reserved state. This region must
// be transitioned to Prepared and then Ready before use.
//
// h must be locked.
func (h *mheap) sysAlloc(n uintptr, hintList **arenaHint, register bool) (v unsafe.Pointer, size uintptr) {
	assertLockHeld(&h.lock)

	// 向上对齐 heapArenaBytes
	n = alignUp(n, heapArenaBytes)

	if hintList == &h.arenaHints {
		// First, try the arena pre-reservation.
		// Newly-used mappings are considered released.
		//
		// Only do this if we're using the regular heap arena hints.
		// This behavior is only for the heap.
		v = h.arena.alloc(n, heapArenaBytes, &gcController.heapReleased)
		if v != nil {
			size = n
			goto mapped
		}
	}

	// 尝试在堆提示的地方增长
	// Try to grow the heap at a hint address.
	for *hintList != nil {
		hint := *hintList
		p := hint.addr
		if hint.down {
			p -= n
		}
		if p+n < p {
			// We can't use this, so don't ask.
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits {
			// Outside addressable heap. Can't use.
			v = nil
		} else {
			v = sysReserve(unsafe.Pointer(p), n)
		}
		// 成功就更新此提示
		if p == uintptr(v) {
			// Success. Update the hint.
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// 失败就放弃此提示
		// Failed. Discard this hint and try the next.
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simplify things there.
		if v != nil {
			sysFreeOS(v, n)
		}
		*hintList = hint.next
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give
		// us.
		// 从系统中获取
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		if v == nil {
			return nil, 0
		}

		// 创建新的提示
		// Create new hints for extending this region.
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// 检查错误的指针或者不能使用的指针
	// Check for bad pointers or pointers we can't use.
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

mapped:
	// Create arena metadata.
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ {
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// Allocate an L2 arena map.
			//
			// Use sysAllocOS instead of sysAlloc or persistentalloc because there's no
			// statistic we can comfortably account for this space in. With this structure,
			// we rely on demand paging to avoid large overheads, but tracking which memory
			// is paged in is too expensive. Trying to account for the whole region means
			// that it will appear like an enormous memory overhead in statistics, even though
			// it is not.
			l2 = (*[1 << arenaL2Bits]*heapArena)(sysAllocOS(unsafe.Sizeof(*l2)))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			if h.arenasHugePages {
				sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			} else {
				sysNoHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			}
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}

		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}
		var r *heapArena
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
		if r == nil {
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Register the arena in allArenas if requested.
		if register {
			if len(h.allArenas) == cap(h.allArenas) {
				size := 2 * uintptr(cap(h.allArenas)) * goarch.PtrSize
				if size == 0 {
					size = physPageSize
				}
				newArray := (*notInHeap)(persistentalloc(size, goarch.PtrSize, &memstats.gcMiscSys))
				if newArray == nil {
					throw("out of memory allocating allArenas")
				}
				oldSlice := h.allArenas
				*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / goarch.PtrSize)}
				copy(h.allArenas, oldSlice)
				// Do not free the old backing array because
				// there may be concurrent readers. Since we
				// double the array each time, this can lead
				// to at most 2x waste.
			}
			h.allArenas = h.allArenas[:len(h.allArenas)+1]
			h.allArenas[len(h.allArenas)-1] = ri
		}

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside to this).
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned 类似 sysReserve 返回字节对齐的地址
// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	retries := 0
retry:
	// 尝试直接获取
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		// 系统没有返回
		return nil, 0
	case p&(align-1) == 0:
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		sysFreeOS(unsafe.Pointer(p), size+align)
		p = alignUp(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			sysFreeOS(p2, size)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		return p2, size
	default:
		// Trim off the unaligned parts.
		pAligned := alignUp(p, align)
		sysFreeOS(unsafe.Pointer(p), pAligned-p)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			sysFreeOS(unsafe.Pointer(end), endLen)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// enableMetadataHugePages enables huge pages for various sources of heap metadata.
//
// A note on latency: for sufficiently small heaps (<10s of GiB) this function will take constant
// time, but may take time proportional to the size of the mapped heap beyond that.
//
// This function is idempotent.
//
// The heap lock must not be held over this operation, since it will briefly acquire
// the heap lock.
func (h *mheap) enableMetadataHugePages() {
	// Enable huge pages for page structure.
	h.pages.enableChunkHugePages()

	// Grab the lock and set arenasHugePages if it's not.
	//
	// Once arenasHugePages is set, all new L2 entries will be eligible for
	// huge pages. We'll set all the old entries after we release the lock.
	lock(&h.lock)
	if h.arenasHugePages {
		unlock(&h.lock)
		return
	}
	h.arenasHugePages = true
	unlock(&h.lock)

	// N.B. The arenas L1 map is quite small on all platforms, so it's fine to
	// just iterate over the whole thing.
	for i := range h.arenas {
		l2 := (*[1 << arenaL2Bits]*heapArena)(atomic.Loadp(unsafe.Pointer(&h.arenas[i])))
		if l2 == nil {
			continue
		}
		sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
	}
}

// base address for all 0-byte allocations
var zerobase uintptr

// nextFreeFast 返回下一个空闲地址
// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
func nextFreeFast(s *mspan) gclinkptr {
	theBit := sys.TrailingZeros64(s.allocCache) // Is there a free object in the allocCache?
	if theBit < 64 {
		// 有空闲对象
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			// 属于 mspan 的对象
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				// 当前 mspan 已分配完
				return 0
			}
			// 更新 allocCache 位图和空闲地址基址
			s.allocCache >>= uint(theBit + 1)
			s.freeindex = freeidx
			s.allocCount++
			// 返回标记位置的地址
			return gclinkptr(result*s.elemsize + s.base())
		}
	}
	return 0
}

// nextFree
// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	s = c.alloc[spc]
	shouldhelpgc = false
	freeIndex := s.nextFreeIndex()
	if freeIndex == s.nelems {
		// The span is full.
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		c.refill(spc)
		shouldhelpgc = true
		s = c.alloc[spc]

		freeIndex = s.nextFreeIndex()
	}

	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base())
	s.allocCount++
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// mallocgc 申请size大小的空间，小对象会从P本地的free list获取，大对象（大于32kB）直接从堆申请
// 申请后还会有一些GC的操作
// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	if size == 0 {
		// 返回大小为 0 的对象地址
		return unsafe.Pointer(&zerobase)
	}

	// It's possible for any malloc to trigger sweeping, which may in
	// turn queue finalizers. Record this dynamic lock edge.
	lockRankMayQueueFinalizer()

	userSize := size
	if asanenabled {
		// Refer to ASAN runtime library, the malloc() function allocates extra memory,
		// the redzone, around the user requested memory region. And the redzones are marked
		// as unaddressable. We perform the same operations in Go to detect the overflows or
		// underflows.
		size += computeRZlog(size)
	}

	if debug.malloc {
		// 开启 debug 模式
		if debug.sbrk != 0 {
			// 开启了 sbrk
			// 计算字节对齐
			align := uintptr(16)
			if typ != nil {
				// TODO(austin): This should be just
				//   align = uintptr(typ.align)
				// but that's only 4 on 32-bit platforms,
				// even if there's a uint64 field in typ (see #599).
				// This causes 64-bit atomic accesses to panic.
				// Hence, we use stricter alignment that matches
				// the normal allocator better.
				if size&7 == 0 {
					align = 8
				} else if size&3 == 0 {
					align = 4
				} else if size&1 == 0 {
					align = 2
				} else {
					align = 1
				}
			}
			// 申请持久化内存
			return persistentalloc(size, align, &memstats.other_sys)
		}

		if inittrace.active && inittrace.id == getg().goid {
			// Init functions are executed sequentially in a single goroutine.
			inittrace.allocs += 1
		}
	}

	// 检查 g 的负债 如果没有开始 GC 则为 nil
	// assistG is the G to charge for this allocation, or nil if
	// GC is not currently active.
	assistG := deductAssistCredit(size)

	// Set mp.mallocing to keep from being preempted by GC.
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	// 标记当前 m 正在执行申请内存操作
	mp.mallocing = 1

	shouldhelpgc := false
	dataSize := userSize
	// 获取 mcache
	c := getMCache(mp)
	if c == nil {
		throw("mallocgc called without a P or outside bootstrapping")
	}
	var span *mspan
	var x unsafe.Pointer
	// 类型是否带有指针
	noscan := typ == nil || typ.PtrBytes == 0
	// In some cases block zeroing can profitably (for latency reduction purposes)
	// be delayed till preemption is possible; delayedZeroing tracks that state.
	delayedZeroing := false
	if size <= maxSmallSize {
		// 小块内存
		if noscan && size < maxTinySize {
			// 无指针且小于细小内存则使用tiny分配器
			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be noscan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.
			off := c.tinyoffset
			// Align tiny pointer for required (conservative) alignment.
			if size&7 == 0 {
				off = alignUp(off, 8)
			} else if goarch.PtrSize == 4 && size == 12 {
				// Conservatively align 12-byte objects to 8 bytes on 32-bit
				// systems so that objects whose first field is a 64-bit
				// value is aligned to 8 bytes and does not cause a fault on
				// atomic access. See issue 37262.
				// TODO(mknyszek): Remove this workaround if/when issue 36606
				// is resolved.
				off = alignUp(off, 8)
			} else if size&3 == 0 {
				off = alignUp(off, 4)
			} else if size&1 == 0 {
				off = alignUp(off, 2)
			}
			if off+size <= maxTinySize && c.tiny != 0 {
				// The object fits into existing tiny block.
				x = unsafe.Pointer(c.tiny + off)
				c.tinyoffset = off + size
				c.tinyAllocs++
				mp.mallocing = 0
				releasem(mp)
				return x
			}
			// Allocate a new maxTinySize block.
			span = c.alloc[tinySpanClass]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(tinySpanClass)
			}
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			if !raceenabled && (size < c.tinyoffset || c.tiny == 0) {
				// Note: disabled when race detector is on, see comment near end of this function.
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else {
			// 其他小块内存分配
			var sizeclass uint8
			if size <= smallSizeMax-8 {
				sizeclass = size_to_class8[divRoundUp(size, smallSizeDiv)]
			} else {
				sizeclass = size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]
			}
			// 获取尺寸等级
			size = uintptr(class_to_size[sizeclass])
			spc := makeSpanClass(sizeclass, noscan)
			// 通过 mcache 获取指定等级的 mspan
			span = c.alloc[spc]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(x, size)
			}
		}
	} else {
		// 大块内存分配
		shouldhelpgc = true
		// For large allocations, keep track of zeroed state so that
		// bulk zeroing can be happen later in a preemptible context.
		span = c.allocLarge(size, noscan)
		span.freeindex = 1
		span.allocCount = 1
		size = span.elemsize
		x = unsafe.Pointer(span.base())
		if needzero && span.needzero != 0 {
			if noscan {
				delayedZeroing = true
			} else {
				memclrNoHeapPointers(x, size)
				// We've in theory cleared almost the whole span here,
				// and could take the extra step of actually clearing
				// the whole thing. However, don't. Any GC bits for the
				// uncleared parts will be zero, and it's just going to
				// be needzero = 1 once freed anyway.
			}
		}
	}

	if !noscan {
		// 有指针的块，标记内存与类型
		var scanSize uintptr
		heapBitsSetType(uintptr(x), size, dataSize, typ)
		if dataSize > typ.Size_ {
			// Array allocation. If there are any
			// pointers, GC has to scan to the last
			// element.
			if typ.PtrBytes != 0 {
				scanSize = dataSize - typ.Size_ + typ.PtrBytes
			}
		} else {
			scanSize = typ.PtrBytes
		}
		c.scanAlloc += scanSize
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	publicationBarrier()
	// As x and the heap bits are initialized, update
	// freeIndexForScan now so x is seen by the GC
	// (including conservative scan) as an allocated object.
	// While this pointer can't escape into user code as a
	// _live_ pointer until we return, conservative scanning
	// may find a dead pointer that happens to point into this
	// object. Delaying this update until now ensures that
	// conservative scanning considers this pointer dead until
	// this point.
	span.freeIndexForScan = span.freeindex

	// 如果在GC期间就将当前对象标记为黑
	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	if gcphase != _GCoff {
		gcmarknewobject(span, uintptr(x), size)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	if asanenabled {
		// We should only read/write the memory with the size asked by the user.
		// The rest of the allocated memory should be poisoned, so that we can report
		// errors when accessing poisoned memory.
		// The allocated memory is larger than required userSize, it will also include
		// redzone and some other padding bytes.
		rzBeg := unsafe.Add(x, userSize)
		asanpoison(rzBeg, size-userSize)
		asanunpoison(x, userSize)
	}

	if rate := MemProfileRate; rate > 0 {
		// Note cache c only valid while m acquired; see #47302
		if rate != 1 && size < c.nextSample {
			c.nextSample -= size
		} else {
			profilealloc(mp, x, size)
		}
	}
	mp.mallocing = 0
	releasem(mp)

	// Pointerfree data can be zeroed late in a context where preemption can occur.
	// x will keep the memory alive.
	if delayedZeroing {
		if !noscan {
			throw("delayed zeroing on data that may contain pointers")
		}
		memclrNoHeapPointersChunked(size, x) // This is a possible preemption point: see #47302
	}

	if debug.malloc {
		if debug.allocfreetrace != 0 {
			tracealloc(x, size, typ)
		}

		if inittrace.active && inittrace.id == getg().goid {
			// Init functions are executed sequentially in a single goroutine.
			inittrace.bytes += uint64(size)
		}
	}

	if assistG != nil {
		// Account for internal fragmentation in the assist
		// debt now that we know it.
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}

	if raceenabled && noscan && dataSize < maxTinySize {
		// Pad tinysize allocations so they are aligned with the end
		// of the tinyalloc region. This ensures that any arithmetic
		// that goes off the top end of the object will be detectable
		// by checkptr (issue 38872).
		// Note that we disable tinyalloc when raceenabled for this to work.
		// TODO: This padding is only performed when the race detector
		// is enabled. It would be nice to enable it if any package
		// was compiled with checkptr, but there's no easy way to
		// detect that (especially at compile time).
		// TODO: enable this padding for all allocations, not just
		// tinyalloc ones. It's tricky because of pointer maps.
		// Maybe just all noscan objects?
		x = add(x, size-dataSize)
	}

	return x
}

// deductAssistCredit reduces the current G's assist credit
// by size bytes, and assists the GC if necessary.
//
// Caller must be preemptible.
//
// Returns the G for which the assist credit was accounted.
func deductAssistCredit(size uintptr) *g {
	var assistG *g
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			gcAssistAlloc(assistG)
		}
	}
	return assistG
}

// memclrNoHeapPointersChunked repeatedly calls memclrNoHeapPointers
// on chunks of the buffer to be zeroed, with opportunities for preemption
// along the way.  memclrNoHeapPointers contains no safepoints and also
// cannot be preemptively scheduled, so this provides a still-efficient
// block copy that can also be preempted on a reasonable granularity.
//
// Use this with care; if the data being cleared is tagged to contain
// pointers, this allows the GC to run before it is all cleared.
func memclrNoHeapPointersChunked(size uintptr, x unsafe.Pointer) {
	v := uintptr(x)
	// got this from benchmarking. 128k is too small, 512k is too large.
	const chunkBytes = 256 * 1024
	vsize := v + size
	for voff := v; voff < vsize; voff = voff + chunkBytes {
		if getg().preempt {
			// may hold locks, e.g., profiling
			goschedguarded()
		}
		// clear min(avail, lump) bytes
		n := vsize - voff
		if n > chunkBytes {
			n = chunkBytes
		}
		memclrNoHeapPointers(unsafe.Pointer(voff), n)
	}
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function.
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

//go:linkname reflectlite_unsafe_New internal/reflectlite.unsafe_New
func reflectlite_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.Size_, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.Size_, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	c := getMCache(mp)
	if c == nil {
		throw("profilealloc called without a P or outside bootstrapping")
	}
	c.nextSample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample 返回内存 prof 下次采样时间，基于泊松分布
// nextSample returns the next sampling point for heap profiling. The goal is
// to sample allocations on average every MemProfileRate bytes, but with a
// completely random distribution over the allocation timeline; this
// corresponds to a Poisson process with parameter MemProfileRate. In Poisson
// processes, the distance between two samples follows the exponential
// distribution (exp(MemProfileRate)), so the best return value is a random
// number taken from an exponential distribution whose mean is MemProfileRate.
func nextSample() uintptr {
	if MemProfileRate == 1 {
		// Callers assign our return value to
		// mcache.next_sample, but next_sample is not used
		// when the rate is 1. So avoid the math below and
		// just return something.
		return 0
	}
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if gp := getg(); gp == gp.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return uintptr(fastexprand(MemProfileRate))
}

// fastexprand returns a random number from an exponential distribution with
// the specified mean.
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrandn(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() uintptr {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return uintptr(fastrandn(uint32(2 * rate)))
	}
	return 0
}

// persistentAlloc 持久申请对象
type persistentAlloc struct {
	base *notInHeap
	off  uintptr
}

// globalAlloc 全局持久化小内存分配器
var globalAlloc struct {
	mutex
	persistentAlloc
}

// persistentChunkSize 是我们在增长 persistentAlloc 时分配的字节数 256k
// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
const persistentChunkSize = 256 << 10

// persistentChunks 是我们分配的所有持久块的列表
// 该列表通过持久块中的第一个字节进行维护
// 这是原子更新的
// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
var persistentChunks *notInHeap

// persistentalloc 封装 sysAlloc 可以申请小块内存
// 没有关联的空闲操作
// 如果 align 是 0 则使用默认的 align 默认为 8
// 返回的内存总是被清零的
// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation.
// Intended for things like function/type/debug-related persistent data.
// If align is 0, uses default align (currently 8).
// The returned memory will be zeroed.
// sysStat must be non-nil.
//
// Consider marking persistentalloc'd types not in heap by embedding
// runtime/internal/sys.NotInHeap.
func persistentalloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// persistentalloc1 返回堆外连续内存分配器
// 只能在 g0 上执行
// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
//
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *sysMemStat) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			// 必须是 2 的幂次
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			// 不能太大
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	if size >= maxBlock {
		// 大块内存直接申请
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	// 小于 64K 的内存申请
	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		// m 有绑定的 p
		// 获取 p 的本地队列
		persistent = &mp.p.ptr().palloc
	} else {
		// m 没有 p
		// 获取全局的链表
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}
	// 调整字节对齐
	persistent.off = alignUp(persistent.off, align)
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		// 超出限制或者没有空间
		// 重新申请一块内存 大小为 persistentChunkSize
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))
		if persistent.base == nil {
			// 申请失败
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// 将新的 persistent.base 添加到 persistentChunks
		// 将 persistentChunks 更为 persistent.base 的地址
		// Add the new chunk to the persistentChunks list.
		for {
			// persistent.base = persistentChunks
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				// 确保 cas 更改成功
				break
			}
		}
		// 设置偏移量
		persistent.off = alignUp(goarch.PtrSize, align)
	}
	// 获取申请内存的基址
	p := persistent.base.add(persistent.off)
	// 增加偏移标识
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		// 如果是全局申请器 解锁
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		// 添加记录信息
		sysStat.add(int64(size))
		memstats.other_sys.add(-int64(size))
	}
	return p
}

// inPersistentAlloc 判断 p 指向内存是否是 persistentalloc 分配的
// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	for chunk != 0 {
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	return false
}

// linearAlloc 是一个简单的线性内存分配器 将预先保留的内存区域根据需要转换为 Ready状态
// 由调用方锁保护
// linearAlloc is a simple linear allocator that pre-reserves a region
// of memory and then optionally maps that region into the Ready state
// as needed.
//
// The caller is responsible for locking.
type linearAlloc struct {
	// next      下一个空闲字节
	// mapped    映射空间的最后一个字节
	// end       保留空间的最后一个字节
	// mapMemory 如果为true 表示内存从保留状态转到准备状态
	next   uintptr // next free byte
	mapped uintptr // one byte past end of mapped space
	end    uintptr // end of reserved space

	mapMemory bool // transition memory from Reserved to Ready if true
}

// init 初始化线性分配器
func (l *linearAlloc) init(base, size uintptr, mapMemory bool) {
	if base+size < base {
		// Chop off the last byte. The runtime isn't prepared
		// to deal with situations where the bounds could overflow.
		// Leave that memory reserved, though, so we don't map it
		// later.
		size -= 1
	}
	l.next, l.mapped = base, base
	l.end = base + size
	l.mapMemory = mapMemory
}

// alloc 申请内存，并标记使用
func (l *linearAlloc) alloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	// 字节对齐
	p := alignUp(l.next, align)
	if p+size > l.end {
		// 超限 返回空
		return nil
	}
	l.next = p + size
	// 向上对齐物理页大小
	if pEnd := alignUp(l.next-1, physPageSize); pEnd > l.mapped {
		if l.mapMemory {
			// Transition from Reserved to Prepared to Ready.
			n := pEnd - l.mapped
			sysMap(unsafe.Pointer(l.mapped), n, sysStat)
			sysUsed(unsafe.Pointer(l.mapped), n, n)
		}
		l.mapped = pEnd
	}
	return unsafe.Pointer(p)
}

// notInHeap 低级分配器分配的堆外内存
// notInHeap is off-heap memory allocated by a lower-level allocator
// like sysAlloc or persistentAlloc.
//
// In general, it's better to use real types which embed
// runtime/internal/sys.NotInHeap, but this serves as a generic type
// for situations where that isn't possible (like in the allocators).
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
type notInHeap struct{ _ sys.NotInHeap }

// add 基于 p 偏移 bytes 字节的指针数据
func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}

// computeRZlog computes the size of the redzone.
// Refer to the implementation of the compiler-rt.
func computeRZlog(userSize uintptr) uintptr {
	switch {
	case userSize <= (64 - 16):
		return 16 << 0
	case userSize <= (128 - 32):
		return 16 << 1
	case userSize <= (512 - 64):
		return 16 << 2
	case userSize <= (4096 - 128):
		return 16 << 3
	case userSize <= (1<<14)-256:
		return 16 << 4
	case userSize <= (1<<15)-512:
		return 16 << 5
	case userSize <= (1<<16)-1024:
		return 16 << 6
	default:
		return 16 << 7
	}
}
