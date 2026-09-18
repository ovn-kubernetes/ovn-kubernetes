// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeimport

import (
	"errors"
	"net"
	"testing"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type routeStream struct {
	util.NetLinkOps
	routes []netlink.Route
	err    error
}

func (s routeStream) RouteListFilteredIter(family int, filter *netlink.Route, mask uint64, visit func(netlink.Route) bool) error {
	if family != netlink.FAMILY_ALL || filter.Table != 1000 || filter.Protocol != unix.RTPROT_BGP || mask != netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL {
		return errors.New("unexpected route filter")
	}
	for _, route := range s.routes {
		if !visit(route) {
			break
		}
	}
	return s.err
}

func TestBGPRoutesDiscardsInterruptedDump(t *testing.T) {
	_, dst, _ := net.ParseCIDR("192.0.2.0/24")
	for _, dumpErr := range []error{netlink.ErrDumpInterrupted, errors.New("receive failed")} {
		c := &controller{log: logr.Discard(), netlink: routeStream{
			routes: []netlink.Route{{Dst: dst, Gw: net.ParseIP("192.0.2.1")}}, err: dumpErr,
		}}
		got, err := c.getBGPRoutes(1000, nil, 0)
		if !errors.Is(err, dumpErr) || got != nil {
			t.Fatalf("partial dump must not become desired state: routes=%v err=%v", got, err)
		}
	}
}

func TestBGPRoutesStreamsBothFamilies(t *testing.T) {
	_, v4, _ := net.ParseCIDR("192.0.2.0/24")
	_, v6, _ := net.ParseCIDR("2001:db8::/64")
	c := &controller{log: logr.Discard(), netlink: routeStream{routes: []netlink.Route{
		{Dst: v4, Gw: net.ParseIP("192.0.2.1"), LinkIndex: 2},
		{Dst: v6, MultiPath: []*netlink.NexthopInfo{
			{Gw: net.ParseIP("fe80::1"), LinkIndex: 2},
			{Gw: net.ParseIP("fe80::2"), LinkIndex: 3},
		}},
	}}}
	got, err := c.getBGPRoutes(1000, nil, 2)
	if err != nil || len(got) != 2 || !got.Has(route{dst: v4.String(), gw: "192.0.2.1"}) || !got.Has(route{dst: v6.String(), gw: "fe80::1"}) {
		t.Fatalf("unexpected dual-stack/multipath routes: %v, %v", got, err)
	}
	got, err = c.getBGPRoutes(1000, []*net.IPNet{v4}, 2)
	if err != nil || len(got) != 1 || !got.Has(route{dst: v6.String(), gw: "fe80::1"}) {
		t.Fatalf("unexpected subnet-filtered routes: %v, %v", got, err)
	}
}

func BenchmarkBGPRoutesStreaming(b *testing.B) {
	routes := make([]netlink.Route, 10000)
	for i := range routes {
		routes[i] = netlink.Route{
			Dst: &net.IPNet{IP: net.IPv4(10, byte(i>>8), byte(i), 0), Mask: net.CIDRMask(24, 32)},
			Gw:  net.ParseIP("192.0.2.1"),
		}
	}
	c := &controller{log: logr.Discard(), netlink: routeStream{routes: routes}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := c.getBGPRoutes(1000, nil, 0)
		if err != nil || len(got) != len(routes) {
			b.Fatalf("routes=%d error=%v", len(got), err)
		}
	}
}
