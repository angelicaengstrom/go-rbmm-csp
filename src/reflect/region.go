// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.regions

package reflect

// RegionAllocFromRegion returns a [Value] representing a pointer to a new zero value for the
// specified type, allocating storage for it in the provided arena. That is,
// the returned Value's Type is [PointerTo](typ).
func RegionAllocFromRegion(r *region.Region, typ Type) Value {
	return ValueOf(region_allocFromRegion(r, PointerTo(typ)))
}

func region_allocFromRegion(r *region.Region, typ any) any
