// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

const maxPort = 65535

// Protocol is an IP protocol number. Its zero value matches all protocols.
type Protocol int

const (
	ProtocolTCP  Protocol = unix.IPPROTO_TCP
	ProtocolUDP  Protocol = unix.IPPROTO_UDP
	ProtocolSCTP Protocol = unix.IPPROTO_SCTP
)

// PortRange is an inclusive port interval. Its zero value matches any port.
type PortRange struct {
	Start uint16
	End   uint16
}

// Destination is a validated, single-protocol traffic match. Protocol zero matches all traffic.
type Destination struct {
	CIDR            netip.Prefix
	Protocol        Protocol
	DestinationPort PortRange
	SourcePort      PortRange
}

// String describes the CIDR and any L4 restrictions for diagnostics.
func (d Destination) String() string {
	if !d.HasL4Filter() {
		return d.CIDR.String()
	}
	return fmt.Sprintf("%s %s destination ports %s source ports %s", d.CIDR, strings.ToUpper(d.ProtocolName()), d.DestinationPort, d.SourcePort)
}

// String describes the inclusive interval, including the wildcard zero value.
func (p PortRange) String() string {
	if p == (PortRange{}) {
		return "any"
	}
	if p.Start == p.End {
		return fmt.Sprint(p.Start)
	}
	return fmt.Sprintf("%d-%d", p.Start, p.End)
}

// HasTrafficSelector reports whether traffic selection is specified for this EgressIP.
func HasTrafficSelector(spec eipv1.EgressIPSpec) bool {
	return spec.TrafficSelector != nil
}

// ParseTrafficSelector returns a selector matching nothing on invalid input.
// Callers handle an absent trafficSelector as catch-all before calling this helper.
func ParseTrafficSelector(selector *metav1.LabelSelector) (labels.Selector, error) {
	if selector == nil || len(selector.MatchLabels)+len(selector.MatchExpressions) == 0 {
		return labels.Nothing(), fmt.Errorf("trafficSelector must specify matchLabels or matchExpressions")
	}
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return labels.Nothing(), fmt.Errorf("invalid trafficSelector: %w", err)
	}
	return parsed, nil
}

// HasL4Filter reports whether this destination restricts the transport protocol.
func (d Destination) HasL4Filter() bool { return d.Protocol != 0 }

// ProtocolName returns the lowercase protocol name, or empty for an unrestricted match.
func (d Destination) ProtocolName() string {
	switch d.Protocol {
	case ProtocolTCP:
		return "tcp"
	case ProtocolUDP:
		return "udp"
	case ProtocolSCTP:
		return "sctp"
	default:
		return ""
	}
}

// ParseDestinations validates and expands protocol lists, removing exact duplicates.
func ParseDestinations(destinations []trafficv1.TrafficMatch) ([]Destination, error) {
	unique := map[Destination]struct{}{}
	for _, destination := range destinations {
		cidr, err := netip.ParsePrefix(destination.DestinationCIDR)
		if err != nil || cidr != cidr.Masked() {
			return nil, fmt.Errorf("invalid destination network %q", destination.DestinationCIDR)
		}
		if len(destination.Protocols) == 0 {
			unique[Destination{CIDR: cidr}] = struct{}{}
			continue
		}
		for _, entry := range destination.Protocols {
			if entry.TCP == nil && entry.UDP == nil && entry.SCTP == nil {
				continue // A newer schema can expose protocols this client cannot decode.
			}
			match, err := parseProtocolMatch(cidr, entry)
			if err != nil {
				return nil, err
			}
			unique[match] = struct{}{}
		}
	}
	result := make([]Destination, 0, len(unique))
	for d := range unique {
		result = append(result, d)
	}
	slices.SortFunc(result, compareDestination)
	return result, nil
}

func parseProtocolMatch(cidr netip.Prefix, entry trafficv1.ProtocolMatch) (Destination, error) {
	var ports *trafficv1.PortProtocol
	var protocol Protocol
	count := 0
	for _, candidate := range []struct {
		protocol Protocol
		ports    *trafficv1.PortProtocol
	}{
		{ProtocolTCP, entry.TCP}, {ProtocolUDP, entry.UDP}, {ProtocolSCTP, entry.SCTP},
	} {
		if candidate.ports != nil {
			protocol, ports = candidate.protocol, candidate.ports
			count++
		}
	}
	if count != 1 {
		return Destination{}, fmt.Errorf("destination %s must specify exactly one protocol per entry", cidr)
	}
	dst, err := parsePort(ports.DestinationPort)
	if err != nil {
		return Destination{}, fmt.Errorf("destination %s destinationPort: %w", cidr, err)
	}
	src, err := parsePort(ports.SourcePort)
	if err != nil {
		return Destination{}, fmt.Errorf("destination %s sourcePort: %w", cidr, err)
	}
	return Destination{CIDR: cidr, Protocol: protocol, DestinationPort: dst, SourcePort: src}, nil
}

func compareDestination(a, b Destination) int {
	if cmp := a.CIDR.Addr().Compare(b.CIDR.Addr()); cmp != 0 {
		return cmp
	}
	av := []int{a.CIDR.Bits(), int(a.Protocol), int(a.DestinationPort.Start), int(a.DestinationPort.End), int(a.SourcePort.Start), int(a.SourcePort.End)}
	bv := []int{b.CIDR.Bits(), int(b.Protocol), int(b.DestinationPort.Start), int(b.DestinationPort.End), int(b.SourcePort.Start), int(b.SourcePort.End)}
	return slices.Compare(av, bv)
}

func parsePort(port *trafficv1.Port) (PortRange, error) {
	if port == nil {
		return PortRange{}, nil
	}
	if (port.Number == nil) == (port.Range == nil) {
		return PortRange{}, fmt.Errorf("exactly one of number or range is required")
	}
	var start, end int32
	if port.Number != nil {
		start, end = *port.Number, *port.Number
	} else {
		start, end = port.Range.Start, port.Range.End
	}
	if start < 1 || end > maxPort || end < start {
		return PortRange{}, fmt.Errorf("invalid port range %d-%d", start, end)
	}
	return PortRange{Start: uint16(start), End: uint16(end)}, nil
}

// SplitByL4 separates address-set-compatible matches from inline L4 matches.
func SplitByL4(destinations []Destination) (cidrOnly, l4 []Destination) {
	for _, d := range destinations {
		if d.HasL4Filter() {
			l4 = append(l4, d)
		} else {
			cidrOnly = append(cidrOnly, d)
		}
	}
	return
}

// Overlaps reports whether a packet can satisfy both destinations.
func (d Destination) Overlaps(other Destination) bool {
	if !d.CIDR.Overlaps(other.CIDR) {
		return false
	}
	if !d.HasL4Filter() || !other.HasL4Filter() {
		return true
	}
	return d.Protocol == other.Protocol && d.DestinationPort.overlaps(other.DestinationPort) && d.SourcePort.overlaps(other.SourcePort)
}

func (p PortRange) overlaps(other PortRange) bool {
	if p == (PortRange{}) || other == (PortRange{}) {
		return true
	}
	return p.Start <= other.End && other.Start <= p.End
}
