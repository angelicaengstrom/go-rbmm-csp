// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"fmt"
	"internal/reflectlite"
	"reflect"
	. "runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestCreateRegion(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Create", func(t *testing.T) {
		region := CreateUserRegion()
		if UserArenaChunkBytes != region.GetSize() {
			t.Errorf("CreateUserRegion() should have size equal to UserArenaChunkBytes, currently %d", uint64(region.GetSize()))
		}

		_, memStats := ReadMemStatsSlow()
		if memStats.HeapAlloc < uint64(UserArenaChunkBytes) {
			t.Errorf("CreateUserRegion() wasn't able to allocate a region-block on the heap")
		}
	})
}

func TestAllocRegion(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Alloc", func(t *testing.T) {

		ss := &smallScalar{5}
		runSubTestAllocRegion(t, ss, false)

		mse := new(mediumScalarEven)
		for i := range mse {
			mse[i] = 121
		}
		runSubTestAllocRegionFullList(t, mse, false)
		runSubTestAllocNestledRegion(t, false)
		runSubTestAllocChannel(t, false)
		runSubTestAllocBufferedChannel(t, false)
		runSubTestAllocGoRoutine(t, false)
		runSubTestAllocLocalFreeList(t, false)
		runSubTestCantAllocLocalFreeList(t, false)
		runSubTestAllocGlobalFreeList(t, false)
	})
}

func runSubTestAllocRegion[S comparable](t *testing.T, value *S, parallel bool) {
	t.Run(reflect.TypeOf(value).Elem().Name(), func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a new region and do a bunch of operations on it.
		region := CreateUserRegion()
		//Assign new region
		x := region.AllocateFromRegion(reflectlite.TypeOf((*S)(nil)))
		if x == nil {
			t.Errorf("AllocateFromRegion() wasn't able to allocate ")
		}

		// Release the region.
		region.RemoveUserRegion()
	})
}

func runSubTestAllocRegionFullList[S comparable](t *testing.T, value *S, parallel bool) {
	t.Run(reflect.TypeOf(value).Elem().Name(), func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		_, prevAllocated := ReadMemStatsSlow()

		n := int(UserArenaChunkBytes / unsafe.Sizeof(*value))
		if n == 0 {
			n = 1
		}

		// Create a new region and do a bunch of operations on it.
		region := CreateUserRegion()
		block1 := region.GetBlock() //First region block that we allocate to

		// Allocates more memory than is available in our region block
		for j := 0; j < n+1; j++ {
			x := region.AllocateFromRegion(reflectlite.TypeOf((*S)(nil)))
			s := x.(*S)
			*s = *value
		}

		// The current region block should now be in another span
		block2 := region.GetBlock()

		// If the first block and the second block are the same we weren't able to fetch a new block from the heap
		if block1 == block2 {
			t.Errorf("runSubTestAllocRegionFullList() wasn't able to allocate a new region block")
		}

		// Each memory block contains UserArenaChunkBytes amount of memory, if the heap contains memory less than
		// what was allocated from the start the allocation must have failed
		_, memStats := ReadMemStatsSlow()
		if memStats.HeapAlloc < prevAllocated.HeapAlloc {
			t.Errorf("runSubTestAllocRegionFullList() wasn't able to allocate multiple region-block on the heap")
		}
		// Release the region.
		region.RemoveUserRegion()

		// After releasing the memory we should have memory less than what was previously allocated + a single region block
		_, memStats = ReadMemStatsSlow()
		if memStats.HeapAlloc >= prevAllocated.HeapAlloc+uint64(UserArenaChunkBytes) {
			t.Errorf("runSubTestAllocRegionFullList() wasn't able to deallocate multiple region-block on the heap")
		}
	})
}

func runSubTestAllocNestledRegion(t *testing.T, parallel bool) {
	t.Run("region.Region", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a new outer region.
		outer := CreateUserRegion()
		_, prevAllocated := ReadMemStatsSlow()
		inner := outer.AllocateInnerRegion()
		x := inner.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{5})).(*smallScalar)
		// Should be able to allocate within the inner region
		if x == nil {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to allocate ")
		}

		// The inner region block shouldn't grow in size as a new region block would do, as it is nestled within
		// the outer region
		_, nestledAllocated := ReadMemStatsSlow()
		if nestledAllocated.HeapAlloc > prevAllocated.HeapAlloc+uint64(UserArenaChunkBytes) {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to allocate nestled region-block on the heap")
		}

		// Release the inner region before the outer
		inner.RemoveUserRegion()

		// Releasing the inner region should not free any memory to the heap, it should return its pages to its parent
		_, nestledFreed := ReadMemStatsSlow()
		if nestledFreed.HeapAlloc == nestledAllocated.HeapAlloc {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to free nestled region-block on the heap")
		}
		outer.RemoveUserRegion()
	})
}

func runSubTestAllocLocalFreeList(t *testing.T, parallel bool) {
	t.Run("region.Region.localFreeList", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a new outer region.
		outer := CreateUserRegion()
		inner := outer.AllocateInnerRegion()
		x := inner.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{5})).(*smallScalar)
		if x == nil {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to allocate ")
		}
		// Release the inner region before the outer, should return its memory to the outer local free list
		inner.RemoveUserRegion()
		if outer.IsEmptyLocalFreeList() {
			t.Errorf("runSubTestAllocLocalFreeList() wasn't able to free memory to local free list")
		}

		// Fill the region block, which should make it require memory from the local free list next time it allocates
		// OBS: We cannot allocate more than UserArenaChunkBytes / 4 as it will be placed on the heap
		outer.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 4)]byte{}))
		outer.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 4)]byte{}))
		outer.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 4)]byte{}))

		// Request memory lesser than the memory that is lesser than the block in the local free-list
		// Should allocate on the page in the local free-list
		outer.AllocateFromRegion(reflectlite.TypeOf(&[UserArenaChunkBytes / 6]byte{}))

		if !outer.IsEmptyLocalFreeList() {
			t.Errorf("runSubTestAllocLocalFreeList() wasn't able to allocate to local free list")
		}

		// Request more memory than what's left in the current block, should require memory from the heap
		outer.AllocateFromRegion(reflectlite.TypeOf(&[UserArenaChunkBytes / 6]byte{}))

		// Release the outer region.
		outer.RemoveUserRegion()

	})
}

func runSubTestCantAllocLocalFreeList(t *testing.T, parallel bool) {
	t.Run("region.Region.localFreeList2", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a new outer region.
		r1 := CreateUserRegion()
		// Depth 1
		r2 := r1.AllocateInnerRegion()
		// Depth 2
		r3 := r2.AllocateInnerRegion()
		r3.RemoveUserRegion() // Place the smaller region to the local free-list
		r4 := r2.AllocateInnerRegion()
		r4.RemoveUserRegion() // Place the smaller region to the local free-list

		if r2.IsEmptyLocalFreeList() {
			t.Errorf("runSubTestCantAllocLocalFreeList() wasn't able to free memory to local free list at depth 1")
		}

		// Fill the region block, which should make it require memory from the local free list next time it allocates
		// OBS: We cannot allocate more than UserArenaChunkBytes / 4 as it will be placed on the heap
		r2.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 16)]byte{}))

		// Request memory larger than the memory in the local free-list
		// Should not allocate from the local free-list
		r2.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 8)]byte{}))
		r2.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes / 8)]byte{}))

		// The local free-list should still contain items since the free pages were too small for the allocation
		if r2.IsEmptyLocalFreeList() {
			t.Errorf("runSubTestAllocLocalFreeList() wasn't able to allocate to local free list")
		}

		// Release the outer regions
		r2.RemoveUserRegion()
		r1.RemoveUserRegion()

	})
}

func runSubTestAllocChannel(t *testing.T, parallel bool) {
	t.Run("channel", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a channel region with a unbuffered channel (size == 0)
		ch, reg := CreateRegionChannel[int](0)

		// Generate a goroutine that sends value to the channel
		go func() {
			ch <- 1
		}()

		// If the main goroutine can't fetch the value, the region-channel wasn't able to allocate
		if <-ch != 1 {
			t.Errorf("CreateRegionChannel() wasn't able to allocate")
		}

		// Close the channel
		close(ch)
		// remove the channel region
		reg.RemoveUserRegion()
	})
}

func runSubTestAllocBufferedChannel(t *testing.T, parallel bool) {
	t.Run("BufferedChannel", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}
		_, preAllocated := ReadMemStatsSlow()

		n := int(UserArenaChunkBytes / unsafe.Sizeof(&mediumPointerEven{}))
		if n == 0 {
			n = 1
		}

		// Create a channel region with a buffered channel
		sz := n + 1                                            // Set the buffered channel to be larger than a region block
		ch, reg := CreateRegionChannel[*mediumPointerEven](sz) // Initializing a buffered channel should generate two region blocks

		_, bufferedAllocated := ReadMemStatsSlow()
		// Fails if the amount of heap allocated is lesser then the static size of region block times 2
		if bufferedAllocated.HeapAlloc < preAllocated.HeapAlloc+uint64(UserArenaChunkBytes)*2 {
			t.Errorf("runSubTestAllocBufferedChannel() wasn't able to initialize channel")
		}
		go func() {
			region := CreateUserRegion()
			mse := region.AllocateFromRegion(reflectlite.TypeOf(&mediumPointerEven{})).(*mediumPointerEven)

			for i := range mse {
				mse[i] = region.AllocateFromRegion(reflectlite.TypeOf(&smallPointer{})).(*smallPointer)
			}
			for i := 0; i < sz; i++ {
				ch <- mse
			}
			region.RemoveUserRegion()
		}()
		// Should be able to fetch the values even if the buffered values are spread out on various region blocks
		for i := 0; i < sz; i++ {
			<-ch
		}

		// Close the channel
		close(ch)
		reg.RemoveUserRegion()
	})
}

func runSubTestAllocGoRoutine(t *testing.T, parallel bool) {
	t.Run("GoRoutineCounter", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}
		r1 := CreateUserRegion()
		x := r1.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{})).(*smallScalar)
		x.X = 5

		// Increment the counter before a goroutine references the region to prevent freeing of memory that will
		// later be used
		if r1.IncrementCounter() {
			go func(value *smallScalar, r1 *UserRegion) {
				// Each goroutine should have its own region to allocate their own values
				// It is not safe to write to a referred memory - without locks
				r2 := CreateUserRegion()
				y := r2.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{})).(*smallScalar)
				y.X = value.X
				r2.RemoveUserRegion()
				// Decrement the counter as the reference is no longer needed, will remove the region if main isn't
				// referring to it anymore
				time.Sleep(5 * time.Millisecond)
				r1.DecrementCounter()
			}(x, r1)
		}

		r1.RemoveUserRegion()
		time.Sleep(10 * time.Millisecond)

	})
}

func TestDeallocRegion(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Dealloc", func(t *testing.T) {
		region := CreateUserRegion()
		// Reads memstats before creating the region
		_, memStatsBefore := ReadMemStatsSlow()
		region.RemoveUserRegion()
		// Reads memstats after freeing the reigon
		_, memStatsAfter := ReadMemStatsSlow()

		// If it is possible to fetch the region block, it wasn't properly freed
		if region.GetBlock() != nil {
			t.Errorf("RemoveUserRegion() should have nil region")
		}

		// If the memStatsAfter are larger or equal to memStatsBefore, the memory wasn't properly freed
		if memStatsAfter.HeapAlloc >= memStatsBefore.HeapAlloc {
			t.Errorf("RemoveUserRegion() wasn't able to deallocate a region-block on the heap")
		}
	})
}

func runSubTestAllocGlobalFreeList(t *testing.T, parallel bool) {
	t.Run("region.Region.globalFreeList", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a new region.
		r1 := CreateUserRegion()

		// Exhaust the global-free list, in case the global-free list got blocks after allocation of r1
		var regions []*UserRegion
		for !IsEmptyGlobalFreeList() {
			regions = append(regions, CreateUserRegion())
		}

		r1.RemoveUserRegion()
		// The removal of r1 should have added the block to the global free-list
		if IsEmptyGlobalFreeList() {
			t.Errorf("runSubTestAllocGlobalFreeList() wasn't able to return to global-free list")
		}

		r2 := CreateUserRegion()
		// The global free-list should now be empty as r2 reused the block
		if !IsEmptyGlobalFreeList() {
			t.Errorf("runSubTestAllocGlobalFreeList() wasn't able to create region from global-free list")
		}

		x := r2.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{}))
		if x == nil {
			t.Errorf("runSubTestAllocGlobalFreeList() wasn't able to allocate from reused memory")
		}

		r2.RemoveUserRegion()

		// Place back all the allocated regions
		for _, region := range regions {
			region.RemoveUserRegion()
		}
	})
}

func TestConcurrentQueue(t *testing.T) {
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("ConcurrentQueue", func(t *testing.T) {
		runSubTestEnqueue(t, false)
		runSubTestDequeue(t, true)
	})
}

func runSubTestEnqueue(t *testing.T, parallel bool) {
	t.Run("Enqueue", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}
		queue := &ConcurrentQueue[int]{}
		queue.Init()
		queue.Enqueue(1)
		queue.Enqueue(2)
	})
}

func runSubTestDequeue(t *testing.T, parallel bool) {
	t.Run("Dequeue", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}
		queue := &ConcurrentQueue[int]{}
		queue.Init()
		queue.Enqueue(1)

		if queue.Dequeue() != 1 {
			t.Errorf("Dequeue() wasn't able to deallocate")
		}

		// Dequeuing when there are no values should result in a nil value
		if queue.Dequeue() != 0 {
			t.Errorf("Dequeue() wasn't able to deallocate")
		}
	})
}

// Benchmarking
func BenchmarkMemoryAllocationDeallocation(b *testing.B) {
	var sizes = [][]byte{
		make([]byte, 64),
		make([]byte, 128),
		make([]byte, 256),
		make([]byte, 512),
		make([]byte, 1024),
	}

	var memBefore, memAfter MemStats
	for _, typ := range sizes {
		GC()
		_, memBefore = ReadMemStatsSlow()

		start := time.Now()
		for i := 0; i < b.N; i++ {
			region := CreateUserRegion()
			_ = region.AllocateFromRegion(reflectlite.TypeOf(&typ))
			region.RemoveUserRegion()
		}
		elapsed := time.Since(start)

		GC()
		_, memAfter = ReadMemStatsSlow()

		b.ReportMetric(float64(memAfter.Alloc-memBefore.Alloc), "bytes_alloc")
		b.ReportMetric(float64(elapsed.Microseconds())/float64(b.N), "µs/op")
	}
}

func BenchmarkHighConcurrency(b *testing.B) {
	const goroutines = 64
	b.Run(fmt.Sprintf("%d_Goroutines", goroutines), func(b *testing.B) {
		b.SetParallelism(goroutines)
		var allocationsCounter atomic.Int32
		var deallocationsCounter atomic.Int32

		b.RunParallel(func(pb *testing.PB) {
			region := CreateUserRegion()
			_ = region.AllocateFromRegion(reflectlite.TypeOf(&[1024]byte{}))

			for pb.Next() {
				if region.IncrementCounter() {
					allocationsCounter.Add(1)
					region.DecrementCounter()
					deallocationsCounter.Add(1)
				}
			}
			region.RemoveUserRegion()
		})
		b.ReportMetric(float64(allocationsCounter.Load()), "allocCounter")
		b.ReportMetric(float64(deallocationsCounter.Load()), "deallocCounter")
	})
}
