// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.regions

package region_test

import (
	"fmt"
	"region"
	"testing"
)

type T1 struct {
	n int
}
type T2 [1 << 20]byte // 1MiB

func TestSmokeLarge(t *testing.T) {
	a := region.CreateRegion()
	defer a.RemoveRegion()
	for i := 0; i < 10*64; i++ {
		_ = region.AllocFromRegion[T2](a)
	}
}

func TestNestledRegion(t *testing.T) {
	outer := region.CreateRegion()
	defer outer.RemoveRegion()

	inner := region.AllocFromRegion[region.Region](outer)
	inner = region.CreateRegion()
	defer inner.RemoveRegion()

	for i := 0; i < 10*64; i++ {
		_ = region.AllocFromRegion[T2](inner)
	}
}

func TestChannelRegion(t *testing.T) {
	ch, reg := region.CreateChannel[int](0)
	go func() {
		ch <- 1
	}()
	fmt.Println(<-ch)
	reg.RemoveRegion()
}
