// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func benchmarkSelectionAnnotation(id uint64) string {
	return `[{"name":"net` + strconv.FormatUint(id, 10) + `","namespace":"tenant","ips":["10.0.0.1"]},` +
		`{"name":"net1"},{"name":"net2"},{"name":"net3"},{"name":"net4"},` +
		`{"name":"net5"},{"name":"net6"},{"name":"net7"}]`
}

func BenchmarkNetworkSelectionCacheAccess(b *testing.B) {
	for _, parallel := range []bool{false, true} {
		for _, cold := range []bool{false, true} {
			name := "serial"
			if parallel {
				name = "parallel"
			}
			if cold {
				name += "/unique-misses"
			} else {
				name += "/warm-hit"
			}
			b.Run(name, func(b *testing.B) {
				c := newNetworkSelectionCache(networkSelectionCacheEntries, networkSelectionCacheBytes)
				annotation := benchmarkSelectionAnnotation(0)
				if _, err := c.get("tenant", annotation); err != nil {
					b.Fatal(err)
				}
				var sequence atomic.Uint64
				lookup := func() {
					raw := annotation
					if cold {
						raw = benchmarkSelectionAnnotation(sequence.Add(1))
					}
					if _, err := c.get("tenant", raw); err != nil {
						b.Error(err)
					}
				}
				b.ReportAllocs()
				if parallel {
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							lookup()
						}
					})
				} else {
					for b.Loop() {
						lookup()
					}
				}
			})
		}
	}
}

func BenchmarkNetworkSelectionCacheColdBurst(b *testing.B) {
	const callbacks = 32
	annotation := benchmarkSelectionAnnotation(0)
	b.ReportAllocs()
	for b.Loop() {
		c := newNetworkSelectionCache(networkSelectionCacheEntries, networkSelectionCacheBytes)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range callbacks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := c.get("tenant", annotation); err != nil {
					b.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
}
