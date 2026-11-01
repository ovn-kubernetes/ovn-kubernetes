// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

func TestTrafficOwnerSelection(t *testing.T) {
	x := Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24")}
	y := Destination{CIDR: netip.MustParsePrefix("198.51.100.0/24")}
	candidate := func(name string, age int64, matches ...Destination) TrafficCandidate {
		return TrafficCandidate{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(age, 0))}}, Destinations: matches}
	}
	candidates := []TrafficCandidate{candidate("a", 1, x), candidate("b", 2, x, y), candidate("c", 3, y)}
	got := SelectTrafficOwners(candidates)
	a, hasA := got["a"]
	c, hasC := got["c"]
	if !hasA || a != nil || got["b"] == nil || got["b"].Owner != "a" || !hasC || c != nil {
		t.Fatalf("rejected owner blocked C: %+v", got)
	}
	slices.Reverse(candidates)
	if reversed := SelectTrafficOwners(candidates); !reflect.DeepEqual(got, reversed) {
		t.Fatalf("event order changed winners: %+v", reversed)
	}
	candidates = []TrafficCandidate{candidate("a", 1), candidate("b", 2, x, y), candidate("c", 3, y)}
	got = SelectTrafficOwners(candidates)
	a, hasA = got["a"]
	b, hasB := got["b"]
	if !hasA || a != nil || !hasB || b != nil || got["c"] == nil || got["c"].Owner != "b" {
		t.Fatalf("previously active C kept preference: %+v", got)
	}
	got = SelectTrafficOwners([]TrafficCandidate{candidate("z", 1, x), candidate("a", 1, x)})
	a, hasA = got["a"]
	if !hasA || a != nil || got["z"] == nil || got["z"].Owner != "a" {
		t.Fatal("name tiebreaker lost")
	}
	got = SelectTrafficOwners([]TrafficCandidate{candidate("z", 1, x), candidate("a", 2, x)})
	z, hasZ := got["z"]
	if !hasZ || z != nil || got["a"] == nil || got["a"].Owner != "z" {
		t.Fatal("name overrode timestamp")
	}
	// A contained IPv6 prefix conflicts only when its port filters overlap too.
	supernet := Destination{CIDR: netip.MustParsePrefix("2001:db8::/64"), Protocol: ProtocolTCP, DestinationPort: PortRange{443, 443}}
	host := supernet
	host.CIDR = netip.MustParsePrefix("2001:db8::1/128")
	otherPort := host
	otherPort.DestinationPort = PortRange{8443, 8443}
	got = SelectTrafficOwners([]TrafficCandidate{candidate("a", 1, supernet), candidate("b", 2, otherPort), candidate("c", 3, host)})
	a, hasA = got["a"]
	b, hasB = got["b"]
	if len(got) != 3 || !hasA || a != nil || !hasB || b != nil || got["c"] == nil || got["c"].Owner != "a" {
		t.Fatalf("IPv6 prefix and port overlap selected the wrong owners: %+v", got)
	}
}

func TestPodTrafficSelectionIsolatesInvalidOwners(t *testing.T) {
	valid := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "healthy"}, Spec: eipv1.EgressIPSpec{
		TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}},
	}}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"traffic": "one"}},
		Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
	invalidSelector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Invalid"}}}
	for _, field := range []string{"namespace", "pod", "traffic"} {
		t.Run(field, func(t *testing.T) {
			invalid := valid.DeepCopy()
			invalid.Name = "invalid"
			switch field {
			case "namespace":
				invalid.Spec.NamespaceSelector = invalidSelector
			case "pod":
				invalid.Spec.PodSelector = invalidSelector
			case "traffic":
				invalid.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "traffic", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"bad value"}},
				}}
			}
			selection, err := ResolvePodTraffic([]*eipv1.EgressIP{invalid, valid}, []*trafficv1.TrafficClass{class})
			if err != nil {
				t.Fatalf("resolve peers with invalid %s selector: %v", field, err)
			}
			for _, name := range []string{"one", "two"} {
				selected := selection.SelectPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil)
				conflict, included := selected[valid.Name]
				if len(selected) != 1 || !included || conflict != nil {
					t.Fatalf("invalid owner blocked healthy peer for pod %s: %+v", name, selected)
				}
			}
		})
	}
}

func TestTrafficConflictCache(t *testing.T) {
	x := Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24")}
	y := Destination{CIDR: netip.MustParsePrefix("198.51.100.0/24")}
	candidates := []TrafficCandidate{
		{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "a"}}, Destinations: []Destination{x}},
		{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "b"}}, Destinations: []Destination{x, y}},
		{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "c"}}, Destinations: []Destination{y}},
		{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "d"}}, Destinations: []Destination{{CIDR: netip.MustParsePrefix("2001:db8::/64")}}},
		{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}},
	}
	cache := make(TrafficConflictCache)
	// Every pod can select a different subset. In particular, C wins beside A,
	// which rejects B, but loses beside B when A does not select that pod.
	for subset := range 1 << len(candidates) {
		var podOwners []TrafficCandidate
		for i, candidate := range candidates {
			if subset&(1<<i) != 0 {
				podOwners = append(podOwners, candidate)
			}
		}
		want := SelectTrafficOwners(podOwners)
		got := cache.SelectOwners(podOwners)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("subset %b: cached selection = %+v, want %+v", subset, got, want)
		}
		slices.Reverse(podOwners)
		reordered := cache.SelectOwners(podOwners)
		if !reflect.DeepEqual(reordered, want) {
			t.Fatalf("subset %b: reordered selection = %+v, want %+v", subset, reordered, want)
		}
		if reordered["b"] != got["b"] {
			t.Fatalf("subset %b: warm lookup did not reuse B's conflict", subset)
		}
	}
	if conflict, exists := cache[[2]string{"c", "a"}]; !exists || conflict != nil {
		t.Fatal("non-overlapping owner pair was not cached")
	}
	if conflict := cache[[2]string{"b", "a"}]; conflict == nil || conflict.Owner != "a" {
		t.Fatal("overlapping owner pair was not cached")
	}
}
