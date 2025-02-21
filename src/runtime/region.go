// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Implementation of (safe) user arenas.
//
// This file contains the implementation of user arenas wherein Go values can
// be manually allocated and freed in bulk. The act of manually freeing memory,
// potentially before a GC cycle, means that a garbage collection cycle can be
// delayed, improving efficiency by reducing GC cycle frequency. There are other
// potential efficiency benefits, such as improved locality and access to a more
// efficient allocation strategy.
//
// What makes the arenas here safe is that once they are freed, accessing the
// arena's memory will cause an explicit program fault, and the arena's address
// space will not be reused until no more pointers into it are found. There's one
// exception to this: if an arena allocated memory that isn't exhausted, it's placed
// back into a pool for reuse. This means that a crash is not always guaranteed.
//
// While this may seem unsafe, it still prevents memory corruption, and is in fact
// necessary in order to make new(T) a valid implementation of arenas. Such a property
// is desirable to allow for a trivial implementation. (It also avoids complexities
// that arise from synchronization with the GC when trying to set the arena chunks to
// fault while the GC is active.)
//
// The implementation works in layers. At the bottom, arenas are managed in chunks.
// Each chunk must be a multiple of the heap arena size, or the heap arena size must
// be divisible by the arena chunks. The address space for each chunk, and each
// corresponding heapArena for that address space, are eternally reserved for use as
// arena chunks. That is, they can never be used for the general heap. Each chunk
// is also represented by a single mspan, and is modeled as a single large heap
// allocation. It must be, because each chunk contains ordinary Go values that may
// point into the heap, so it must be scanned just like any other object. Any
// pointer into a chunk will therefore always cause the whole chunk to be scanned
// while its corresponding arena is still live.
//
// Chunks may be allocated either from new memory mapped by the OS on our behalf,
// or by reusing old freed chunks. When chunks are freed, their underlying memory
// is returned to the OS, set to fault on access, and may not be reused until the
// program doesn't point into the chunk anymore (the code refers to this state as
// "quarantined"), a property checked by the GC.
//
// The sweeper handles moving chunks out of this quarantine state to be ready for
// reuse. When the chunk is placed into the quarantine state, its corresponding
// span is marked as noscan so that the GC doesn't try to scan memory that would
// cause a fault.
//
// At the next layer are the user arenas themselves. They consist of a single
// active chunk which new Go values are bump-allocated into and a list of chunks
// that were exhausted when allocating into the arena. Once the arena is freed,
// it frees all full chunks it references, and places the active one onto a reuse
// list for a future arena to use. Each arena keeps its list of referenced chunks
// explicitly live until it is freed. Each user arena also maps to an object which
// has a finalizer attached that ensures the arena's chunks are all freed even if
// the arena itself is never explicitly freed.
//
// Pointer-ful memory is bump-allocated from low addresses to high addresses in each
// chunk, while pointer-free memory is bump-allocated from high address to low
// addresses. The reason for this is to take advantage of a GC optimization wherein
// the GC will stop scanning an object when there are no more pointers in it, which
// also allows us to elide clearing the heap bitmap for pointer-free Go values
// allocated into arenas.
//
// Note that arenas are not safe to use concurrently.
//
// In summary, there are 2 resources: arenas, and arena chunks. They exist in the
// following lifecycle:
//
// (1) A new arena is created via newArena.
// (2) Chunks are allocated to hold memory allocated into the arena with new or slice.
//    (a) Chunks are first allocated from the reuse list of partially-used chunks.
//    (b) If there are no such chunks, then chunks on the ready list are taken.
//    (c) Failing all the above, memory for a new chunk is mapped.
// (3) The arena is freed, or all references to it are dropped, triggering its finalizer.
//    (a) If the GC is not active, exhausted chunks are set to fault and placed on a
//        quarantine list.
//    (b) If the GC is active, exhausted chunks are placed on a fault list and will
//        go through step (a) at a later point in time.
//    (c) Any remaining partially-used chunk is placed on a reuse list.
// (4) Once no more pointers are found into quarantined arena chunks, the sweeper
//     takes these chunks out of quarantine and places them on the ready list.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Functions starting with arena_ are meant to be exported to downstream users
// of arenas. They should wrap these functions in a higher-lever API.
//
// The underlying arena and its resources are managed through an opaque unsafe.Pointer.

// region_createRegion is a wrapper around newUserArena.
//
//go:linkname region_createRegion region.runtime_region_createRegion
func region_createRegion() unsafe.Pointer {
	return unsafe.Pointer(newUserArena())
}

// region_allocFromRegion is a wrapper around (*userArena).new, except that typ
// is an any (must be a *_type, still) and typ must be a type descriptor
// for a pointer to the type to actually be allocated, i.e. pass a *T
// to allocate a T. This is necessary because this function returns a *T.
//
//go:linkname region_allocFromRegion region.runtime_region_allocFromRegion
func region_allocFromRegion(region unsafe.Pointer, typ any) any {
	return typ
}

// region_removeRegion is a wrapper around (*userArena).free.
//
//go:linkname region_removeRegion region.runtime_region_removeRegion
func region_removeRegion(region unsafe.Pointer) {
	((*userRegion)(region)).removeRegion()
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
		throw("user arena chunk size is not a multiple of the page size")
	}
	if regionBlockBytes%physPageSize != 0 {
		throw("user arena chunk size is not a multiple of the physical page size")
	}
	if regionBlockBytes < heapArenaBytes {
		if heapArenaBytes%regionBlockBytes != 0 {
			throw("user arena chunk size is smaller than a heap arena, but doesn't divide it")
		}
	} else {
		if regionBlockBytes%heapArenaBytes != 0 {
			throw("user arena chunks size is larger than a heap arena, but not a multiple")
		}
	}
}

type userRegion struct {
	// current is the user region block we're currently allocating into.
	current *mspan

	// defunct is true if free has been called on this region.
	//
	// This is just a best-effort way to discover a concurrent allocation
	// and free. Also used to detect a double-free.
	defunct atomic.Bool
}

// createUserRegion creates a new userRegion ready to be used.
func createUserRegion() *userRegion {
	setGCPercent(-1)
	r := new(userRegion)
	SetFinalizer(r, func(r *userRegion) {
		// If region handle is dropped without being freed, then call
		// free on the region, so the region chunks are never reclaimed
		// by the garbage collector.
		r.removeRegion()
	})
	r.current = newRegionBlock()

	return r
}

func newRegionBlock() *mspan {
	var span *mspan
	systemstack(func() {
		span = mheap_.allocRegionBlock()
	})
	if span == nil {
		throw("out of memory")
	}
	return span
}

func (h *mheap) allocRegionBlock() *mspan {
	var s *mspan
	var base uintptr

	lock(&h.lock)
	v, size := h.sysAlloc(regionBlockBytes, &mheap_.userArena.arenaHints, &mheap_.userArenaArenas) // Allocates heap arena space, returns memory region in reserved state
	if size%regionBlockBytes != 0 {
		throw("sysAlloc size is not divisible by userArenaChunkBytes")
	}
	if size > regionBlockBytes {
		// We got more than we asked for. This can happen if
		// heapArenaSize > regionBlockBytes, or if sysAlloc just returns
		// some extra as a result of trying to find an aligned region.
		//
		// TODO: Divide it up and put it on the local free list.
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

	//TODO: Continue Here
	// What is reserved state? What more states are there? Why do we need to transition it?

	// sysAlloc returns Reserved address space, and any span we're
	// reusing is set to fault (so, also Reserved), so transition
	// it to Prepared and then Ready.
	//
	// Unlike (*mheap).grow, just map in everything that we
	// asked for. We're likely going to use it all.
	sysMap(unsafe.Pointer(base), userArenaChunkBytes, &gcController.heapReleased)
	sysUsed(unsafe.Pointer(base), userArenaChunkBytes, userArenaChunkBytes)

	// Model the user arena as a heap span for a large object.
	spc := makeSpanClass(0, false)
	h.initSpan(s, spanAllocHeap, spc, base, userArenaChunkPages)
	s.isUserArenaChunk = true
	s.elemsize -= userArenaChunkReserveBytes()
	s.limit = s.base() + s.elemsize
	s.freeindex = 1
	s.allocCount = 1

	// Account for this new arena chunk memory.
	gcController.heapInUse.add(int64(userArenaChunkBytes))
	gcController.heapReleased.add(-int64(userArenaChunkBytes))

	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.inHeap, int64(userArenaChunkBytes))
	atomic.Xaddint64(&stats.committed, int64(userArenaChunkBytes))

	// Model the arena as a single large malloc.
	atomic.Xadd64(&stats.largeAlloc, int64(s.elemsize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(s.elemsize))

	// Update heapLive.
	gcController.update(int64(s.elemsize), 0)

	// This must clear the entire heap bitmap so that it's safe
	// to allocate noscan data without writing anything out.
	s.initHeapBits()

	// Clear the span preemptively. It's an arena chunk, so let's assume
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

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	h.central[spc].mcentral.fullSwept(h.sweepgen).push(s)

	// Set up an allocation header. Avoid write barriers here because this type
	// is not a real type, and it exists in an invalid location.
	*(*uintptr)(unsafe.Pointer(&s.largeType)) = uintptr(unsafe.Pointer(s.limit))
	*(*uintptr)(unsafe.Pointer(&s.largeType.GCData)) = s.limit + unsafe.Sizeof(_type{})
	s.largeType.PtrBytes = 0
	s.largeType.Size_ = s.elemsize

	return s
}

// createUserRegion creates a new userRegion ready to be used.
func (r *userRegion) removeRegion() {
	if r.defunct.Load() {
		panic("region double free")
	}

	// Mark ourselves as defunct.
	r.defunct.Store(true)
	SetFinalizer(r, nil)

	s := r.current
	if s != nil {
		freeUserRegionBlock(s, unsafe.Pointer(s.base()))
	}
	// nil out a.active so that a race with freeing will more likely cause a crash.
	r.current = nil
}

// freeUserArenaChunk releases the user arena represented by s back to the runtime.
//
// x must be a live pointer within s.
//
// The runtime will set the user arena to fault once it's safe (the GC is no longer running)
// and then once the user arena is no longer referenced by the application, will allow it to
// be reused.
func freeUserRegionBlock(s *mspan, x unsafe.Pointer) {
	if !s.isUserArenaChunk {
		throw("span is not for a user arena")
	}
	if s.npages*pageSize != userArenaChunkBytes {
		throw("invalid user arena span size")
	}

	// Mark the region as free to various sanitizers immediately instead
	// of handling them at sweep time.
	if raceenabled {
		racefree(unsafe.Pointer(s.base()), s.elemsize)
	}
	if msanenabled {
		msanfree(unsafe.Pointer(s.base()), s.elemsize)
	}
	if asanenabled {
		asanpoison(unsafe.Pointer(s.base()), s.elemsize)
	}

	// Make ourselves non-preemptible as we manipulate state and statistics.
	//
	// Also required by setUserArenaChunksToFault.
	mp := acquirem()

	// Update the span class to be noscan. What we want to happen is that
	// any pointer into the span keeps it from getting recycled, so we want
	// the mark bit to get set, but we're about to set the address space to fault,
	// so we have to prevent the GC from scanning this memory.
	//
	// It's OK to set it here because (1) a GC isn't in progress, so the scanning code
	// won't make a bad decision, (2) we're currently non-preemptible and in the runtime,
	// so a GC is blocked from starting. We might race with sweeping, which could
	// put it on the "wrong" sweep list, but really don't care because the chunk is
	// treated as a large object span and there's no meaningful difference between scan
	// and noscan large objects in the sweeper. The STW at the start of the GC acts as a
	// barrier for this update.
	s.spanclass = makeSpanClass(0, true)

	// Actually set the arena chunk to fault, so we'll get dangling pointer errors.
	// sysFault currently uses a method on each OS that forces it to evacuate all
	// memory backing the chunk.
	sysFault(unsafe.Pointer(s.base()), s.npages*pageSize)

	// Everything on the list is counted as in-use, however sysFault transitions to
	// Reserved, not Prepared, so we skip updating heapFree or heapReleased and just
	// remove the memory from the total altogether; it's just address space now.
	gcController.heapInUse.add(-int64(s.npages * pageSize))

	// Count this as a free of an object right now as opposed to when
	// the span gets off the quarantine list. The main reason is so that the
	// amount of bytes allocated doesn't exceed how much is counted as
	// "mapped ready," which could cause a deadlock in the pacer.
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

	KeepAlive(x)
	releasem(mp)
}
