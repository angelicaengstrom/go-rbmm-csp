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
func region_createRegion(sz int) unsafe.Pointer {
	return unsafe.Pointer(createUserRegion(uintptr(sz)))
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
func region_removeRegion(region unsafe.Pointer) int {
	return ((*userRegion)(region)).removeRegion()
}

// region_createChannel is a wrapper around (*userRegion).makeChan.
//
//go:linkname region_createChannel region.runtime_region_createChannel
func region_createChannel(region unsafe.Pointer, typ any, sz int) unsafe.Pointer {
	if sz < 0 {
		panic("createChannel: negative buffer-size")
	}
	t := (*_type)(efaceOf(&typ).data)
	if t.Kind_&abi.KindMask != abi.Pointer {
		throw("createChannel: non-pointer type")
	}
	t = (*ptrtype)(unsafe.Pointer(t)).Elem
	return unsafe.Pointer(((*userRegion)(region)).makeChan(t, sz))
}

// region_allocNestledRegion is a wrapper around (*userRegion).allocateInnerRegion.
//
//go:linkname region_allocNestledRegion region.runtime_region_allocNestledRegion
func region_allocNestledRegion(region unsafe.Pointer, sz int) unsafe.Pointer {
	return unsafe.Pointer((*userRegion)(region).allocateInnerRegion(sz))
}

//go:linkname region_incRefCounter region.runtime_region_incRefCounter
func region_incRefCounter(region unsafe.Pointer) bool {
	return (*userRegion)(region).incrementCounter()
}

//go:linkname region_decRefCounter region.runtime_region_decRefCounter
func region_decRefCounter(region unsafe.Pointer) int {
	return (*userRegion)(region).decrementCounter()
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
	// current is the region page we're currently allocating into.
	current *mspan

	// fullList is a list of full pages that have not enough free memory left
	fullList *mspan

	// refs is a counter keeping track of the amount of goroutines that are currently referring to the region,
	// a region cannot be freed until the refs count is 0
	refs atomic.Int32

	// localFreeList is a list of nestled region pages that previously has been freed
	localFreeList *concurrentFreeList[*mspan]

	// parent is the outer region this region is nestled onto
	// nil if the region is non-nestled
	parent *userRegion

	// depth is the current depth of the inner region, used to allocate a proper inner size
	// 0 if the region is non-nestled
	depth uintptr

	lock mutex
}

type WaitGroup struct {
	counter atomic.Int32
}

func (wg *WaitGroup) Add(delta int) {
	wg.counter.Add(int32(delta))
}

func (wg *WaitGroup) Done() {
	wg.counter.Add(-1)
}

func (wg *WaitGroup) Wait() {
	for wg.counter.Load() > 0 {
		// Simulate waiting
	}
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
					var empty T
					return empty
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
func createUserRegion(sz uintptr) *userRegion {
	r := new(userRegion)
	r.localFreeList = &concurrentFreeList[*mspan]{}
	r.localFreeList.init()
	r.current = newRegionBlock(r, sz)
	r.refs.Store(1)
	r.parent = nil
	r.depth = 0
	return r
}

// newRegionBlock returns a new region block, either from the local free-list or as a large block from the heap
func newRegionBlock(r *userRegion, sz uintptr) *mspan {
	var span *mspan

	// If inner region block has released its memory, fetch these first from the local-free list
	span = r.localFreeList.dequeue()
	if span != nil {
		base := span.base()

		mheap_.initRegionBlock(span, base, span.npages)
		setMemStatsAfterAlloc(span, 0)

		// Pages from the local free list should not be returned to the global free list since they are not the first
		// layer of the region
		span.nestled = true
	} else {
		// If there are no memory in the local-free list, request memory from the heap
		systemstack(func() {
			span = mheap_.allocNewRegionBlock(sz)

			nbytes := span.elemsize
			base := span.base()

			for i := nbytes; i < regionBlockBytes-nbytes; i += nbytes {
				lock(&mheap_.lock)
				s := mheap_.allocMSpanLocked()
				unlock(&mheap_.lock)

				s.init(base+i, span.npages)
				r.localFreeList.enqueue(s)
			}
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
func (h *mheap) allocNewRegionBlock(sz uintptr) *mspan {
	var s *mspan
	var base uintptr
	npages := regionBlockPages
	created := 0

	//If the global-free list is non-empty, take those first instead of calling sysAlloc
	s = h.userArena.globalFreeList.dequeue()
	if s != nil {
		base = s.base()
		npages = s.npages
	} else { // If there were no value to be found in the global-free list, alloc space
		lock(&h.lock)
		// Allocates heap arena space, returns memory region in reserved state
		v, size := h.sysAlloc(regionBlockBytes, &mheap_.userArena.arenaHints, &mheap_.userArenaArenas)
		unlock(&h.lock)
		created++

		if size%regionBlockBytes != 0 {
			throw("sysAlloc size is not divisible by regionBlockBytes")
		}
		if size > regionBlockBytes {
			// We got more than we asked for. This can happen if
			// heapArenaSize > regionBlockBytes, or if sysAlloc just returns
			// some extra as a result of trying to find an aligned region.

			for i := regionBlockBytes; i < size; i += regionBlockBytes {
				lock(&h.lock)
				s := h.allocMSpanLocked()
				unlock(&h.lock)
				s.init(uintptr(v)+i, regionBlockPages)
				created++
				h.userArena.globalFreeList.enqueue(s)
			}
		}
		base = uintptr(v)
		if base == 0 {
			// Out of memory.
			return nil
		}

		lock(&h.lock)
		// Allocates a mspan object, but h.lock must be held
		s = h.allocMSpanLocked()
		s.npages = regionBlockPages
		unlock(&h.lock)
	}

	if sz < s.npages<<pageShift {
		npages = sz >> pageShift
		if npages == 0 || sz&(pageSize-1) != 0 {
			npages++
		}
	}

	h.initRegionBlock(s, base, npages)
	setMemStatsAfterAlloc(s, created)

	// Pages that are requested directly from the heap should be returned to the global-free list, not the local
	// since it will be the first layer of region
	s.nestled = false

	return s
}

func (h *mheap) initRegionBlock(s *mspan, base uintptr, npages uintptr) *mspan {
	// sysAlloc returns Reserved address space, and any span we're
	// reusing is set to fault (so, also Reserved), so transition
	// it to Prepared and then Ready.
	nbytes := npages << pageShift
	sysMap(unsafe.Pointer(base), nbytes, &gcController.heapReleased)
	sysUsed(unsafe.Pointer(base), nbytes, nbytes)

	// Model the user region as a heap span for a large object.
	spc := makeSpanClass(0, true)
	h.initSpan(s, spanAllocHeap, spc, base, npages)
	s.isUserArenaChunk = true
	s.limit = s.base() + nbytes

	// Clear the span preemptively. It's an region block, so let's assume
	// everything is going to be used.
	//
	// This also seems to make a massive difference as to whether or
	// not Linux decides to back this memory with transparent huge
	// pages. There's latency involved in this zeroing, but the hugepage
	// gains are almost always worth it. Note: it's important that we
	// clear even if it's freshly mapped and we know there's no point
	// to zeroing as *that* is the critical signal to use huge pages.
	memclrNoHeapPointers(unsafe.Pointer(s.base()), nbytes)

	s.needzero = 0

	s.freeIndexForScan = 1

	// Set up the range for allocation.
	s.userArenaChunkFree = makeAddrRange(base, base+s.elemsize)

	return s
}

// setMemStatsAfterAlloc updates the memstats with the new region block that has been allocated on the heap
func setMemStatsAfterAlloc(s *mspan, created int) {
	nbytes := s.npages << pageShift
	// Account for this new arena chunk memory.
	gcController.heapInUse.add(int64(nbytes))
	gcController.heapReleased.add(-int64(nbytes))

	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.inHeap, int64(nbytes))
	atomic.Xaddint64(&stats.committed, int64(nbytes))

	// Model the arena as a single large malloc.
	atomic.Xadd64(&stats.largeAlloc, int64(s.elemsize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	atomic.Xadd64(&stats.regionAlloc, int64(s.elemsize))
	atomic.Xadd64(&stats.regionCreated, int64(created))
	atomic.Xadd64(&stats.regionReuse, 1)
	atomic.Xadd64(&stats.regionIntFrag, int64(s.elemsize))
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(s.elemsize))

	// Update heapLive.
	gcController.update(int64(s.elemsize), 0)
}

// removeRegion is used by goroutines that no longer needs a reference of the region, ensuring that its content are
// freed if no goroutine references it anymore
func (r *userRegion) removeRegion() int {
	refs := r.refs.Add(-1)
	if refs == 0 {
		r.freeUserRegion()
		return 0
	} else if refs < 0 {
		panic("region double free")
	}
	return int(refs)
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

	// Make ourselves non-preemptible as we manipulate state and statistics.
	//
	// Also required by freeUserRegionBlock.
	mp := acquirem()

	base := s.base()

	// Actually set the region block to fault, so we'll get dangling pointer errors.
	// sysFault currently uses a method on each OS that forces it to evacuate all
	// memory backing the chunk.

	//sysFault(unsafe.Pointer(base), s.elemsize)
	sysUnused(unsafe.Pointer(base), s.elemsize)

	setMemStatsAfterFree(s)

	mheap_.pagesInUse.Add(-s.npages)
	s.state.set(mSpanDead)

	// Add the whole region-page to the global free list of the heap
	if !s.nestled {
		s.npages = regionBlockPages
		mheap_.userArena.globalFreeList.enqueue(s)
	} else if r.parent != nil {
		// If not outer region, free pages to local free list to the parent region
		r.parent.localFreeList.enqueue(s)
	}

	KeepAlive(x)
	releasem(mp)
}

// setMemStatsAfterAlloc updates the memstats after freeing of region block on the heap
func setMemStatsAfterFree(s *mspan) {
	// Everything on the list is counted as in-use, however sysFault transitions to
	// Reserved, not Prepared, so we skip updating heapFree or heapReleased and just
	// remove the memory from the total altogether; it's just address space now.
	nbytes := int64(s.npages << pageShift)
	gcController.heapInUse.add(-nbytes)

	gcController.totalFree.Add(int64(s.elemsize))

	// Update consistent stats to match.
	//
	// We're non-preemptible, so it's safe to update consistent stats (our P
	// won't change out from under us).
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.committed, -nbytes)
	atomic.Xaddint64(&stats.inHeap, -nbytes)
	atomic.Xadd64(&stats.largeFreeCount, 1)
	atomic.Xadd64(&stats.largeFree, int64(s.elemsize))
	atomic.Xadd64(&stats.regionDealloc, int64(s.elemsize))

	atomic.Xadd64(&stats.regionIntFrag, -int64(s.limit-s.userArenaChunkFree.base.a))
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

	lock(&r.lock)
	x := r.alloc(te)
	unlock(&r.lock)

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

	var unusedList *mspan
	// Try again, a result of the region block being full
	for x == nil {
		if s.userArenaChunkFree.size() > regionBlockMaxAllocBytes {
			throw("wasted too much memory in an region block")
		}
		// If the block is unused, don't append it to fullList, return it to the local or global list
		if s.isUnused() {
			s.next = unusedList
			unusedList = s
		} else { // Append the used block to the fullList
			r.current.next = r.fullList
			r.fullList = r.current
		}
		s = r.newRegionBlock(typ.Size_)
		x = s.bump(typ)
	}
	r.current = s

	// The unused blocks are returned afterward, otherwise we might iterate and fetch the same block again
	for unusedList != nil {
		// Currently the unused block will be placed on the parents local freelist not its own
		r.freeUserRegionBlock(s, unsafe.Pointer(unusedList.base()))
		unusedList = unusedList.next
	}

	return x
}

func (r *userRegion) newRegionBlock(sz uintptr) *mspan {
	var s *mspan
	if sz < r.current.elemsize {
		sz = r.current.elemsize
	} else {
		sz = regionBlockMaxAllocBytes
	}
	// Fetch a new region block, and try to bump again
	if r.parent != nil && sz < regionBlockMaxAllocBytes {
		b := &abi.Type{Size_: sz, Align_: 1, Kind_: abi.Array}
		systemstack(func() {
			s = r.parent.newInnerRegionBlock(b)
		})
	} else {
		s = newRegionBlock(r, sz)
	}
	return s
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
	if size > regionBlockMaxAllocBytes {
		// Redirect allocations that don't fit into a block well directly
		// from the heap.
		return newobject(typ)
	}

	// Prevent preemption as we set up the space for a new object.
	//
	// Act like we're allocating.
	mp := acquirem()
	if doubleCheckMalloc {
		if mp.mallocing != 0 {
			throw("malloc deadlock")
		}
		if mp.gsignal == getg() {
			throw("malloc during signal")
		}
	}
	mp.mallocing = 1

	var ptr unsafe.Pointer

	v, ok := s.userArenaChunkFree.takeFromFront(size, typ.Align_)
	if ok {
		ptr = unsafe.Pointer(v)
		// Update the internal fragmentation of the region block
		stats := memstats.heapStats.acquire()
		atomic.Xadd64(&stats.regionIntFrag, -int64(s.userArenaChunkFree.base.a-v))
		memstats.heapStats.release()
	}
	mp.mallocing = 0
	releasem(mp)

	if ptr == nil {
		// Failed to allocate.
		return nil
	}
	if s.needzero != 0 {
		throw("region block needs zeroing, but should already be zeroed")
	}

	return ptr
}

// makeChan generates a hchan that is completely stored in its separate region
//
// buffered channels value buffers are referenced using c.refs which are bumped to the region
// unbuffered channels is handled normally
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
	if overflow || mem > regionBlockMaxAllocBytes || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	lock(&r.lock)
	c := (*hchan)(r.alloc(abi.TypeOf(hchan{})))
	switch {
	case mem == 0:
		//unbuffered channel
		c.buf = r.alloc(elem)
	default:
		//buffered channel
		b := &abi.Type{Size_: mem, Align_: 1, Kind_: abi.Array}
		c.buf = r.alloc(b)
	}
	unlock(&r.lock)

	c.elemsize = uint16(elem.Size_)
	c.elemtype = elem
	c.dataqsiz = uint(size)
	c.isregionblock = true
	lockInit(&c.lock, lockRankHchan)

	// Since the entire buffer is already allocated, it is ok to free here
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
func (r *userRegion) decrementCounter() int {
	refs := r.refs.Add(-1)
	parent := r.parent
	if refs == 0 { // No goroutine is accessing the memory
		r.freeUserRegion()
	} else if refs < 0 {
		panic("region double free")
	}
	if parent != nil {
		return r.parent.decrementCounter()
	} else {
		return int(refs)
	}
}

// allocateInnerRegion returns a userRegion within the parent region with a predetermined size depending on
// the depth of the child region
func (r *userRegion) allocateInnerRegion(sz int) *userRegion {
	npages := sz >> pageShift

	// Round up the number of pages used in this block
	if npages == 0 || sz&(pageSize-1) != 0 {
		npages++
	}

	sz = npages << pageShift

	lock(&r.lock)
	if sz > int(regionBlockMaxAllocBytes) || sz > int(r.current.elemsize) {
		panic("Inner region too large for outer region block")
	} else if sz <= 0 {
		panic("Inner region too small for outer region block")
	}

	// Initialize the inner region
	inner := (*userRegion)(r.alloc(abi.TypeOf(userRegion{})))
	unlock(&r.lock)

	inner.parent = r
	inner.localFreeList = &concurrentFreeList[*mspan]{}
	inner.localFreeList.init()
	inner.depth = r.depth + 1
	inner.refs.Store(1)

	b := &abi.Type{Size_: uintptr(sz), Align_: 1, Kind_: abi.Array}

	systemstack(func() {
		inner.current = r.newInnerRegionBlock(b)
	})

	return inner
}

// newInnerRegionBlock generates a mspan in the parent region block with a predetermined size.
// This block is used by the inner region to store data
func (r *userRegion) newInnerRegionBlock(block *_type) *mspan {
	// Allocates a block to the outer region with a predetermined size
	lock(&r.lock)
	base := r.alloc(block)

	limit := r.current.userArenaChunkFree.base.a
	unlock(&r.lock)

	// The range of the inner region is the bounds before and after allocation on the parent
	s := r.generateSpanFromRange(uintptr(base), limit)
	s.nestled = true // the span is allocated at a depth > 0, therefore it is nestled

	/*
		stats := memstats.heapStats.acquire()
		atomic.Xadd64(&stats.regionIntFrag, int64(s.elemsize))
		memstats.heapStats.release()*/

	return s
}

// generateSpanFromRange generates a region block between base and limit
//
// The range between base and limit has to be divisible by pageSize
func (r *userRegion) generateSpanFromRange(base uintptr, limit uintptr) *mspan {
	// Generate a new address range based on base and limit
	rng := makeAddrRange(base, limit)
	nbytes := rng.size() // The number of bytes that the range contains
	if nbytes&(pageSize-1) != 0 {
		panic("Number of bytes in region aren't divisible by pageSize, ")
	}

	npages := nbytes >> pageShift

	lock(&mheap_.lock)
	s := mheap_.allocMSpanLocked()
	unlock(&mheap_.lock)

	// Initialize the region block
	s.init(base, npages)
	s.isUserArenaChunk = true
	s.userArenaChunkFree = rng
	s.elemsize = nbytes
	s.limit = limit
	s.needzero = 0 // Should already have been zeroed by the outer region

	return s
}
