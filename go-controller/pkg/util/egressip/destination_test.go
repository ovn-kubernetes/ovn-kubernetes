// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"net/netip"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

func TestParseDestinations(t *testing.T) {
	udp := trafficv1.ProtocolMatch{UDP: &trafficv1.PortProtocol{
		DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)},
		SourcePort:      &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}},
	}}
	raw := []trafficv1.TrafficMatch{
		{DestinationCIDR: "2001:db8::/32"},
		{DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{udp, udp, {SCTP: &trafficv1.PortProtocol{}}}},
		{DestinationCIDR: "192.0.2.0/24"},
	}
	matches, err := ParseDestinations(raw)
	if err != nil {
		t.Fatalf("parse traffic destinations: %v", err)
	}
	want := []Destination{
		{CIDR: netip.MustParsePrefix("192.0.2.0/24")},
		{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolUDP, DestinationPort: PortRange{5060, 5060}, SourcePort: PortRange{16384, 32767}},
		{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolSCTP},
		{CIDR: netip.MustParsePrefix("2001:db8::/32")},
	}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	cidrOnly, l4 := SplitByL4(matches)
	if len(cidrOnly) != 2 || len(l4) != 2 {
		t.Fatal("wrong CIDR/L4 partition")
	}

	for name, invalid := range map[string]trafficv1.TrafficMatch{
		"host bits":      {DestinationCIDR: "192.0.2.1/24"},
		"invalid CIDR":   {DestinationCIDR: "invalid"},
		"two protocols":  {DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{}, UDP: &trafficv1.PortProtocol{}}}},
		"empty port":     {DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{}}}}},
		"zero port":      {DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Number: ptr.To[int32](0)}}}}},
		"reversed range": {DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 99, End: 1}}}}}},
		"oversized port": {DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Number: ptr.To[int32](65536)}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDestinations([]trafficv1.TrafficMatch{invalid}); err == nil {
				t.Fatal("invalid match accepted")
			}
		})
	}
}

func TestDestinationOverlap(t *testing.T) {
	base := Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolUDP, DestinationPort: PortRange{5060, 5062}}
	for _, tc := range []struct {
		name    string
		other   Destination
		overlap bool
	}{
		{"CIDR-only", Destination{CIDR: base.CIDR}, true},
		{"other family", Destination{CIDR: netip.MustParsePrefix("2001:db8::/32")}, false},
		{"disjoint CIDR", Destination{CIDR: netip.MustParsePrefix("198.51.100.0/24")}, false},
		{"other protocol", Destination{CIDR: base.CIDR, Protocol: ProtocolTCP}, false},
		{"destination boundary", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, DestinationPort: PortRange{5062, 5100}}, true},
		{"disjoint ports", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, DestinationPort: PortRange{5063, 5100}}, false},
		{"source and destination can match one packet", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, SourcePort: PortRange{16384, 32767}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if base.Overlaps(tc.other) != tc.overlap || tc.other.Overlaps(base) != tc.overlap {
				t.Fatal("wrong overlap result")
			}
		})
	}
	a, b := base, base
	a.SourcePort, b.SourcePort = PortRange{100, 200}, PortRange{201, 300}
	if a.Overlaps(b) {
		t.Fatal("disjoint source ports overlap")
	}
}

func TestSelectorPresenceIndependentOfGate(t *testing.T) {
	before := config.OVNKubernetesFeature.EnableTrafficClass
	t.Cleanup(func() { config.OVNKubernetesFeature.EnableTrafficClass = before })
	spec := eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "test"}}}
	for _, enabled := range []bool{false, true} {
		config.OVNKubernetesFeature.EnableTrafficClass = enabled
		if !HasTrafficSelector(spec) || HasTrafficSelector(eipv1.EgressIPSpec{}) {
			t.Fatal("selector presence depends on gate")
		}
	}
}

func TestParseTrafficSelector(t *testing.T) {
	for _, selector := range []*metav1.LabelSelector{
		nil, {},
		{MatchLabels: map[string]string{"bad key": "one"}},
		{MatchLabels: map[string]string{"class": "bad value"}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpIn}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpExists, Values: []string{"one"}}}},
	} {
		parsed, err := ParseTrafficSelector(selector)
		if err == nil || parsed.Matches(labels.Set{}) || parsed.Matches(labels.Set{"class": "one"}) {
			t.Fatalf("invalid selector %+v did not fail closed: %v, %v", selector, parsed, err)
		}
	}
	parsed, err := ParseTrafficSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}})
	if err != nil || !parsed.Matches(labels.Set{"class": "one"}) || parsed.Matches(labels.Set{"class": "two"}) {
		t.Fatalf("valid selector lost its match: %v, %v", parsed, err)
	}
}

func TestUnsupportedProtocolEntries(t *testing.T) {
	eip := &eipv1.EgressIP{Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "one"}}}}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "mixed", Labels: map[string]string{"group": "one"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{}, {TCP: &trafficv1.PortProtocol{}}}}}}}
	got, err := ResolveTraffic(eip, []*trafficv1.TrafficClass{class})
	if err != nil || len(got.Destinations) != 1 || got.Destinations[0].Protocol != ProtocolTCP || len(got.Unsupported) != 1 || got.Unsupported[0].Class != "mixed" {
		t.Fatalf("mixed resolution: %+v, %v", got, err)
	}
	class.Spec.Matches[0].Protocols = []trafficv1.ProtocolMatch{{}}
	got, err = ResolveTraffic(eip, []*trafficv1.TrafficClass{class})
	if err != nil || len(got.Destinations) != 0 || got.ClassCount != 1 || len(got.Unsupported) != 1 {
		t.Fatalf("unsupported entries widened: %+v, %v", got, err)
	}
	class.Spec.Matches[0].Protocols = nil
	got, err = ResolveTraffic(eip, []*trafficv1.TrafficClass{class})
	if err != nil || len(got.Destinations) != 1 || got.Destinations[0].HasL4Filter() {
		t.Fatalf("CIDR wildcard lost: %+v, %v", got, err)
	}
}
