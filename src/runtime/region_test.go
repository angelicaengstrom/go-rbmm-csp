// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"reflect"
	. "runtime"
	"testing"
	"unsafe"
)

func TestCreateRegion(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))
	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Create", func(t *testing.T) {
		region := CreateUserRegion()
		if UserArenaChunkBytes == region.GetSize() {
			t.Errorf("CreateUserRegion() should have size equal to UserArenaChunkBytes, currently %d", uint64(region.GetSize()))
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
	})
}

func runSubTestAllocRegion[S comparable](t *testing.T, value *S, parallel bool) {
	t.Run(reflect.TypeOf(value).Elem().Name(), func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Allocate and write data, enough to exhaust the arena.
		//
		// This is an underestimate, likely leaving some space in the arena. That's a good thing,
		// because it gives us coverage of boundary cases.
		n := int(UserArenaChunkBytes / unsafe.Sizeof(*value))
		if n == 0 {
			n = 1
		}

		// Create a new region and do a bunch of operations on it.
		//region := NewUserArena()

		//Assign new region

		// Release the region.

	})
}

func TestDeallocRegion(t *testing.T) {

}
