// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

var (
	benchmarkPodSelection PodTrafficSelection
	benchmarkOwners       map[string]*TrafficConflict
)

// Overlapping owners share five classes, as in the OKEP's scale example.
// Disjoint owners use separate classes to exercise comparisons without early exits.
func trafficBenchmarkInputs(ownerCount, classesPerOwner, matchesPerClass int, overlapping bool) ([]*eipv1.EgressIP, []*trafficv1.TrafficClass) {
	var owners []*eipv1.EgressIP
	var classes []*trafficv1.TrafficClass
	for i := range ownerCount {
		group := fmt.Sprint(i)
		if overlapping {
			group = "shared"
		}
		owners = append(owners, &eipv1.EgressIP{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("owner-%02d", i), CreationTimestamp: metav1.NewTime(time.Unix(int64(i), 0))},
			Spec: eipv1.EgressIPSpec{
				TrafficSelector:   &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": group}},
				NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "production"}},
				PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
			},
		})
		if overlapping && i != 0 {
			continue
		}
		for j := range classesPerOwner {
			class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("class-%d-%d", i, j), Labels: map[string]string{"traffic": group}}}
			for k := range matchesPerClass {
				index := (i*classesPerOwner+j)*matchesPerClass + k
				addr := netip.AddrFrom4([4]byte{198, 18, byte(index >> 8), byte(index)})
				class.Spec.Matches = append(class.Spec.Matches, trafficv1.TrafficMatch{
					DestinationCIDR: addr.String() + "/32", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{}}},
				})
			}
			classes = append(classes, class)
		}
	}
	return owners, classes
}

func BenchmarkPodTrafficResolution(b *testing.B) {
	for _, tc := range []struct {
		name        string
		overlapping bool
	}{
		{"disjoint", false},
		{"overlapping", true},
	} {
		owners, classes := trafficBenchmarkInputs(20, 5, 25, tc.overlapping)
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/cached=%t", tc.name, cached), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					// Include a cold resolution followed by reuse across 500 pods.
					var cache PodTrafficSelectionCache
					for range 500 {
						var err error
						if cached {
							benchmarkPodSelection, err = cache.Resolve(owners, classes)
						} else {
							benchmarkPodSelection, err = ResolvePodTraffic(owners, classes)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func BenchmarkTrafficConflictScan(b *testing.B) {
	for _, tc := range []struct {
		name                           string
		owners, pods, classes, matches int
		overlapping                    bool
		groups                         int
	}{
		{"small-disjoint", 5, 100, 1, 25, false, 1},
		{"reference-disjoint", 20, 500, 5, 25, false, 1},
		{"reference-overlapping", 20, 500, 5, 25, true, 1},
		{"reference-sparse", 20, 500, 5, 25, false, 4},
	} {
		owners, classes := trafficBenchmarkInputs(tc.owners, tc.classes, tc.matches, tc.overlapping)
		resolved, err := ResolvePodTraffic(owners, classes)
		if err != nil {
			b.Fatal(err)
		}
		pods := make([][]TrafficCandidate, tc.pods)
		for p := range pods {
			for i, owner := range owners {
				if i%tc.groups == p%tc.groups {
					pods[p] = append(pods[p], resolved[owner.Name].TrafficCandidate)
				}
			}
		}
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/cached=%t", tc.name, cached), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var cache TrafficConflictCache
					if cached {
						cache = make(TrafficConflictCache)
					}
					for _, candidates := range pods {
						benchmarkOwners = cache.SelectOwners(candidates)
					}
				}
			})
		}
	}
}
