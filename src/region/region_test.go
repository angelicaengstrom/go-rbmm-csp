// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.regions

package region_test

import (
	"fmt"
	"region"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
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

func BenchmarkAllocDeallocRegion(b *testing.B) {
	debug.SetGCPercent(-1)
	rounds := 10
	Alloc := 0.0
	Dealloc := 0.0
	M_C := 0.0
	T_A := 0.0
	T_D := 0.0

	for j := 0; j < rounds; j++ {
		numAllocations := 100000
		var memStats runtime.MemStats
		var successfulAlloc uint64
		var allocationTimeStart time.Time
		var allocationTime int64

		var deallocationTimeStart time.Time
		var deallocationTime int64

		var peakMemoryConsumption uint64

		r1 := region.CreateRegion()

		for i := region.AllocFromRegion[int](r1); *i < numAllocations; *i++ {
			allocationTimeStart = time.Now()
			obj := region.AllocFromRegion[[128]byte](r1)
			allocationTime += time.Since(allocationTimeStart).Nanoseconds()

			if obj != nil {
				successfulAlloc++
			}

			runtime.ReadMemStats(&memStats)
			if memStats.HeapAlloc > peakMemoryConsumption {
				peakMemoryConsumption = memStats.HeapAlloc
			}
		}

		deallocationTimeStart = time.Now()
		r1.RemoveRegion()
		deallocationTime = time.Since(deallocationTimeStart).Nanoseconds()

		runtime.ReadMemStats(&memStats)
		Alloc += float64(successfulAlloc) / float64(numAllocations)
		Dealloc += float64(memStats.RegionDealloc) / float64(memStats.RegionAlloc)
		M_C += float64(peakMemoryConsumption)
		T_A += float64(allocationTime)
		T_D += float64(deallocationTime)

	}
	b.ReportMetric(float64(Alloc)/float64(rounds), "Alloc%")
	b.ReportMetric(float64(Dealloc)/float64(rounds), "Dealloc%")
	b.ReportMetric(float64(M_C)/float64(rounds), "M_consumption(B)")
	b.ReportMetric(float64(T_A)/float64(rounds), "T_alloc(ns)")
	b.ReportMetric(float64(T_D)/float64(rounds), "T_dealloc(ns)")
}

func BenchmarkAllocDeallocGC(b *testing.B) {
	rounds := 10
	Alloc := 0.0
	Dealloc := 0.0
	M_C := 0.0
	T_A := 0.0
	T_D := 0.0

	for j := 0; j < rounds; j++ {
		numAllocations := 100000
		var memStats runtime.MemStats

		var allocationTimeStart time.Time
		var allocationTime int64
		var successfulAlloc uint64

		var peakMemoryConsumption uint64

		var deallocationTimeStart time.Time
		var deallocationTime int64

		runtime.ReadMemStats(&memStats)
		mallocBefore := memStats.Mallocs
		freeBefore := memStats.Frees

		var obj []byte

		debug.SetGCPercent(-1)

		for i := 0; i < numAllocations; i++ {
			allocationTimeStart = time.Now()
			obj = make([]byte, 128)
			allocationTime += time.Since(allocationTimeStart).Nanoseconds()

			if obj != nil {
				successfulAlloc++
			}
			runtime.ReadMemStats(&memStats)
			if memStats.HeapAlloc > peakMemoryConsumption {
				peakMemoryConsumption = memStats.HeapAlloc
			}
		}

		deallocationTimeStart = time.Now()
		runtime.GC()
		deallocationTime = time.Since(deallocationTimeStart).Nanoseconds()
		runtime.ReadMemStats(&memStats)
		Alloc += float64(memStats.Mallocs-mallocBefore) / float64(numAllocations)
		Dealloc += float64(memStats.Frees-freeBefore) / float64(numAllocations)
		M_C += float64(peakMemoryConsumption)
		T_A += float64(allocationTime)
		T_D += float64(deallocationTime)
	}
	b.ReportMetric(float64(Alloc)/float64(rounds), "Alloc%")
	b.ReportMetric(float64(Dealloc)/float64(rounds), "Dealloc%")
	b.ReportMetric(float64(M_C)/float64(rounds), "M_consumption(B)")
	b.ReportMetric(float64(T_A)/float64(rounds), "T_alloc(ns)")
	b.ReportMetric(float64(T_D)/float64(rounds), "T_dealloc(ns)")
}

func BenchmarkReusalRegion(b *testing.B) {
	rounds := 10

	var Reuse, ExtFrag, IntFrag, T_A float64
	debug.SetGCPercent(-1)

	for j := 0; j < rounds; j++ {
		numAllocations := 100
		var memStats runtime.MemStats
		var allocationTimeStart time.Time
		var allocationTime int64

		var peakInternalFragmentation, peakExternalFragmentation, peakMemoryConsumption float64

		for i := 0; i < numAllocations; i++ {
			allocationTimeStart = time.Now()
			r1 := region.CreateRegion()
			region.AllocFromRegion[[10485]byte](r1)
			region.AllocFromRegion[[524288]byte](r1)
			region.AllocFromRegion[[1048576]byte](r1)
			region.AllocFromRegion[[2097152]byte](r1)
			allocationTime += time.Since(allocationTimeStart).Nanoseconds()

			runtime.ReadMemStats(&memStats)
			if peakInternalFragmentation < float64(memStats.RegionIntFrag)/float64(memStats.RegionInUse) {
				peakInternalFragmentation = float64(memStats.RegionIntFrag) / float64(memStats.RegionInUse)
			}

			if peakExternalFragmentation < float64(memStats.HeapIdle)/float64(memStats.HeapSys) {
				peakExternalFragmentation = float64(memStats.HeapIdle) / float64(memStats.HeapSys)
			}

			if peakMemoryConsumption < float64(memStats.HeapLargeInUse) {
				peakMemoryConsumption = float64(memStats.HeapLargeInUse)
			}

			r1.RemoveRegion()
		}

		runtime.ReadMemStats(&memStats)
		regionReused := float64(memStats.RegionReuse) - float64(memStats.RegionCreated)
		Reuse += regionReused / float64(memStats.RegionReuse)
		ExtFrag += peakExternalFragmentation
		IntFrag += peakInternalFragmentation
		T_A += float64(allocationTime)
	}

	b.ReportMetric(float64(Reuse)/float64(rounds), "Reuse%")
	b.ReportMetric(float64(IntFrag)/float64(rounds), "IntFrag%")
	b.ReportMetric(float64(ExtFrag)/float64(rounds), "ExtFrag%")
	b.ReportMetric(float64(T_A)/float64(rounds), "T_alloc(ns)")
}

func BenchmarkReusalGC(b *testing.B) {
	rounds := 10

	var Reuse, ExtFrag, IntFrag, T_A float64
	debug.SetGCPercent(-1)

	for j := 0; j < rounds; j++ {
		numAllocations := 100
		var memStats runtime.MemStats
		var allocationTimeStart time.Time
		var allocationTime int64

		var peakInternalFragmentation, peakExternalFragmentation float64

		sizes := []int{10485, 524288, 1048576, 2097152}

		for i := 0; i < numAllocations; i++ {
			allocationTimeStart = time.Now()
			allocatedObjects := make([][]byte, len(sizes))
			for k := 0; k < len(sizes); k++ {
				allocatedObjects[k] = make([]byte, sizes[k])
			}
			allocationTime += time.Since(allocationTimeStart).Nanoseconds()

			runtime.ReadMemStats(&memStats)
			if peakInternalFragmentation < float64(memStats.HeapIntFrag)/float64(memStats.HeapAlloc) {
				peakInternalFragmentation = float64(memStats.HeapIntFrag) / float64(memStats.HeapAlloc)
			}

			if peakExternalFragmentation < float64(memStats.HeapIdle)/float64(memStats.HeapSys) {
				peakExternalFragmentation = float64(memStats.HeapIdle) / float64(memStats.HeapSys)
			}

			runtime.GC()
		}

		runtime.ReadMemStats(&memStats)
		spanReused := float64(memStats.HeapSpanUsed) - float64(memStats.HeapSpanCreated)
		Reuse += spanReused / float64(memStats.HeapSpanUsed)
		ExtFrag += peakExternalFragmentation
		IntFrag += peakInternalFragmentation
		T_A += float64(allocationTime)
	}

	b.ReportMetric(float64(Reuse)/float64(rounds), "Reuse%")
	b.ReportMetric(float64(IntFrag)/float64(rounds), "IntFrag%")
	b.ReportMetric(float64(ExtFrag)/float64(rounds), "ExtFrag%")
	b.ReportMetric(float64(T_A)/float64(rounds), "T_alloc(ns)")
}

const (
	numWorkers  = 15
	numRequests = 10
)

type position struct {
	x int
	y int
}

func BenchmarkConcurrencyStruct(b *testing.B) {
	debug.SetGCPercent(-1)
	requests, r1 := region.CreateChannel[position](numRequests)

	for i := 0; i < numWorkers; i++ {
		if r1.IncRefCounter() {
			go func() {
				for j := 0; j < numRequests; j++ {
					requests <- position{i, j}
				}
				r1.DecRefCounter()
			}()
		}
	}

	for i := 0; i < numRequests*numWorkers; i++ {
		fmt.Println(<-requests)
	}

	close(requests)

	r1.RemoveRegion()

}

func BenchmarkConcurrencyRegion(b *testing.B) {
	debug.SetGCPercent(-1)

	rounds := 10

	var T_C, T_L, Theta, Err float64

	for r := 0; r < rounds; r++ {
		var latencyTime, err float64
		startTime := time.Now()
		requests, r1 := region.CreateChannel[int](numRequests)

		for i := 0; i < numWorkers; i++ {
			if r1.IncRefCounter() {
				go func() {
					for j := 0; j < numRequests; j++ {
						latencyStart := time.Now()
						requests <- i
						latencyTime += float64(time.Since(latencyStart).Milliseconds())
					}
					r1.DecRefCounter()
				}()
			}
		}

		var responses [numWorkers]int

		for i := 0; i < numRequests*numWorkers; i++ {
			responses[<-requests]++
		}

		close(requests)

		r1.RemoveRegion()
		T_C += float64(time.Since(startTime).Milliseconds())

		for _, res := range responses {
			if res != numRequests {
				err += float64(numRequests - res)
			}
		}

		Err += err / float64(numRequests)
		T_L += latencyTime
		Theta += float64(numRequests) / T_C
	}
	b.ReportMetric(Err/float64(rounds), "Err%")
	b.ReportMetric(float64(T_C)/float64(rounds), "T_C(ms)")
	b.ReportMetric(float64(T_L)/float64(rounds), "T_L(ms)")
	b.ReportMetric(float64(Theta)/float64(rounds), "Theta(op/ms)")
}

func BenchmarkConcurrencyGC(b *testing.B) {
	debug.SetGCPercent(-1)

	rounds := 10

	var T_C, T_L, Theta, Err float64

	for r := 0; r < rounds; r++ {
		var latencyTime, err float64
		startTime := time.Now()
		requests := make(chan int, numRequests)

		for i := 0; i < numWorkers; i++ {
			go func() {
				for j := 0; j < numRequests; j++ {
					latencyStart := time.Now()
					requests <- i
					latencyTime += float64(time.Since(latencyStart).Milliseconds())
				}
			}()
		}

		var responses [numWorkers]int

		for i := 0; i < numRequests*numWorkers; i++ {
			responses[<-requests]++
		}

		close(requests)

		runtime.GC()
		T_C += float64(time.Since(startTime).Milliseconds())

		for _, res := range responses {
			if res != numRequests {
				err += float64(numRequests - res)
			}
		}

		Err += err / float64(numRequests)
		T_L += latencyTime
		Theta += float64(numRequests) / T_C
	}
	b.ReportMetric(Err/float64(rounds), "Err%")
	b.ReportMetric(float64(T_C)/float64(rounds), "T_C(ms)")
	b.ReportMetric(float64(T_L)/float64(rounds), "T_L(ms)")
	b.ReportMetric(float64(Theta)/float64(rounds), "Theta(op/ms)")
}

type smallScalar struct {
	X uintptr
}
type complex struct {
	s  string
	i  int64
	f  float64
	ch chan bool
}

func TestAllocChannel(t *testing.T) {
	t.Run("channel", func(t *testing.T) {
		// Create a channel region with a unbuffered channel (size == 0)
		ch, reg := region.CreateChannel[complex](0)
		ch2 := region.AllocChannel[bool](0, reg)
		//ch := make(chan complex)
		// Generate a goroutine that sends value to the channel
		reg.IncRefCounter()
		go func() {
			ch <- complex{"test", 123, 4.56, ch2}
			<-ch2
			reg.DecRefCounter()
		}()

		res := <-ch
		res.ch <- true

		// If the main goroutine can't fetch the value, the region-channel wasn't able to allocate
		if res.s != "test" && res.i != 123 && res.f != 4.56 {
			t.Log(res)
			t.Errorf("CreateRegionChannel() wasn't able to allocate")
		}

		// Close the channel
		close(ch)

		// remove the channel region
		reg.RemoveRegion()
	})
}
