//go:build goexperiment.regions

// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"reflect"
	"region"
)

func main() {
	a := region.CreateRegion()
	defer a.RemoveRegion()

	const iValue = 10

	i := region.AllocFromRegion[int](a)
	*i = iValue

	if *i != iValue {
		// This test doesn't reasonably expect this to fail. It's more likely
		// that *i crashes for some reason. Still, why not check it.
		log.Fatalf("bad i value: got %d, want %d", *i, iValue)
	}
}
