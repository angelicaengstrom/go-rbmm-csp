// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/reflectlite"
	"reflect"
	. "runtime"
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
		/*
			ss := &smallScalar{5}
			runSubTestAllocRegion(t, ss, false)

			mse := new(mediumScalarEven)
			for i := range mse {
				mse[i] = 121
			}
			runSubTestAllocRegionFullList(t, mse, false)

			mso := new(mediumScalarOdd)
			for i := range mso {
				mso[i] = 122
			}
			runSubTestUserArenaNew(t, mso, false)

			runSubTestAllocNestledRegion(t, false)

			runSubTestAllocChannel(t, false)

			runSubTestAllocBufferedChannel(t, false)

			runSubTestAllocGoRoutine(t, false)*/

		runSubTestAllocLocalFreeList(t, false)
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

		n := int(UserArenaChunkBytes / unsafe.Sizeof(*value))
		if n == 0 {
			n = 1
		}

		// Create a new region and do a bunch of operations on it.
		region := CreateUserRegion()
		block1 := region.GetBlock() //First region block that we allocate to

		for j := 0; j < n+1; j++ {
			x := region.AllocateFromRegion(reflectlite.TypeOf((*S)(nil)))
			s := x.(*S)
			*s = *value
		}

		// The current region block should now be in another span
		block2 := region.GetBlock()

		if block1 == block2 {
			t.Errorf("runSubTestAllocRegionFullList() wasn't able to allocate a new region block")
		}

		_, memStats := ReadMemStatsSlow()
		if memStats.HeapAlloc < uint64(UserArenaChunkBytes)*2 {
			t.Errorf("runSubTestAllocRegionFullList() wasn't able to allocate multiple region-block on the heap")
		}

		// Release the region.
		region.RemoveUserRegion()

		_, memStats = ReadMemStatsSlow()
		if memStats.HeapAlloc > uint64(UserArenaChunkBytes)*2 {
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
		inner := outer.AllocateInnerRegion()
		x := inner.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{5})).(*smallScalar)
		if x == nil {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to allocate ")
		}

		// Release the inner region before the outer
		inner.RemoveUserRegion()

		// Release the region.
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
		t.Log("outer.AllocateInnerRegion()")
		x := inner.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{5})).(*smallScalar)
		if x == nil {
			t.Errorf("runSubTestAllocNestledRegion() wasn't able to allocate ")
		}

		// Release the inner region before the outer
		inner.RemoveUserRegion()
		t.Log("inner.RemoveUserRegion()")

		outer.AllocateFromRegion(reflectlite.TypeOf(&[(UserArenaChunkBytes * 3 / 4) - 100]byte{}))
		t.Log("outer.AllocateFromRegion(UserArenaChunkBytes 3 / 4 - 5)")

		outer.AllocateFromRegion(reflectlite.TypeOf(&[UserArenaChunkBytes / 6]byte{}))
		t.Log("outer.AllocateFromRegion(UserArenaChunkBytes / 5)")

		outer.AllocateFromRegion(reflectlite.TypeOf(&[UserArenaChunkBytes / 6]byte{}))
		t.Log("outer.AllocateFromRegion(UserArenaChunkBytes / 5)")

		// Release the region.
		//outer.RemoveUserRegion()
		t.Log("outer.RemoveUserRegion()")

	})
}

func runSubTestAllocChannel(t *testing.T, parallel bool) {
	t.Run("channel", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Create a channel region.
		ch, reg := CreateRegionChannel[int](0)
		go func() {
			ch <- 1
		}()

		if <-ch != 1 {
			t.Errorf("CreateRegionChannel() wasn't able to allocate")
		}
		// Close the channel
		close(ch)
		reg.RemoveUserRegion()
	})
}

func runSubTestAllocBufferedChannel(t *testing.T, parallel bool) {
	t.Run("BufferedChannel", func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		n := int(UserArenaChunkBytes / unsafe.Sizeof(&mediumPointerEven{}))
		if n == 0 {
			n = 1
		}

		// Create a channel region.
		sz := n + 1
		ch, reg := CreateRegionChannel[*mediumPointerEven](sz)
		go func() {
			region := CreateUserRegion()
			mse := region.AllocateFromRegion(reflectlite.TypeOf(&mediumPointerEven{})).(*mediumPointerEven)

			for i := range mse {
				mse[i] = region.AllocateFromRegion(reflectlite.TypeOf(&smallPointer{})).(*smallPointer)
			}
			for i := 0; i < sz; i++ {
				ch <- mse
			}

		}()
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
		// region.RemoveUserRegion()

		// If the region hasn't been removed already
		if r1.IncrementCounter() {
			go func(value *smallScalar, r1 *UserRegion) {
				r2 := CreateUserRegion()
				y := r2.AllocateFromRegion(reflectlite.TypeOf(&smallScalar{})).(*smallScalar)
				y.X = value.X
				r2.RemoveUserRegion()
				r1.RemoveUserRegion()
			}(x, r1)
		}

		time.Sleep(5 * time.Millisecond)

		r1.RemoveUserRegion()

	})
}

func TestDeallocRegion(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Dealloc", func(t *testing.T) {
		region := CreateUserRegion()
		_, memStatsBefore := ReadMemStatsSlow()
		region.RemoveUserRegion()
		_, memStatsAfter := ReadMemStatsSlow()

		if region.GetBlock() != nil {
			t.Errorf("RemoveUserRegion() should have nil region")
		}

		if memStatsAfter.HeapAlloc > memStatsBefore.HeapAlloc+uint64(UserArenaChunkBytes) {
			t.Errorf("RemoveUserRegion() wasn't able to deallocate a region-block on the heap")
		}
	})
}
