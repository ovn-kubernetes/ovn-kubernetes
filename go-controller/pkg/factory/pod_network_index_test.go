// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
)

func networkIndexPod(name, selection string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant", Name: name, ResourceVersion: "1",
		Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": selection},
	}}
}

func TestPodNetworkSelectionIndex(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		podNetworkSelectionIndex: podNetworkSelectionIndexFunc,
	})
	pods := []*corev1.Pod{
		networkIndexPod("local", "blue@net1"),
		networkIndexPod("remote", "infra/blue"),
		networkIndexPod("multiple", `[{"name":"blue"},{"name":"alias","namespace":"infra"},{"name":"blue","interface":"net3"}]`),
		networkIndexPod("unrelated", "green"),
		networkIndexPod("none", ""),
		networkIndexPod("invalid", "["),
	}
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	check := func(keys []string, want ...string) {
		t.Helper()
		objects, err := podsForNADs(indexer, keys)
		if err != nil {
			t.Fatal(err)
		}
		got := sets.New[string]()
		for _, obj := range objects {
			got.Insert(obj.(*corev1.Pod).Name)
		}
		if len(objects) != got.Len() || !got.Equal(sets.New(want...)) {
			t.Fatalf("NADs %v: got %v (%d callbacks), want %v", keys, got, len(objects), want)
		}
	}
	check([]string{"tenant/blue", "infra/alias", "tenant/blue"}, "local", "multiple", "invalid")
	check([]string{"infra/blue"}, "remote", "invalid")
	check(nil, "invalid")

	// Informer updates must remove the old index entries, including the
	// malformed-annotation fallback, and deletes must release them entirely.
	changed := pods[0].DeepCopy()
	changed.Annotations["k8s.v1.cni.cncf.io/networks"] = "green"
	if err := indexer.Update(changed); err != nil {
		t.Fatal(err)
	}
	repaired := pods[5].DeepCopy()
	repaired.Annotations["k8s.v1.cni.cncf.io/networks"] = "green"
	if err := indexer.Update(repaired); err != nil {
		t.Fatal(err)
	}
	check([]string{"tenant/blue"}, "multiple")
	if err := indexer.Delete(pods[2]); err != nil {
		t.Fatal(err)
	}
	check([]string{"tenant/blue", "infra/alias"})
}

func TestPodNetworkSelectionIndexMalformedJSON(t *testing.T) {
	for _, annotation := range []string{"[null]", `[{"name":"blue"},null]`, "[", `{"name":"blue"}`, `[{"name":42}]`} {
		t.Run(annotation, func(t *testing.T) {
			keys, err := podNetworkSelectionIndexFunc(networkIndexPod("invalid", annotation))
			if err != nil || len(keys) != 1 || keys[0] != invalidPodNetworkSelection {
				t.Fatalf("malformed selection must reach normal error handling: keys=%v, err=%v", keys, err)
			}
		})
	}
}

func TestPodNADSnapshotWhileSelectionChanges(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		podNetworkSelectionIndex: podNetworkSelectionIndexFunc,
	})
	if err := indexer.Add(networkIndexPod("moving", "blue")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			selection := "blue"
			if i%2 == 0 {
				selection = "alias"
			}
			if err := indexer.Update(networkIndexPod("moving", selection)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer wg.Wait()
	for i := 0; i < 2000; i++ {
		pods, err := podsForNADs(indexer, []string{"tenant/blue", "tenant/alias"})
		if err != nil || len(pods) != 1 {
			t.Fatalf("lost or duplicated a Pod moving between NAD aliases: count=%d, err=%v", len(pods), err)
		}
	}
}

// BenchmarkSecondaryPodStartup measures the snapshot and network-selection
// parsing for both startup passes. It excludes IP allocation/API writes and the
// factory's event queues, so it is not an end-to-end cluster startup benchmark.
func BenchmarkSecondaryPodStartup(b *testing.B) {
	const podCount = 71420
	const networkCount = 3600
	for _, annotated := range []int{2872, podCount} {
		b.Run(fmt.Sprintf("annotated=%d", annotated), func(b *testing.B) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
				podNetworkSelectionIndex: podNetworkSelectionIndexFunc,
			})
			for i := 0; i < podCount; i++ {
				selection := ""
				if i < annotated {
					selection = "["
					for j := 0; j < 10; j++ {
						if j > 0 {
							selection += ","
						}
						selection += fmt.Sprintf(`{"name":"net-%d"}`, (i*10+j)%networkCount)
					}
					selection += "]"
				}
				if err := indexer.Add(networkIndexPod(fmt.Sprintf("pod-%d", i), selection)); err != nil {
					b.Fatal(err)
				}
			}
			selected, err := podsForNADs(indexer, []string{"tenant/net-0"})
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("startup snapshot: %d total Pods, %d selected Pods", podCount, len(selected))
			for _, indexed := range []bool{false, true} {
				b.Run(fmt.Sprintf("indexed=%t", indexed), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						var objects []interface{}
						if indexed {
							var err error
							objects, err = podsForNADs(indexer, []string{"tenant/net-0"})
							if err != nil {
								b.Fatal(err)
							}
						} else {
							objects = indexer.List()
						}
						for pass := 0; pass < 2; pass++ {
							for _, obj := range objects {
								pod := obj.(*corev1.Pod)
								if pod.Annotations["k8s.v1.cni.cncf.io/networks"] == "" {
									continue
								}
								if _, err := nadutils.ParsePodNetworkAnnotation(pod); err != nil {
									b.Fatal(err)
								}
							}
						}
					}
				})
			}
		})
	}
}

// BenchmarkPodNetworkIndexUpdate exposes the ongoing cost of maintaining the
// index, including status-only updates where network selection is unchanged.
func BenchmarkPodNetworkIndexUpdate(b *testing.B) {
	for _, indexed := range []bool{false, true} {
		b.Run(fmt.Sprintf("indexed=%t", indexed), func(b *testing.B) {
			indexers := cache.Indexers{}
			if indexed {
				indexers[podNetworkSelectionIndex] = podNetworkSelectionIndexFunc
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, indexers)
			pod := networkIndexPod("pod", `[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"},{"name":"f"},{"name":"g"},{"name":"h"},{"name":"i"},{"name":"j"}]`)
			if err := indexer.Add(pod); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := indexer.Update(pod); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
