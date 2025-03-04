// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.regions

/*
The region package provides the ability to allocate memory for a collection
of Go values and free that space manually all at once. The purpose
of this functionality is to improve efficiency: manually freeing memory
before a garbage collection delays that cycle. Less frequent cycles means
the CPU cost of the garbage collector is incurred less frequently.

This functionality in this package is mostly captured in the Region type.
Region allocate large chunks of memory for Go values, so they're likely to
be inefficient for allocating only small amounts of small Go values. They're
best used in bulk, on the order of MiB of memory allocated on each use.

Note that by allowing for this limited form of manual memory allocation
that use-after-free bugs are possible with regular Go values. This package
limits the impact of these use-after-free bugs by preventing reuse of freed
memory regions until the garbage collector is able to determine that it is
safe. Typically, a use-after-free bug will result in a fault and a helpful
error message, but this package reserves the right to not force a fault on
freed memory. That means a valid implementation of this package is to just
allocate all memory the way the runtime normally would, and in fact, it
reserves the right to occasionally do so for some Go values.
*/

package region

import (
	"internal/reflectlite"
	"unsafe"
)

// Region represents a collection of Go values allocated and freed together.
// Regions are useful for improving efficiency as they may be freed back to
// the runtime manually, though any memory obtained from freed arenas must
// not be accessed once that happens. A Region is automatically freed once
// it is no longer referenced, so it must be kept alive (see runtime.KeepAlive)
// until any memory allocated from it is no longer needed.

type Region struct {
	r unsafe.Pointer
}

// CreateRegion allocates a new region.
func CreateRegion() *Region {
	return &Region{r: runtime_region_createRegion()}
}

// RemoveRegion frees the region (and all objects allocated from the region) so that
// memory backing the region can be reused fairly quickly without garbage
// collection overhead. Applications must not call any method on this
// region after it has been freed.
func (r *Region) RemoveRegion() {
	runtime_region_removeRegion(r.r)
	r.r = nil
}

// AllocFromRegion creates a new *T in the provided region. The *T must not be used after
// the region is freed. Accessing the value after free may result in a fault,
// but this fault is also not guaranteed.
func AllocFromRegion[T any](r *Region) *T {
	typ := reflectlite.TypeOf((*T)(nil))
	if typ == reflectlite.TypeOf((*Region)(nil)) {
		var region any
		region = &Region{r: runtime_region_allocNestledRegion(r.r)}
		return (region).(*T)
	}
	return runtime_region_allocFromRegion(r.r, typ).(*T)
}

// CreateChannel creates a new chan in the provided region. The chan must not be used after
// the region is freed. Accessing the value after free may result in a fault,
// but this fault is also not guaranteed.
func CreateChannel[T any](size int) (chan T, *Region) {
	r := CreateRegion()
	var ch chan T
	chPtr := (*uintptr)(unsafe.Pointer(&ch))
	*chPtr = (uintptr)(runtime_region_createChannel(r.r, reflectlite.TypeOf((*T)(nil)), size))
	return ch, r
}

//go:linkname reflect_region_allocFromRegion reflect.region_allocFromRegion
func reflect_region_allocFromRegion(r *Region, typ any) any {
	return runtime_region_allocFromRegion(r.r, typ)
}

//go:linkname runtime_region_createRegion
func runtime_region_createRegion() unsafe.Pointer

//go:linkname runtime_region_allocFromRegion
func runtime_region_allocFromRegion(region unsafe.Pointer, typ any) any

//go:linkname runtime_region_removeRegion
func runtime_region_removeRegion(region unsafe.Pointer)

//go:linkname runtime_region_createChannel
func runtime_region_createChannel(region unsafe.Pointer, typ any, size int) unsafe.Pointer

//go:linkname runtime_region_allocNestledRegion
func runtime_region_allocNestledRegion(region unsafe.Pointer) unsafe.Pointer
