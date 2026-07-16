// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestGetNicName(t *testing.T) {
	tests := []struct {
		desc      string
		errMatch  error
		outputExp string
		inpBrName string
		ovsData   []libovsdbtest.TestData
	}{
		{
			desc:      "bridge not present in ovsdb",
			errMatch:  fmt.Errorf("failed to get bridge"),
			inpBrName: "missing",
			ovsData:   []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}},
		},
		{
			desc:      "single system-typed port returns the port name",
			outputExp: "port1",
			inpBrName: "br0",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br0-uuid"}},
				&vswitchd.Bridge{UUID: "br0-uuid", Name: "br0", Ports: []string{"port1-uuid"}},
				&vswitchd.Port{UUID: "port1-uuid", Name: "port1", Interfaces: []string{"iface1-uuid"}},
				&vswitchd.Interface{UUID: "iface1-uuid", Name: "port1", Type: "system"},
			},
		},
		{
			desc:      "bridge-uplink external-id wins over system port",
			outputExp: "uplink-iface",
			inpBrName: "br0",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br0-uuid"}},
				&vswitchd.Bridge{UUID: "br0-uuid", Name: "br0", Ports: []string{"port1-uuid"}, ExternalIDs: map[string]string{"bridge-uplink": "uplink-iface"}},
				&vswitchd.Port{UUID: "port1-uuid", Name: "port1", Interfaces: []string{"iface1-uuid"}},
				&vswitchd.Interface{UUID: "iface1-uuid", Name: "port1", Type: "system"},
			},
		},
		{
			desc:      "no system port, bridge-uplink external-id wins over br-prefix fallback",
			outputExp: "uplink-iface",
			inpBrName: "brOther",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-uuid"}},
				&vswitchd.Bridge{UUID: "br-uuid", Name: "brOther", Ports: []string{"port-uuid"}, ExternalIDs: map[string]string{"bridge-uplink": "uplink-iface"}},
				&vswitchd.Port{UUID: "port-uuid", Name: "internal-port", Interfaces: []string{"iface-uuid"}},
				&vswitchd.Interface{UUID: "iface-uuid", Name: "internal-port", Type: "internal"},
			},
		},
		{
			desc:      "no system port, no bridge-uplink, br-prefix is stripped",
			outputExp: "Name",
			inpBrName: "brName",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-uuid"}},
				&vswitchd.Bridge{UUID: "br-uuid", Name: "brName", Ports: []string{"port-uuid"}},
				&vswitchd.Port{UUID: "port-uuid", Name: "internal-port", Interfaces: []string{"iface-uuid"}},
				&vswitchd.Interface{UUID: "iface-uuid", Name: "internal-port", Type: "internal"},
			},
		},
		{
			desc:      "no system port, no bridge-uplink, no br-prefix returns explicit error",
			outputExp: "",
			errMatch:  fmt.Errorf("unable to resolve uplink for bridge"),
			inpBrName: "noprefix",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-uuid"}},
				&vswitchd.Bridge{UUID: "br-uuid", Name: "noprefix", Ports: []string{"port-uuid"}},
				&vswitchd.Port{UUID: "port-uuid", Name: "internal-port", Interfaces: []string{"iface-uuid"}},
				&vswitchd.Interface{UUID: "iface-uuid", Name: "internal-port", Type: "internal"},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: tc.ovsData})
			require.NoError(t, err, "harness setup")
			t.Cleanup(cleanup.Cleanup)

			res, err := GetNicName(ovsClient, tc.inpBrName)
			t.Log(res, err)

			if tc.errMatch != nil {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.outputExp, res)
			}
		})
	}
}

func TestSaveIPAddress(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpAddrs                 []netlink.Addr
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:       "empty address list, LinkSetup(newLink) succeeds",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:       "deleting address from old link errors out",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "adding address to new link errors out",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "saving IP address to new link succeeds",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := saveIPAddress(tc.inpNewLink, tc.inpOldLink, tc.inpAddrs)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestDelAddRoute(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		errExp                   bool
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpRoute                 netlink.Route
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:       "test path where RouteDel() fails",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test path where RouteAdd() fails",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 1}}},
			},
		},
		{
			desc:       "test success path",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := delAddRoute(tc.inpOldLink, tc.inpNewLink, tc.inpRoute)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSaveRoute(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		errExp                   bool
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpRoutes                []netlink.Route
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc: "providing empty routes should return no error",
		},
		{
			desc:       "test error path for GCE special case",
			errExp:     true,
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Dst: ovntest.MustParseIPNet("192.168.1.0/24"), Gw: ovntest.MustParseIP("10.10.10.1")},
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test error path for when adding default gateway",
			errExp:     true,
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test success path",
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := saveRoute(tc.inpOldLink, tc.inpNewLink, tc.inpRoutes)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestNicToBridge(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	nicHWAddr, err := net.ParseMAC("00:11:22:33:44:55")
	require.NoError(t, err)
	randomHWAddr, err := net.ParseMAC("66:77:88:99:aa:bb")
	require.NoError(t, err)

	rootOvs := func() []libovsdbtest.TestData {
		return []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}}
	}

	tests := []struct {
		desc                     string
		inpIface                 string
		outBridge                string
		errExp                   bool
		initialOvsData           []libovsdbtest.TestData
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:           "invalid interface name fails to return a link",
			inpIface:       "",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:     "bridge creation fails when Open_vSwitch root row is missing",
			inpIface: "eth0",
			errExp:   true,
			// No Open_vSwitch root row → CreateOrUpdateNicBridge mutates a
			// non-existent row and fails.
			initialOvsData: []libovsdbtest.TestData{},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:           "IP address retrieval for link fails",
			inpIface:       "eth0",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:           "Route retrieval for link fails",
			inpIface:       "eth0",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:           "Retrieving link by bridge name fails",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Dst: ovntest.MustParseIPNet("10.168.1.0/24")}}, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
				{OnCallMethodName: "IsLinkNotFoundError", OnCallMethodArgType: []string{"*errors.errorString"}, RetArgList: []interface{}{false}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:           "Saving IP address to bridge fails",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 2},
			},
		},
		{
			desc:           "Saving routes to bridge fails",
			errExp:         true,
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:           "IP address and Routes of interface to OVS bridge succeeds",
			inpIface:       "eth0",
			outBridge:      "breth0",
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:           "bridge is not used until it inherits the NIC MAC address",
			inpIface:       "eth0",
			outBridge:      "breth0",
			initialOvsData: rootOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
				// the bridge netdev first materialises with a random MAC
				// address, then ovs-vswitchd applies the NIC MAC address
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "eth0", HardwareAddr: nicHWAddr}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "breth0", HardwareAddr: randomHWAddr}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "breth0", HardwareAddr: nicHWAddr}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			// reset the shared mocks so that one case cannot leak
			// expectations or recorded calls into the next
			mockNetLinkOps.Mock.ExpectedCalls = nil
			mockNetLinkOps.Mock.Calls = nil
			mockLink.Mock.ExpectedCalls = nil
			mockLink.Mock.Calls = nil
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: tc.initialOvsData})
			require.NoError(t, err, "harness setup")
			t.Cleanup(cleanup.Cleanup)

			res, err := NicToBridge(ovsClient, tc.inpIface)
			t.Log(res, err)
			if tc.errExp {
				require.Error(t, err)
				assert.Empty(t, res)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.outBridge, res)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestBridgeToNic(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	bridgeUUID := "bridge-uuid"
	patchPortUUID := "patch-port-uuid"
	patchIfaceUUID := "patch-iface-uuid"
	normalPortUUID := "normal-port-uuid"
	normalIfaceUUID := "normal-iface-uuid"
	brIntUUID := "br-int-uuid"
	peerPortUUID := "peer-port-uuid"
	peerIfaceUUID := "peer-iface-uuid"

	bridgeWithPatch := func() []libovsdbtest.TestData {
		return []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, brIntUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: "breth0", Ports: []string{patchPortUUID, normalPortUUID}},
			&vswitchd.Port{UUID: patchPortUUID, Name: "patch-breth0-to-br-int", Interfaces: []string{patchIfaceUUID}},
			&vswitchd.Interface{UUID: patchIfaceUUID, Name: "patch-breth0-to-br-int", Type: "patch", Options: map[string]string{"peer": "patch-br-int-to-breth0"}},
			&vswitchd.Port{UUID: normalPortUUID, Name: "eth0", Interfaces: []string{normalIfaceUUID}},
			&vswitchd.Interface{UUID: normalIfaceUUID, Name: "eth0", Type: "system"},
			&vswitchd.Bridge{UUID: brIntUUID, Name: "br-int", Ports: []string{peerPortUUID}},
			&vswitchd.Port{UUID: peerPortUUID, Name: "patch-br-int-to-breth0", Interfaces: []string{peerIfaceUUID}},
			&vswitchd.Interface{UUID: peerIfaceUUID, Name: "patch-br-int-to-breth0", Type: "patch", Options: map[string]string{"peer": "patch-breth0-to-br-int"}},
		}
	}

	minimalOvs := func() []libovsdbtest.TestData {
		return []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}}
	}

	// netlink prelude shared by tests that get past the early netlink-only failures.
	netlinkPrelude := []ovntest.TestifyMockHelper{
		// bridgeLink, err := netlink.LinkByName(bridge)
		{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
		// addrs, err := netlink.AddrList(bridgeLink, ...)
		{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
		// routes, err := netlink.RouteList(bridgeLink, ...)
		{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
		// ifaceLink, err := netlink.LinkByName(nicName)
		{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
		// saveIPAddress: AddrDel, AddrAdd, LinkSetUp
		{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
		{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
		{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
		// saveRoute: RouteDel, RouteAdd
		{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
		{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
	}
	linkAttrsPrelude := []ovntest.TestifyMockHelper{
		// saveIPAddress's call to newLink.Attrs() (for addr.Label)
		{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
		// saveRoute -> delAddRoute -> newLink.Attrs().Index
		{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
	}
	tests := []struct {
		desc                     string
		inpBridge                string
		errExp                   bool
		initialOvsData           []libovsdbtest.TestData
		expectedOvsData          []libovsdbtest.TestData
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:           "invalid bridge name fails to return a link",
			inpBridge:      "brinvalid",
			errExp:         true,
			initialOvsData: minimalOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:           "IP address retrieval for link fails",
			inpBridge:      "breth0",
			errExp:         true,
			initialOvsData: minimalOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:           "Route retrieval for link fails",
			inpBridge:      "breth0",
			errExp:         true,
			initialOvsData: minimalOvs(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:           "retrieving interface link using nic name fails",
			inpBridge:      "breth0",
			errExp:         true,
			initialOvsData: bridgeWithPatch(),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:           "deletes bridge and removes patch peer from br-int; non-patch ports are ignored",
			inpBridge:      "breth0",
			errExp:         false,
			initialOvsData: bridgeWithPatch(),
			expectedOvsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{brIntUUID}},
				&vswitchd.Bridge{UUID: brIntUUID, Name: "br-int"},
			},
			onRetArgsNetLinkLibOpers: netlinkPrelude,
			onRetArgsLinkIfaceOpers:  linkAttrsPrelude,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: tc.initialOvsData})
			require.NoError(t, err, "harness setup")
			t.Cleanup(cleanup.Cleanup)

			err = BridgeToNic(ovsClient, tc.inpBridge)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.expectedOvsData != nil {
				matcher := libovsdbtest.HaveData(tc.expectedOvsData)
				ok, mErr := matcher.Match(ovsClient)
				assert.True(t, ok, matcher.FailureMessage(ovsClient))
				require.NoError(t, mErr)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}
