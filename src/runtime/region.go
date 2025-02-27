// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Implementation of regions.
//
// This file contains the implementation of regions wherein Go values can
// be manually allocated and freed in bulk. There are other
// potential efficiency benefits, such as improved locality and access to a more
// efficient allocation strategy.
//
// The implementation works in layers. At the bottom, regions are managed in blocks.
// Each block must be a multiple of the heap arena size, or the heap arena size must
// be divisible by the region blocks. The address space for each block, and each
// corresponding heapArena for that address space, are eternally reserved for use as
// region blocks. That is, they can never be used for the general heap. Each block
// is also represented by a single mspan, and is modeled as a single large heap
// allocation. It must be, because each chunk contains ordinary Go values that may
// point into the heap, so it must be scanned just like any other object. Any
// pointer into a block will therefore always cause the whole block to be scanned
// while its corresponding region is still live.
//
// At the next layer are the user regions themselves. They consist of a single
// active block which new Go values are bump-allocated into and a list of blocks
// that were exhausted when allocating into the region. Once the region is freed,
// it frees all full blocks it references. Each region keeps its list of referenced blocks
// explicitly live until it is freed.
//
// Memory is bump-allocated from low addresses to high addresses in each
// chunk.
//
// In summary, there are 2 resources: region, and region blocks. They exist in the
// following lifecycle:
//
// (1) A new region is created via createUserRegion.
// (2) Blocks are allocated to hold memory allocated into the region with allocFromRegion.
//    (a) Blocks are first allocated from the reuse list of partially-used block.
//    (b) Failing all above, memory for a new block is mapped.
// (3) The region is freed, or all references to it are dropped, triggering its finalizer.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/runtime/math"
	"unsafe"
)

// region_createRegion is a wrapper around createUserRegion.
//
//go:linkname region_createRegion region.runtime_region_createRegion
func region_createRegion() unsafe.Pointer {
	return unsafe.Pointer(createUserRegion())
}

// region_allocFromRegion is a wrapper around (*userRegion).allocateFromRegion, except that typ
// is an any (must be a *_type, still) and typ must be a type descriptor
// for a pointer to the type to actually be allocated, i.e. pass a *T
// to allocate a T. This is necessary because this function returns a *T.
//
//go:linkname region_allocFromRegion region.runtime_region_allocFromRegion
func region_allocFromRegion(region unsafe.Pointer, typ any) any {
	return ((*userRegion)(region)).allocateFromRegion(typ)
}

// region_removeRegion is a wrapper around (*userRegion).removeRegion.
//
//go:linkname region_removeRegion region.runtime_region_removeRegion
func region_removeRegion(region unsafe.Pointer) {
	((*userRegion)(region)).removeRegion()
}

// region_createChannel is a wrapper around (*userRegion).makeChan.
//
//go:linkname region_createChannel region.runtime_region_createChannel
func region_createChannel(region unsafe.Pointer, typ any, sz int) unsafe.Pointer {
	return unsafe.Pointer(((*userRegion)(region)).makeChan(abi.TypeOf(typ), sz))
}

const (
	// regionBlockBytes is the size of a user region block.
	regionBlockBytesMax = 8 << 20
	regionBlockBytes    = uintptr(int64(regionBlockBytesMax-heapArenaBytes)&(int64(regionBlockBytesMax-heapArenaBytes)>>63) + heapArenaBytes) // min(regionBlockBytesMax, heapArenaBytes)

	// regionBlockPages is the number of pages a user region block uses.
	regionBlockPages = regionBlockBytes / pageSize

	// regionBlockMaxAllocBytes is the maximum size of an object that can
	// be allocated from a region. This number is chosen to cap worst-case
	// fragmentation of user regions to 25%. Larger allocations are redirected
	// to the heap.
	regionBlockMaxAllocBytes = regionBlockBytes / 4
)

func init() {
	if regionBlockPages*pageSize != regionBlockBytes {
		throw("user region block size is not a multiple of the page size")
	}
	if regionBlockBytes%physPageSize != 0 {
		throw("user region block size is not a multiple of the physical page size")
	}
	if regionBlockBytes < heapArenaBytes {
		if heapArenaBytes%regionBlockBytes != 0 {
			throw("user region block size is smaller than a heap arena, but doesn't divide it")
		}
	} else {
		if regionBlockBytes%heapArenaBytes != 0 {
			throw("user arena block size is larger than a heap arena, but not a multiple")
		}
	}
}

type userRegion struct {
	// current is the user region block we're currently allocating into.
	current *mspan

	// fullList is a list of full blocks that have not enough free memory left, and
	// that we'll free once this region is freed.
	fullList *mspan

	// defunct is true if free has been called on this region.
	//
	// This is just a best-effort way to discover a concurrent allocation
	// and free. Also used to detect a double-free.
	defunct atomic.Bool

	// refs is a counter keeping track of the amount of goroutines that are currently referring to the region,
	// a region cannot be freed until the refs count is 0
	refs atomic.Int32

	// localFreeList is a list of blocks that previously has been freed
	localFreeList *concurrentFreeList

	// parent is the outer region this region is created by
	parent *userRegion

	// depth is the current depth of the inner region, used to allocate a proper inner size
	depth uintptr
}

type concurrentFreeList struct {
	head *mspan
	tail *mspan
}

func (l *concurrentFreeList) init() {
	l.head = nil
	l.tail = nil
}

func (l *concurrentFreeList) enqueue(s *mspan) {
	s.prev = nil
	s.next = l.head
	if l.head == nil {
		l.tail = s
	} else {
		l.head.prev = s
	}
	l.head = s
}

func (l *concurrentFreeList) dequeue() *mspan {
	if l.tail == nil {
		return nil
	}
	s := l.tail
	prev := s.prev
	if prev != nil {
		prev.next = nil
		l.tail = prev
	} else {
		l.head = nil
		l.tail = nil
	}
	s.prev = nil
	s.next = nil
	return s
}

func (l *concurrentFreeList) isEmpty() bool {
	return l.tail == nil
}

// createUserRegion creates a new userRegion ready to be used.
func createUserRegion() *userRegion {
	r := new(userRegion)
	SetFinalizer(r, func(r *userRegion) {
		r.removeRegion()
	})
	r.localFreeList = &concurrentFreeList{}
	r.current = newRegionBlock(r)
	r.refs.Store(1)
	r.parent = nil
	r.depth = 0
	return r
}

func newRegionBlock(r *userRegion) *mspan {
	var span *mspan
	if !r.localFreeList.isEmpty() {
		//TODO: Solve race, fetch s atomically (though there are no risk of race here since each local region isn't allocated concurrently)
		span = r.localFreeList.dequeue()

		print("Allocating a new region block from local list...\n")
		print(unsafe.Pointer(span.startAddr), " startAddr\n")
		print(unsafe.Pointer(span.limit), " limit\n")
	} else {
		systemstack(func() {
			span = mheap_.allocNewRegionBlock()
		})
	}
	if span == nil {
		throw("out of memory")
	}
	return span
}

func (h *mheap) allocNewRegionBlock() *mspan {
	var s *mspan
	var base uintptr

	lock(&h.lock)
	// Retrieves addres sfrom hint, thereafter bumps hint to ptr + regionBlockBytes
	v, size := h.sysAlloc(regionBlockBytes, &mheap_.userArena.arenaHints, &mheap_.userArenaArenas) // Allocates heap arena space, returns memory region in reserved state
	if size%regionBlockBytes != 0 {
		throw("sysAlloc size is not divisible by regionBlockBytes")
	}
	if size > regionBlockBytes {
		// We got more than we asked for. This can happen if
		// heapArenaSize > regionBlockBytes, or if sysAlloc just returns
		// some extra as a result of trying to find an aligned region.
		//TODO: Set these extra pages to the global free list
		/*
			for i := regionBlockBytes; i < size; i += regionBlockBytes {
				s := h.allocMSpanLocked()
				s.init(uintptr(v)+i, regionBlockPages)
				s.next = r.localFreeList
				r.localFreeList = s
			}*/
		size = regionBlockBytes
	}
	base = uintptr(v)
	if base == 0 {
		// Out of memory.
		unlock(&h.lock)
		return nil
	}
	s = h.allocMSpanLocked() // Allocates a mspan object, but h.lock must be held
	unlock(&h.lock)

	// sysAlloc returns Reserved address space, and any span we're
	// reusing is set to fault (so, also Reserved), so transition
	// it to Prepared and then Ready.
	sysMap(unsafe.Pointer(base), regionBlockBytes, &gcController.heapReleased)
	sysUsed(unsafe.Pointer(base), regionBlockBytes, regionBlockBytes)

	// Model the user region as a heap span for a large object.
	spc := makeSpanClass(0, true)
	h.initSpan(s, spanAllocHeap, spc, base, regionBlockPages)
	s.isUserArenaChunk = true
	s.limit = s.base() + s.elemsize

	setMemStatsAfterAlloc(s)

	// Clear the span preemptively. It's an region block, so let's assume
	// everything is going to be used.
	//
	// This also seems to make a massive difference as to whether or
	// not Linux decides to back this memory with transparent huge
	// pages. There's latency involved in this zeroing, but the hugepage
	// gains are almost always worth it. Note: it's important that we
	// clear even if it's freshly mapped and we know there's no point
	// to zeroing as *that* is the critical signal to use huge pages.
	memclrNoHeapPointers(unsafe.Pointer(s.base()), s.elemsize)
	s.needzero = 0

	s.freeIndexForScan = 1

	// Set up the range for allocation.
	s.userArenaChunkFree = makeAddrRange(base, base+s.elemsize)

	// The span is allocated in depth 0
	s.nestled = false

	return s
}

func setMemStatsAfterAlloc(s *mspan) {
	// Account for this new arena chunk memory.
	gcController.heapInUse.add(int64(s.npages * pageSize))
	gcController.heapReleased.add(-int64(s.npages * pageSize))

	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.inHeap, int64(s.npages*pageSize))
	atomic.Xaddint64(&stats.committed, int64(s.npages*pageSize))

	// Model the arena as a single large malloc.
	atomic.Xadd64(&stats.largeAlloc, int64(s.elemsize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(s.elemsize))

	// Update heapLive.
	gcController.update(int64(s.elemsize), 0)
}

// removeRegion frees the current region block, and the full blocks from fullList
func (r *userRegion) removeRegion() {
	// If more than 0 still references the region, return
	if r.refs.Add(-1) > 0 {
		return
	}
	if r.defunct.Load() {
		panic("region double free")
	}

	// Mark ourselves as defunct.
	r.defunct.Store(true)

	//TODO: How to handle inner region blocks? If we free the outer block, then we shouldn't free the inner one?
	// Fixed - see s.nestled

	// Free all the full region blocks.
	s := r.fullList
	for s != nil {
		r.fullList = s.next
		s.next = nil
		r.freeUserRegionBlock(s, unsafe.Pointer(s.base()))
		s = r.fullList
	}

	// Free the current region block.
	s = r.current
	if s != nil {
		r.freeUserRegionBlock(s, unsafe.Pointer(s.base()))
	}

	// Free the local free list, released by our child, to our parent
	if r.parent != nil {
		print("Freeing local-free list to parent...\n")
		s = r.localFreeList.dequeue()
		for s != nil {
			r.freeUserRegionBlock(s, unsafe.Pointer(s.base()))
			s = r.localFreeList.dequeue()
		}
	}

	// nil out r.current so that a race with freeing will more likely cause a crash.
	r.current = nil
	r = nil
}

// freeUserRegionBlock releases the user region represented by s back to the runtime.
//
// x must be a live pointer within s.
//
// The runtime will set the user block to fault once
func (r *userRegion) freeUserRegionBlock(s *mspan, x unsafe.Pointer) {
	if !s.isUserArenaChunk {
		throw("span is not for a region block")
	}

	// If the span is nestled it means that the memory will be deallocated from its outer span
	if s.nestled && r.depth == 0 {
		return
	}

	// Make ourselves non-preemptible as we manipulate state and statistics.
	//
	// Also required by freeUserRegionBlock.
	mp := acquirem()

	// If inner region, free pages to local free list

	if r.depth > 0 {
		base := s.base()
		// Clear memory
		memclrNoHeapPointers(unsafe.Pointer(base), s.elemsize)
		s.needzero = 0

		// Set up the range for allocation.
		s.userArenaChunkFree = makeAddrRange(base, s.limit)

		//TODO: solve race, what if goroutines referencing different inner regions wants to append the list at the same time?
		// ev. idea: lock-free queue
		r.parent.localFreeList.enqueue(s)
		print("Freeing memory (sending it to local-free list)...\n")
		print(unsafe.Pointer(s.startAddr), " startAddr\n")
		print(unsafe.Pointer(s.limit), " limit\n")
	} else {
		// Actually set the region block to fault, so we'll get dangling pointer errors.
		// sysFault currently uses a method on each OS that forces it to evacuate all
		// memory backing the chunk.
		sysFault(unsafe.Pointer(s.base()), s.npages*pageSize)

		//TODO: Add the page to the global free list of the heap

		setMemStatsAfterFree(s)
	}
	KeepAlive(x)
	releasem(mp)
}

func setMemStatsAfterFree(s *mspan) {
	// Everything on the list is counted as in-use, however sysFault transitions to
	// Reserved, not Prepared, so we skip updating heapFree or heapReleased and just
	// remove the memory from the total altogether; it's just address space now.
	gcController.heapInUse.add(-int64(s.npages * pageSize))

	gcController.totalFree.Add(int64(s.elemsize))

	// Update consistent stats to match.
	//
	// We're non-preemptible, so it's safe to update consistent stats (our P
	// won't change out from under us).
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.committed, -int64(s.npages*pageSize))
	atomic.Xaddint64(&stats.inHeap, -int64(s.npages*pageSize))
	atomic.Xadd64(&stats.largeFreeCount, 1)
	atomic.Xadd64(&stats.largeFree, int64(s.elemsize))
	memstats.heapStats.release()
}

// allocateFromRegion receives any type and casts it to a type that could be used for alloc
// returns a pointer to the created variable that has been allocated within the region
func (r *userRegion) allocateFromRegion(typ any) any {
	t := (*_type)(efaceOf(&typ).data)
	if t.Kind_&abi.KindMask != abi.Pointer {
		throw("allocateFromRegion: non-pointer type")
	}
	te := (*ptrtype)(unsafe.Pointer(t)).Elem
	x := r.alloc(te)
	var result any
	e := efaceOf(&result)
	e._type = t
	e.data = x
	return result
}

// alloc reserves space in the current block or calls newRegionBlock and reserves space
// in a new block.
func (r *userRegion) alloc(typ *_type) unsafe.Pointer {
	s := r.current
	x := s.bump(typ)
	// Try again, a result of the region block being full
	for x == nil {
		if s.userArenaChunkFree.size() > regionBlockMaxAllocBytes {
			throw("wasted too much memory in an region block")
		}
		// Fetch a new region block, and try to bump again
		r.current = newRegionBlock(r)
		x = r.current.bump(typ)

		if s.isUnused() {
			//TODO: If the block is unused, return it to local-list, must be called after newRegionBlock is called
			// otherwise it will be a forever loop of fetching a local page that doesn't fit
			// OBS: What if there are more than one page that is too small? Maybe this logic have to be outside the for-loop
			r.localFreeList.enqueue(s)
		} else {
			// Append the current mspan to the fullList
			s.next = r.fullList
			r.fullList = s
		}
	}
	return x
}

func (s *mspan) isUnused() bool {
	return s.base() == s.userArenaChunkFree.base.a
}

// bump reserves space in the user region for an item of the specified
// type.
func (s *mspan) bump(typ *_type) unsafe.Pointer {
	size := typ.Size_
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}
	if size > regionBlockBytes {
		// Redirect allocations that don't fit into a block well directly
		// from the heap.
		return newobject(typ)
	}

	print("Bumping the pointer within the region...\n")
	print(unsafe.Pointer(s.startAddr), " startAddr\n")
	print(unsafe.Pointer(s.userArenaChunkFree.base.a), " bumpPointer before\n")

	// Prevent preemption as we set up the space for a new object.
	//
	// Act like we're allocating.
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	var ptr unsafe.Pointer

	v, ok := s.userArenaChunkFree.takeFromFront(size, typ.Align_)
	if ok {
		ptr = unsafe.Pointer(v)
	}
	if ptr == nil {
		// Failed to allocate.
		mp.mallocing = 0
		releasem(mp)
		return nil
	}
	if s.needzero != 0 {
		throw("arena chunk needs zeroing, but should already be zeroed")
	}

	mp.mallocing = 0
	releasem(mp)

	print(unsafe.Pointer(s.userArenaChunkFree.base.a), " bumpPointer after\n")
	print(unsafe.Pointer(s.limit), " limit\n")

	return ptr
}

// makeChan generates a hchan that is completely stored in its separate region
//
// buffered channels value buffers are referenced using c.refs which are bumped to the region
// unbuffered channels is handled normally
// TODO: eventually sudog and g could be stored inside the region? Ensuring that receiver and sender queue aligns within
func (r *userRegion) makeChan(t *_type, size int) *hchan {
	elem := t

	// compiler checks this but be safe.
	if elem.Size_ >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.Align_ > maxAlign {
		throw("makechan: bad alignment")
	}

	mem, overflow := math.MulUintptr(elem.Size_, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	//TODO: return the rest of the memory to the global free list, memory that we don't use
	var c *hchan
	switch {
	case mem == 0:
		//unbuffered channel
		c = (*hchan)(r.alloc(abi.TypeOf(hchan{})))
		c.buf = r.alloc(elem)
	default:
		//buffered channel
		c = (*hchan)(r.alloc(abi.TypeOf(hchan{})))
		for i := 0; i < size; i++ {
			c.refs = append(c.refs, r.alloc(elem))
		}
	}
	c.elemsize = uint16(elem.Size_)
	c.elemtype = elem
	c.dataqsiz = uint(size)
	c.isregionblock = true
	c.regionblock = r //could be removed, usage case was only if we wanted to bump sudog and g
	lockInit(&c.lock, lockRankHchan)

	return c
}

// incrementCounter is used by goroutines that needs a reference of the region, ensuring that its content aren't
// removed until the goroutine has finished its execution
//
// returns false if the region block is already freed, should therefore be called before a goroutine starts execution
func (r *userRegion) incrementCounter() bool {
	if r.refs.Add(1) > 1 {
		return true
	}
	return false
}

const (
	smallInnerRegionBlockBytes  = regionBlockBytes / 64
	mediumInnerRegionBlockBytes = regionBlockBytes / 16
	largeInnerRegionBlockBytes  = regionBlockBytes / 4
)

// allocateInnerRegion returns a userRegion within the parent region with a predetermined size depending on
// the depth of the child region
func (r *userRegion) allocateInnerRegion() *userRegion {
	inner := (*userRegion)(r.alloc(abi.TypeOf(userRegion{})))
	inner.parent = r
	inner.localFreeList = &concurrentFreeList{}
	inner.depth = r.depth + 1
	var typ *_type
	// Allocate different amount of bytes to the inner region, depending on it's depth
	switch inner.depth {
	case 1:
		typ = abi.TypeOf([largeInnerRegionBlockBytes]byte{})
	case 2:
		typ = abi.TypeOf([mediumInnerRegionBlockBytes]byte{})
	default:
		typ = abi.TypeOf([smallInnerRegionBlockBytes]byte{})
	}
	systemstack(func() {
		inner.current = r.newInnerRegionBlock(typ)
	})
	//TODO: Handle goroutines that references the inner region, what if the outer removes the inner one?
	// ev. idea: increment the parent.refs, the parent can't remove the region until the inner is removed, double-check
	inner.refs.Store(1)

	return inner
}

// newInnerRegionBlock generates a mspan in the parent region block with a predetermined size
// this block is used by the inner region to store data
func (r *userRegion) newInnerRegionBlock(typ *_type) *mspan {
	base := r.current.userArenaChunkFree.base.a
	r.alloc(typ)
	limit := r.current.userArenaChunkFree.base.a
	lock(&mheap_.lock)
	s := mheap_.allocMSpanLocked()
	unlock(&mheap_.lock)

	rng := makeAddrRange(base, limit)
	nbytes := rng.size()

	s.init(base, nbytes/pageSize)
	s.isUserArenaChunk = true
	s.userArenaChunkFree = rng
	s.limit = limit
	s.needzero = 0   // should already be zeroed by the parent region
	s.nestled = true // the span is allocated at a depth > 0
	return s
}

// TODO: Create a method that returns the unused memory to the global free list, or local free list if inner region
