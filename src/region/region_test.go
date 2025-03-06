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
	inner := region.AllocFromRegion[region.Region](outer)

	for i := 0; i < 10*64; i++ {
		_ = region.AllocFromRegion[T2](inner)
	}
	inner.RemoveRegion()
	outer.RemoveRegion()
}

func TestChannelRegion(t *testing.T) {
	ch, reg := region.CreateChannel[int](0)
	go func() {
		ch <- 1
	}()
	fmt.Println(<-ch)
	reg.RemoveRegion()
}

func TestChannelRegion2(t *testing.T) {
	ch, r1 := region.CreateChannel[int](5)

	if r1.IncRefCounter() {
		go func() {
			r2 := region.CreateRegion()
			for i := region.AllocFromRegion[int](r2); *i < 5; *i++ {
				ch <- *i
			}
			r2.RemoveRegion()
			r1.DecRefCounter()
		}()
	}

	if r1.IncRefCounter() {
		go func() {
			r2 := region.CreateRegion()
			for i := region.AllocFromRegion[int](r2); *i < 5; *i++ {
				<-ch
			}
			r2.RemoveRegion()
			r1.DecRefCounter()
		}()
	}
	r1.RemoveRegion()
}
