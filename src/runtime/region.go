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

	// refs is a counter keeping track of the amount of goroutines that are currently referring to the region,
	// a region cannot be freed until the refs count is 0
	refs atomic.Int32

	// localFreeList is a list of blocks that previously has been freed
	localFreeList *concurrentFreeList[*mspan]

	// parent is the outer region this region is created by
	parent *userRegion

	// depth is the current depth of the inner region, used to allocate a proper inner size
	depth uintptr
}

type node[T any] struct {
	s    T
	next atomic.Pointer[node[T]]
}

type concurrentFreeList[T any] struct {
	head atomic.Pointer[node[T]]
	tail atomic.Pointer[node[T]]
}

func (l *concurrentFreeList[T]) init() {
	l.head.Store(&node[T]{})
	l.tail.Store(l.head.Load())
}

func (l *concurrentFreeList[T]) enqueue(elem T) {
	// New value to be enqueued
	n := &node[T]{s: elem}
	for {
		last := l.tail.Load()    //Reads tail
		next := last.next.Load() //Finds the node that appears to be last
		if last == l.tail.Load() {
			// Verify that node is indeed last, checks whether that node has a successor
			if next == nil {
				// If so, appends the new n
				if last.next.CompareAndSwap(next, n) {
					// Changes the queues tail field from the prior last n to the current last n
					l.tail.CompareAndSwap(last, n)
					// If this fails, the thread can return successfully because the call fails only if some other
					// thread helps it by advancing tail
					return
				}
			} else { // If the tail node has a successor, the method tries to help other threads
				// Advances tail to refer directly to the successor before trying again to insert its own node
				l.tail.CompareAndSwap(last, next)
			}
		}
	}
}

func (l *concurrentFreeList[T]) dequeue() T {
	for {
		first := l.head.Load()    // Reads head
		last := l.tail.Load()     // Reads tail
		next := first.next.Load() // Reads sentinel node
		if first == l.head.Load() {
			if first == last {
				if next == nil {
					panic("Queue is empty")
				}
				// If head equals tail and the next node isn't nil, then the tail is lagging behind
				// As in the enqueue() method, this attempts to help make tail consistent by swinging it
				// to the sentinel node's successor
				l.tail.CompareAndSwap(last, next)
			} else {
				// The value is read from the successor of the sentinel node
				s := next.s
				// Updates head to remove sentinel
				if l.head.CompareAndSwap(first, next) {
					return s
				}
			}
		}
	}
}

func (l *concurrentFreeList[T]) isEmpty() bool {
	return l.head.Load() == l.tail.Load()
}

// createUserRegion creates a new userRegion ready to be used.
//
// This method is only used for regions at depth 0
func createUserRegion() *userRegion {
	r := new(userRegion)
	SetFinalizer(r, func(r *userRegion) {
		r.removeRegion()
	})
	r.localFreeList = &concurrentFreeList[*mspan]{}
	r.localFreeList.init()
	r.current = newRegionBlock(r)
	r.refs.Store(1)
	r.parent = nil
	r.depth = 0
	return r
}

// newRegionBlock returns a new region block, either from the local free-list or as a large block from the heap
func newRegionBlock(r *userRegion) *mspan {
	var span *mspan

	// If inner region block has released its memory, fetch these first from the local-free list
	if !r.localFreeList.isEmpty() {
		span = r.localFreeList.dequeue()
	} else {
		// If there are no memory in the local-free list, request memory from the heap
		systemstack(func() {
			span = mheap_.allocNewRegionBlock()
		})
		if span == nil {
			throw("out of memory")
		}
	}
	return span
}

// allocNewRegionBlock allocates a region block from the heap, either from the global free-list or by
// sysAlloc.
//
// returns a ready-to-use mspan with cleaned memory
func (h *mheap) allocNewRegionBlock() *mspan {
	var s *mspan
	var base uintptr

	//If the global-free list is non-empty, take those first instead of calling sysAlloc
	//TODO: Check the global free list before calling sysAlloc
	lock(&h.lock)
	// Retrieves address from hint, thereafter bumps hint to ptr + regionBlockBytes
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
		} */
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

	// Pages that are requested directly from the heap should be returned to the global-free list, not the local
	// since it will be the first layer of region
	s.nestled = false

	return s
}

// setMemStatsAfterAlloc updates the memstats with the new region block that has been allocated on the heap
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

// removeRegion is used by goroutines that no longer needs a reference of the region, ensuring that its content are
// freed if no goroutine references it anymore
func (r *userRegion) removeRegion() {
	refs := r.refs.Add(-1)
	if refs == 0 {
		r.freeUserRegion()
	} else if refs < 0 {
		panic("region double free, ")
	}
}

// freeUserRegion frees the current region block, and the full blocks from fullList
//
// Note: We don't have to free our localFreeList that was freed from our child to our parent
// because these inner blocks are already considered as taken as current or in fullList
func (r *userRegion) freeUserRegion() {
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

	// If not outer region, free pages to local free list to the parent region
	if r.depth > 0 {
		base := s.base()
		// Clear memory
		memclrNoHeapPointers(unsafe.Pointer(base), s.elemsize)
		s.needzero = 0

		// Set up the range for allocation.
		s.userArenaChunkFree = makeAddrRange(base, s.limit)

		// enqueue the span to the localFreeList
		r.parent.localFreeList.enqueue(s)
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

// setMemStatsAfterAlloc updates the memstats after freeing of region block on the heap
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
		s = newRegionBlock(r)
		x = s.bump(typ)

		// If the block is unused, free the memory
		if s.isUnused() {
			r.freeUserRegionBlock(s, unsafe.Pointer(s.base()))
		} else {
			s.next = r.fullList
			r.fullList = s
		}
	}
	r.current = s

	return x
}

// isUnused returns true if the base address it the same as the bump-pointer of the region block
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
//
// The method also handle references of nestled regions, incrementing the outer regions counter to prevent
// removal of the inner region when being referenced. Only returns true if all parent blocks aren't already freed.
// Each incrementCounter needs a corresponding decrementCounter
func (r *userRegion) incrementCounter() bool {
	refs := r.refs.Add(1)
	if refs > 1 {
		if r.parent == nil {
			return true
		}
		return r.parent.incrementCounter()
	}
	return false
}

// decrementCounter is used by goroutines that no longer needs a reference of the region, ensuring that its content are
// freed if no goroutine references it anymore
//
// The method also handle references of nestled regions, decrementing the outer regions counter.
// Each incrementCounter needs a corresponding decrementCounter
func (r *userRegion) decrementCounter() {
	refs := r.refs.Add(-1)
	parent := r.parent
	if refs == 0 { // No goroutine is accessing the memory
		r.freeUserRegion()
	} else if refs < 0 {
		// If lesser than 0 goroutines references the region, this method must have been called twice
		print("region double free, ", refs)
	}
	if parent != nil {
		r.parent.decrementCounter()
	}
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
	inner.localFreeList = &concurrentFreeList[*mspan]{}
	inner.localFreeList.init()
	inner.depth = r.depth + 1
	inner.refs.Store(1)
	var b *_type
	// Allocate different amount of bytes to the inner region block, depending on it's depth
	switch inner.depth {
	case 1:
		b = abi.TypeOf([largeInnerRegionBlockBytes]byte{})
	case 2:
		b = abi.TypeOf([mediumInnerRegionBlockBytes]byte{})
	default:
		b = abi.TypeOf([smallInnerRegionBlockBytes]byte{})
	}
	systemstack(func() {
		inner.current = r.newInnerRegionBlock(b)
	})

	return inner
}

// newInnerRegionBlock generates a mspan in the parent region block with a predetermined size
// this block is used by the inner region to store data
func (r *userRegion) newInnerRegionBlock(block *_type) *mspan {
	// Allocates a block to the outer region with a predetermined size
	base := r.current.userArenaChunkFree.base.a
	r.alloc(block)
	limit := r.current.userArenaChunkFree.base.a
	lock(&mheap_.lock)
	s := mheap_.allocMSpanLocked()
	unlock(&mheap_.lock)

	// The range of the inner region is the bounds before and after allocation on the parent
	rng := makeAddrRange(base, limit)
	nbytes := rng.size()

	s.init(base, nbytes/pageSize)
	s.isUserArenaChunk = true
	s.userArenaChunkFree = rng
	s.limit = limit
	s.needzero = 0   // should already have been zeroed by the parent region
	s.nestled = true // the span is allocated at a depth > 0, therefore it is nestled
	return s
}

// TODO: Create a method that returns the unused memory to the global free list, or local free list if inner region
