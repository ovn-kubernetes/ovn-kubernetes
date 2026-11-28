// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func selectionPod(namespace, annotation string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "pod", Annotations: map[string]string{nadapi.NetworkAttachmentAnnot: annotation}}}
}

func TestNetworkSelectionCacheSemantics(t *testing.T) {
	annotations := []string{"", "net, other/net@if1,net@if2", `[]`, `[{"name":"net"},{"name":"net","namespace":"other","interface":"eth2","ips":["10.0.0.1","2001:db8::1"],"mac":"00:11:22:33:44:55","infiniband-guid":"guid","ipam-claim-reference":"claim","portMappings":[{"hostPort":80,"containerPort":8080,"protocol":"tcp"}],"bandwidth":{"ingressRate":123},"cni-args":{"nested":[{"key":"value"}]},"default-route":["10.0.0.2","2001:db8::2"]}]`, `[`, "bad/name/format"}
	c := newNetworkSelectionCache(3, 4096)
	for _, namespace := range []string{"first", "second"} {
		for _, annotation := range annotations {
			pod := selectionPod(namespace, annotation)
			want, wantErr := nadutils.ParsePodNetworkAnnotation(pod)
			if annotation == "" {
				want, wantErr = []*nadapi.NetworkSelectionElement{}, nil
			}
			for range 2 {
				got, err := c.get(namespace, annotation)
				if (err != nil) != (wantErr != nil) {
					t.Fatalf("%q: got error %v, want %v", annotation, err, wantErr)
				}
				if err == nil && !reflect.DeepEqual(got, want) {
					t.Fatalf("%q: got %#v, want %#v", annotation, got, want)
				}
			}
		}
	}
}

func TestNetworkSelectionCopiesAreIndependent(t *testing.T) {
	p := selectionPod("ns", `[{"name":"net","ips":["10.0.0.1"],"portMappings":[{"hostPort":80}],"bandwidth":{"ingressRate":123},"cni-args":{"nested":[{"key":"value"}]},"default-route":["10.0.0.2"]}]`)
	want, err := nadutils.ParsePodNetworkAnnotation(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetK8sPodAllNetworkSelections(p)
	if err != nil {
		t.Fatal(err)
	}
	got[0].Name = "changed"
	got[0].IPRequest[0] = "changed"
	got[0].PortMappingsRequest[0].HostPort = 999
	got[0].BandwidthRequest.IngressRate = 999
	(*got[0].CNIArgs)["nested"].([]interface{})[0].(map[string]interface{})["key"] = "changed"
	got[0].GatewayRequest[0][0] = 99
	again, err := GetK8sPodAllNetworkSelections(p)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("cached data mutated: %v, %v", again, err)
	}
}

func TestNetworkSelectionCacheBoundsAndInterleaving(t *testing.T) {
	c := newNetworkSelectionCache(2, 40)
	for i := range 100 {
		if _, err := c.get("ns", fmt.Sprintf("net%d", i)); err != nil {
			t.Fatal(err)
		}
		if c.cache.Len() > 2 {
			t.Fatalf("cache exceeded entry bound: %d", c.cache.Len())
		}
	}
	if _, err := c.get("ns", strings.Repeat("a", 50)); err != nil {
		t.Fatal(err)
	}
	if c.cache.Len() != 2 {
		t.Fatal("oversized entry changed cache")
	}
	for _, annotation := range []string{"old", "new", "old", "new"} {
		got, err := c.get("ns", annotation)
		if err != nil || got[0].Name != annotation {
			t.Fatalf("wrong event version: %v %v", got, err)
		}
	}
}

func TestNetworkSelectionCacheAdmissionAndEviction(t *testing.T) {
	c := newNetworkSelectionCache(2, 40)
	get := func(annotation string) {
		t.Helper()
		got, err := c.get("ns", annotation)
		want, wantErr := nadutils.ParseNetworkAnnotation(annotation, "ns")
		if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("parse %q: got %v, %v; want %v, %v", annotation, got, err, want, wantErr)
		}
	}
	contains := func(annotation string) bool {
		_, ok := c.cache.Get(networkSelectionKey{namespace: "ns", annotation: annotation})
		return ok
	}
	get("first")
	get("second")
	get("first")
	get("third")
	if contains("second") || !contains("first") || !contains("third") {
		t.Fatal("inserting a third entry must evict the least recently used entry")
	}

	// Include namespace bytes in the admission threshold. Even a full cache
	// of admitted entries must fit the encoded-input budget.
	boundary := strings.Repeat("a", 18)
	get(boundary)
	if !contains(boundary) {
		t.Fatal("entry at the size limit was not admitted")
	}
	get(boundary + "a")
	if contains(boundary+"a") || c.cache.Len() != 2 || !contains(boundary) {
		t.Fatal("oversized input must parse without entering or displacing the cache")
	}
	if _, err := c.get("ns", "["); err == nil || contains("[") {
		t.Fatal("invalid input must return an error without entering the cache")
	}

	// A namespace change changes the parser's default namespace, even when
	// the annotation text is identical.
	for _, namespace := range []string{"first", "second", "first"} {
		got, err := c.get(namespace, "net")
		if err != nil || len(got) != 1 || got[0].Namespace != namespace {
			t.Fatalf("incorrect namespace for %q: %v, %v", namespace, got, err)
		}
	}
}

func TestNetworkSelectionCacheConcurrentEviction(t *testing.T) {
	c := newNetworkSelectionCache(4, 4096)
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				name := fmt.Sprintf("net%d-%d", worker, i)
				annotation := fmt.Sprintf(`[{"name":%q,"ips":["10.0.0.1"]}]`, name)
				got, err := c.get("ns", annotation)
				if err != nil || len(got) != 1 || got[0].Name != name || got[0].IPRequest[0] != "10.0.0.1" {
					t.Errorf("incorrect parse during eviction: %v, %v", got, err)
					return
				}
				copy := copyNetworkSelection(got[0])
				copy.IPRequest[0] = "changed"
				if got[0].IPRequest[0] != "10.0.0.1" {
					t.Error("copy mutated a shared parse result")
					return
				}
			}
		}()
	}
	wg.Wait()
	if c.cache.Len() > 4 {
		t.Fatalf("concurrent inserts exceeded the entry bound: %d", c.cache.Len())
	}
}

func TestNetworkSelectionMappingResolvesCurrentNADs(t *testing.T) {
	p := selectionPod("ns", "net,net@if2")
	n := &userDefinedNetInfo{netName: "network"}
	matches := true
	match := func(string) bool { return matches }
	for _, expected := range []bool{true, false, true} {
		matches = expected
		on, mapping, err := getPodNADToNetworkMappingWithPredicate(p, n, match)
		if err != nil || on != expected {
			t.Fatalf("resolution was cached: on=%v err=%v", on, err)
		}
		if expected {
			if len(mapping) != 2 {
				t.Fatalf("duplicates lost: %v", mapping)
			}
			for _, selection := range mapping {
				selection.Name = "mutated"
			}
		}
	}
}

func TestNetworkSelectionCacheConcurrentReaders(t *testing.T) {
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				p := selectionPod("ns", fmt.Sprintf("net%d", i%4))
				got, err := GetK8sPodAllNetworkSelections(p)
				if err != nil {
					t.Error(err)
					return
				}
				if got[0].Name != fmt.Sprintf("net%d", i%4) {
					t.Error("cross-call mutation")
				}
				got[0].Name = "mutated"
			}
		}()
	}
	wg.Wait()
}

func BenchmarkNetworkSelectionFanout(b *testing.B) {
	var entries []string
	for i := range 8 {
		entries = append(entries, fmt.Sprintf(`{"name":"net%d","namespace":"tenant","interface":"net%d"}`, i, i))
	}
	raw := "[" + strings.Join(entries, ",") + "]"
	p := selectionPod("tenant", raw)
	for _, fanout := range []int{1, 8, 16, 450} {
		b.Run(fmt.Sprintf("parse-each/%d", fanout), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for range fanout {
					if _, err := nadutils.ParsePodNetworkAnnotation(p); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
		b.Run(fmt.Sprintf("cached-mapping/%d", fanout), func(b *testing.B) {
			n := &userDefinedNetInfo{netName: "network"}
			b.ReportAllocs()
			for b.Loop() {
				for j := range fanout {
					match := func(key string) bool { return j < 8 && key == fmt.Sprintf("tenant/net%d", j) }
					if _, _, err := getPodNADToNetworkMappingWithPredicate(p, n, match); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
