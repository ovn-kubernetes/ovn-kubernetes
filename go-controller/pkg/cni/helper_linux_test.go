// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	kexec "k8s.io/utils/exec"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	cni_type_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/cni/pkg/types"
	cni_ns_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/plugins/pkg/ns"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestRenameLink(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

	tests := []struct {
		desc                 string
		inpCurrName          string
		inpNewName           string
		errExp               bool
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:        "test code path when LinkByName() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetDown() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetName() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetUp() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test success code path",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			err := renameLink(tc.inpCurrName, tc.inpNewName)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}

func TestMoveIfToNetns(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNetNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

	tests := []struct {
		desc                 string
		inpIfaceName         string
		inpNetNs             ns.NetNS
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		netNsOpsMockHelper   []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when LinkByName() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     nil,
			errMatch:     fmt.Errorf("failed to lookup device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:         "test code path when LinkSetNsFd() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			errMatch:     fmt.Errorf("failed to move device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test success path",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNetNS.Mock, tc.netNsOpsMockHelper)

			err := moveIfToNetns(tc.inpIfaceName, tc.inpNetNs)
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNetNS.AssertExpectations(t)
		})
	}
}

func TestSafeMoveIfToNetns(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNetNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

	tests := []struct {
		desc                 string
		inpIfaceName         string
		inpNetNs             ns.NetNS
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		netNsOpsMockHelper   []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when LinkSetNsFd() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			errMatch:     fmt.Errorf("failed to move device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test code path when LinkSetNsFd() returns 'file exists' error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// error when moving netdevice to namespace
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("file exists")}},
				// rename the netdevice before moving
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// move netdevice to namespace
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test success path",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNetNS.Mock, tc.netNsOpsMockHelper)

			_, err := safeMoveIfToNetns(tc.inpIfaceName, tc.inpNetNs, "containerID")
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNetNS.AssertExpectations(t)
		})
	}
}

func TestSetupNetwork(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

	tests := []struct {
		desc                 string
		inpLink              netlink.Link
		inpPodIfaceInfo      *PodInterfaceInfo
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
	}{
		{
			desc:    "test code path when AddrAdd returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
				},
			},
			errMatch: fmt.Errorf("failed to add IP addr"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:    "test code path when AddRoute for gateway returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
				},
			},
			errMatch: fmt.Errorf("failed to add gateway route"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test code path when AddRoute for pod returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			errMatch: fmt.Errorf("failed to add pod route"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test success path",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test container link already set up",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Flags: net.FlagUp}}, CallTimes: 3},
			},
		},
		{
			desc:    "test skip ip config",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				SkipIPConfig: true,
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)

			err := setupNetwork(tc.inpLink, tc.inpPodIfaceInfo)
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSetupInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNS := new(cni_ns_mocks.NetNS)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

	/* Need the below to test the Do() function that requires root and needs to be figured out
	testOSNameSpace, err := ns.GetCurrentNS()
	if err != nil {
		t.Log(err)
		t.Fatal("failed to get NameSpace for test")
	}*/

	tests := []struct {
		desc                 string
		inpNetNS             ns.NetNS
		inpContID            string
		inpIfaceName         string
		inpPodIfaceInfo      *PodInterfaceInfo
		errExp               bool
		errMatch             error
		nsMockHelper         []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when Do() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
			},
			errExp: true,
			nsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		/* TODO: Running the below requires root, need to figure this out
		// `sudo -E /usr/local/go/bin/go test -v -run TestSetupInterface` would be the command, but mocking SetupVeth() mock is a challenge
		{
			desc:         "test code path when SetupVeth() returns error",
			inpNetNS:     testOSNameSpace,
			inpContID:    "test",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
			},
			errExp: true,
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{"SetupVeth",[]string{"string", "string", "int", "*ns.NetNS"}, []interface{}{nil, nil, fmt.Errorf("mock error")}},
			},
		},*/
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)

			hostIface, contIface, err := setupInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo)
			t.Log(hostIface, contIface, err)
			if tc.errExp {
				require.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNS.AssertExpectations(t)
		})
	}
}

func TestSetupSriovInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)
	mockNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	// set `sriovnetOps` in util/sriovnet_linux.go to a mock instance for unit tests execution
	util.SetSriovnetOpsInst(mockSriovnetOps)

	res, err := sriovnet.GetUplinkRepresentor("0000:01:00.0")
	t.Log(res, err)
	/* Need the below to test the Do() function that requires root and needs to be figured out
	testOSNameSpace, err := ns.GetCurrentNS()
	if err != nil {
		t.Log(err)
		t.Fatal("failed to get NameSpace for test")
	}*/

	netNsDoForward := &cni_ns_mocks.NetNS{}
	netNsDoForward.On("Fd", mock.Anything).Return(uintptr(0))
	var netNsDoError error
	netNsDoForward.On("Do", mock.AnythingOfType("func(ns.NetNS) error")).Run(func(args mock.Arguments) {
		do := args.Get(0).(func(ns.NetNS) error)
		netNsDoError = do(nil)
	}).Return(nil)

	tests := []struct {
		desc                 string
		inpNetNS             ns.NetNS
		inpContID            string
		inpIfaceName         string
		inpPodIfaceInfo      *PodInterfaceInfo
		inpPCIAddrs          string
		errExp               bool
		errMatch             error
		nsMockHelper         []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		sriovOpsMockHelper   []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
		onRetArgsKexecIface  []ovntest.TestifyMockHelper
		onRetArgsCmdList     []ovntest.TestifyMockHelper
		runnerInstance       kexec.Interface
	}{
		{
			desc:         "test code path when moveIfToNetns() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:         "test code path when Do() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:         "test code path when GetUplinkRepresentor() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when GetVfIndexByPciAddress() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{-1, fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when GetVfRepresentor() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when retrieving LinkByName() for host interface errors out",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			errExp:      true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for SetVFHardwreAddress()
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetVfHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "int", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				// The below mock call is for the LinkByName() of the host representor
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test success code path for non-DPU host mode",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for SetVFHardwreAddress()
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetVfHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "int", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				// The below mock call is for the LinkByName() of the host representor
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is to retrieve the MAC address of the host representor
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "VFRepresentor", HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01")}}},
			},
		},
		{
			desc:         "test code path when working in DPUHost mode",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             false,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			runnerInstance:     mockKexecIface,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when container LinkByName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetDown() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetUp() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container second LinkByName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetHardwareAddr() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetMTU() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container second LinkSetUp() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork AddrAdd() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork Gateways AddRoute() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}, CallTimes: 3},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork Routes AddRoute() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}, CallTimes: 3},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			netNsDoError = nil
			hostIface, contIface, err := setupSriovInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo, tc.inpPCIAddrs, false)
			t.Log(hostIface, contIface, err)
			if err == nil {
				err = netNsDoError
			}
			if tc.errExp {
				require.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNS.AssertExpectations(t)
			mockSriovnetOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestPodRequest_deletePodConntrack(t *testing.T) {
	mockTypeResult := new(cni_type_mocks.Result)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	tests := []struct {
		desc                 string
		inpPodRequest        PodRequest
		inpPrevResult        *current.Result
		resultMockHelper     []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc: "test code path when CNIConf.PrevResult == nil",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: nil,
					},
				},
			},
		},
		{
			desc: "test code path NewResultFromResult returns error",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			resultMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Version", OnCallMethodArgType: []string{}, RetArgList: []interface{}{"0.0.0"}},
			},
		},
		{
			desc: "test code path when ip.Interface != nil and path when Sandbox is empty value",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.1.0",
				Interfaces: []*current.Interface{{Name: "eth0"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
		},
		{
			desc: "test code path when DeleteConntrack returns error",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.1.0",
				Interfaces: []*current.Interface{{Name: "eth0", Sandbox: "blah"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilters", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), fmt.Errorf("mock error")}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockTypeResult.Mock, tc.resultMockHelper)
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)

			if tc.inpPrevResult != nil {
				res, err := json.Marshal(tc.inpPrevResult)
				if err != nil {
					t.Log(err)
					t.Fatal("json marshal error, test input invalid for inpPrevResult")
				} else {
					tc.inpPodRequest.CNIConf.PrevResult, err = current.NewResult(res)
					if err != nil {
						t.Fatal("NewResult failed", err)
					}
				}
			}
			tc.inpPodRequest.deletePodConntrack()
			mockTypeResult.AssertExpectations(t)
		})
	}
}

func TestConfigureOVS(t *testing.T) {
	mockLink := new(netlink_mocks.Link)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)

	util.SetNetLinkOpMockInst(mockNetLinkOps)
	util.SetSriovnetOpsInst(mockSriovnetOps)

	vfPciAddress := "0000:c5:03.1"
	fakeIP := "192.168.1.1/24"
	ip, ipnet, _ := net.ParseCIDR(fakeIP)
	ipnet.IP = ip
	sandboxID := "deadbeef"
	ovnPfEncapIpMapping := "enp1s0f0:10.0.0.1,enp193s0f0:10.0.0.193,enp197s0f0:10.0.0.197"

	tests := []struct {
		desc                  string
		podNs                 string
		podName               string
		vfRep                 string
		ifInfo                *PodInterfaceInfo
		ovnPfEncapIpMapping   string
		errMatch              error
		pfEncapIp             string
		execMock              *ovntest.FakeExec
		sriovnetOpsMockHelper []ovntest.TestifyMockHelper
		netLinkOpsMockHelper  []ovntest.TestifyMockHelper
	}{
		{
			desc:    "VF representor has matching external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "VF representor has no matching external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp999s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp999s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp999s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "empty external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping:   "", // no external_ids:ovn-pf-encap-ip-mapping config
			errMatch:              nil,
			pfEncapIp:             "", // ovs port added without encap-ip
			execMock:              ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "ignore get SR-IOV uplink representor failure",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "", // ovs port added without encap-ip
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "GetUplinkRepresentor",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{"", fmt.Errorf("failed to lookup")},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "LinkByName fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to find interface"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:    "LinkSetMTU fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to set MTU on"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:    "LinkSetUp fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to set link UP on"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovnetOpsMockHelper)

			err := util.SetExec(tc.execMock)
			require.NoError(t, err)
			err = SetExec(tc.execMock)
			require.NoError(t, err)

			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id=ns-foo_pod-bar"),
			})

			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSFindCmd("30", "Interface", "external_ids", "name="+tc.vfRep),
				Output: "",
			})

			// getPfEncapIP()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: tc.ovnPfEncapIpMapping,
			})

			// ovs-vsctl add port to br-int
			ovsAddPortCmd := fmt.Sprintf(
				"ovs-vsctl --timeout=30 --may-exist "+
					"add-port br-int %s other_config:transient=true "+
					"-- set interface %s external_ids:attached_mac=%s "+
					"external_ids:iface-id=%s external_ids:iface-id-ver=%s "+
					"external_ids:sandbox=%s ",
				tc.vfRep, tc.vfRep, "", genIfaceID(tc.podNs, tc.podName), tc.ifInfo.PodUID, sandboxID)
			if tc.pfEncapIp != "" {
				ovsAddPortCmd += fmt.Sprintf("external_ids:encap-ip=%s ", tc.pfEncapIp)
			}
			ovsAddPortCmd += fmt.Sprintf("external_ids:ip_addresses=%s external_ids:vf-netdev-name=%s "+
				"-- --if-exists remove interface %s external_ids %s -- --if-exists remove interface %s external_ids %s",
				fakeIP, tc.ifInfo.NetdevName, tc.vfRep, ovntypes.NetworkExternalID, tc.vfRep, ovntypes.NADExternalID)
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: ovsAddPortCmd,
				Err: nil,
			})

			// clearPodBandwidth()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "interface", "name",
					fmt.Sprintf("external-ids:sandbox=%s", sandboxID)),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "qos", "_uuid",
					fmt.Sprintf("external-ids:sandbox=%s", sandboxID)),
			})

			// waitForPodInterface()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Interface", tc.vfRep, "external-ids", "iface-id") + " " + "external-ids:ovn-installed",
				Output: genIfaceID(tc.podNs, tc.podName) + "\n" + "true",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Interface", tc.vfRep, "external-ids", "iface-id"),
				Output: genIfaceID(tc.podNs, tc.podName),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=8,dl_src="),
				Output: "non-empty-output",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=0,in_port=1"),
				Output: "non-empty-output",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=48,ip,ip_dst=" + strings.Split(fakeIP, "/")[0]),
				Output: "non-empty-output",
			})

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			var pod corev1.Pod
			pod.UID = "xyz"
			podNamespaceLister := v1mocks.PodNamespaceLister{}
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
			var podLister v1mocks.PodLister
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
			fakeClient := fake.NewSimpleClientset(&corev1.PodList{Items: []corev1.Pod{pod}})
			clientset := NewClientSet(fakeClient, &podLister)
			err = ConfigureOVS(ctx, nil, tc.podNs, tc.podName, "", tc.vfRep,
				tc.ifInfo, sandboxID, vfPciAddress, false, clientset)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}

			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestConfigureOVS_getPfEncapIpWithError(t *testing.T) {
	mockLink := new(netlink_mocks.Link)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)

	util.SetNetLinkOpMockInst(mockNetLinkOps)
	util.SetSriovnetOpsInst(mockSriovnetOps)

	vfPciAddress := "0000:c5:03.1"
	fakeIP := "192.168.1.1/24"
	ip, ipnet, _ := net.ParseCIDR(fakeIP)
	ipnet.IP = ip
	sandboxID := "deadbeef"
	vfRep := "enp1s0f0_1"

	tests := []struct {
		desc                  string
		podNs                 string
		podName               string
		vfRep                 string
		ifInfo                *PodInterfaceInfo
		ovnPfEncapIpMapping   string
		errMatch              error
		pfEncapIp             string
		execMock              *ovntest.FakeExec
		execMockCommands      []*ovntest.ExpectedCmd
		sriovnetOpsMockHelper []ovntest.TestifyMockHelper
		netLinkOpsMockHelper  []ovntest.TestifyMockHelper
	}{
		{
			desc:    "ovs get external_ids:ovn-pf-encap-ip-mapping failed",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   vfRep,
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				NetName:    ovntypes.DefaultNetworkName,
				NetdevName: "enp1s0f0v1",
				PodUID:     "xyz",
			},
			errMatch:  fmt.Errorf("failed to get ovn-pf-encap-ip-mapping"),
			pfEncapIp: "",
			execMockCommands: []*ovntest.ExpectedCmd{
				{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id=ns-foo_pod-bar"),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "external_ids", "name="+vfRep),
				},
				{
					Cmd: genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Err: fmt.Errorf("ovs-vsctl: any error ..."),
				},
			},
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper:  []ovntest.TestifyMockHelper{},
		},
		{
			desc:    "bad external_ids:ovn-pf-encap-ip-mapping config",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   vfRep,
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				NetName:    ovntypes.DefaultNetworkName,
				NetdevName: "enp1s0f0v1",
				PodUID:     "xyz",
			},
			errMatch:  fmt.Errorf("bad ovn-pf-encap-ip-mapping config"),
			pfEncapIp: "",
			execMockCommands: []*ovntest.ExpectedCmd{
				{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id=ns-foo_pod-bar"),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "external_ids", "name="+vfRep),
				},
				{
					Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Output: "10.0.0.1,10.0.0.193,10.0.0.197",
				},
			},
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper:  []ovntest.TestifyMockHelper{},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			execMock := ovntest.NewFakeExec()
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovnetOpsMockHelper)

			err := util.SetExec(execMock)
			require.NoError(t, err)
			err = SetExec(execMock)
			require.NoError(t, err)

			execMock.AddFakeCmds(tc.execMockCommands)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			var pod corev1.Pod
			podNamespaceLister := v1mocks.PodNamespaceLister{}
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)

			var podLister v1mocks.PodLister
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
			err = ConfigureOVS(ctx, nil, tc.podNs, tc.podName, "", tc.vfRep,
				tc.ifInfo, sandboxID, vfPciAddress, false, nil)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}

			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSetupIngressFilter(t *testing.T) {
	nft := knftables.NewFake(knftables.NetDevFamily, "ingress_filter")

	gwLLA := "fe80::1"
	err := setupIngressFilterWithTableAndLifetimeMatcher(nft, "test-iface-1", gwLLA, "mark")
	require.NoError(t, err)
	output := nft.Dump()

	expectedDump := `add table netdev ingress_filter
add chain netdev ingress_filter input { type filter hook ingress device "test-iface-1" priority 0 ; }
add rule netdev ingress_filter input icmpv6 type nd-router-advert ip6 saddr != fe80::1 mark != 0 drop`

	assert.Equal(t, expectedDump, strings.TrimRight(output, "\n"), "The nftables dump output does not match the expected output")
}

// TODO(leih): Below functions are copied from pkg/node/base_node_network_controller_dpu_test.go.
// Move them to a common place to elimate duplications.
func genOVSFindCmd(timeout, table, column, condition string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=%s --no-heading --format=csv --data=bare --columns=%s find %s %s",
		timeout, column, table, condition)
}

func genOVSGetCmd(table, record, column, key string, timeout ...int) string {
	_timeout := 30
	if len(timeout) > 0 {
		_timeout = timeout[0]
	}
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=%d --if-exists get %s %s %s", _timeout, table, record, column)
}

func genIfaceID(podNamespace, podName string) string {
	return fmt.Sprintf("%s_%s", podNamespace, podName)
}

func genOfctlDumpFlowsCmd(queryStr string) string {
	return fmt.Sprintf("ovs-ofctl --timeout=10 --no-stats --strict dump-flows br-int %s", queryStr)
}

// TestDHCPClientID verifies the RFC 2132 Client-ID (option 61) construction:
// a 1-byte hardware type (0x01 = Ethernet) followed by the MAC. The one-shot
// DORA must present the same client identity the guest's DHCP client will use
// after boot, so the DHCP server hands the guest this very lease instead of
// allocating a different IP.
func TestDHCPClientID(t *testing.T) {
	mac := ovntest.MustParseMAC("02:00:00:00:20:0b")
	assert.Equal(t, append([]byte{0x01}, mac...), dhcpClientID(mac))
}

// TestClasslessRoutesToCNIRoutes covers the option-121 (RFC 3442) handling at
// our boundary: well-formed payloads are parsed by the dhcpv4 library and
// converted to CNI routes; malformed payloads — authored by the external,
// untrusted DHCP server — must be rejected wholesale (fail-closed), never
// half-parsed into bogus routes (a prefix length > 32 used to yield a
// nil-mask route that masqueraded as a default route).
func TestClasslessRoutesToCNIRoutes(t *testing.T) {
	// fromOption121 runs raw option-121 bytes through the same library path
	// acquireDHCPLease uses on the ACK: Options.Get → Routes.FromBytes.
	fromOption121 := func(payload []byte) []*cnitypes.Route {
		ack := &dhcpv4.DHCPv4{
			Options: dhcpv4.OptionsFromList(
				dhcpv4.OptGeneric(dhcpv4.OptionClasslessStaticRoute, payload),
			),
		}
		return classlessRoutesToCNIRoutes(ack.ClasslessStaticRoute())
	}

	tests := []struct {
		desc     string
		payload  []byte
		expected []*cnitypes.Route
	}{
		{
			desc:     "no option payload yields no routes",
			payload:  nil,
			expected: nil,
		},
		{
			desc:    "single /24 route",
			payload: []byte{24, 192, 168, 5, 10, 0, 0, 1},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:  net.IPv4(10, 0, 0, 1).To4(),
				},
			},
		},
		{
			desc:    "default route (mask width 0)",
			payload: []byte{0, 10, 0, 0, 1},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(0, 0, 0, 0).To4(), Mask: net.CIDRMask(0, 32)},
					GW:  net.IPv4(10, 0, 0, 1).To4(),
				},
			},
		},
		{
			desc: "multiple routes",
			payload: []byte{
				24, 10, 220, 2, 10, 220, 2, 1, // 10.220.2.0/24 via 10.220.2.1
				16, 172, 16, 172, 16, 0, 1, // 172.16.0.0/16 via 172.16.0.1
			},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(10, 220, 2, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:  net.IPv4(10, 220, 2, 1).To4(),
				},
				{
					Dst: net.IPNet{IP: net.IPv4(172, 16, 0, 0).To4(), Mask: net.CIDRMask(16, 32)},
					GW:  net.IPv4(172, 16, 0, 1).To4(),
				},
			},
		},
		{
			desc:     "truncated payload is rejected wholesale",
			payload:  []byte{24, 192, 168},
			expected: nil,
		},
		{
			desc: "valid route followed by a truncated one rejects the whole option",
			payload: []byte{
				8, 10, 10, 0, 0, 1, // 10.0.0.0/8 via 10.0.0.1
				24, 192, // truncated
			},
			expected: nil,
		},
		{
			desc:     "prefix length greater than 32 is rejected wholesale",
			payload:  []byte{33, 192, 168, 5, 0, 0, 10, 0, 0, 1},
			expected: nil,
		},
		{
			// an unspecified router means an on-link route (RFC 3442); it
			// must carry SCOPE_LINK, as the delegated dhcp plugin reports it
			desc:    "on-link route (unspecified router) is marked SCOPE_LINK",
			payload: []byte{24, 192, 168, 5, 0, 0, 0, 0},
			expected: []*cnitypes.Route{
				{
					Dst:   net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:    net.IPv4zero.To4(),
					Scope: ptr.To(int(netlink.SCOPE_LINK)),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			routes := fromOption121(tc.payload)
			require.Len(t, routes, len(tc.expected))
			for i, expected := range tc.expected {
				assert.True(t, expected.Dst.IP.Equal(routes[i].Dst.IP),
					"route %d dst IP: expected %s got %s", i, expected.Dst.IP, routes[i].Dst.IP)
				assert.Equal(t, expected.Dst.Mask.String(), routes[i].Dst.Mask.String(), "route %d dst mask", i)
				assert.True(t, expected.GW.Equal(routes[i].GW),
					"route %d gw: expected %s got %s", i, expected.GW, routes[i].GW)
				assert.Equal(t, expected.Scope, routes[i].Scope, "route %d scope", i)
			}
		})
	}
}

// TestDHCPACKToCNIResult covers the ACK→CNI-result conversion, which must
// mirror the delegated dhcp IPAM plugin (plugins/ipam/dhcp) so pods and VMs
// report identical results for the same lease: a mask-less ACK is an error
// (never a made-up prefix), the option-3 router is always the IPConfig
// gateway, and per RFC 3442 option 121 replaces the router-derived default
// route entirely.
func TestDHCPACKToCNIResult(t *testing.T) {
	const ifName = "net1"

	newACK := func(yiaddr net.IP, opts ...dhcpv4.Option) *dhcpv4.DHCPv4 {
		ack := &dhcpv4.DHCPv4{Options: dhcpv4.OptionsFromList(opts...)}
		if yiaddr != nil {
			ack.YourIPAddr = yiaddr
		}
		return ack
	}
	yiaddr := net.IPv4(10, 1, 192, 213)
	mask := dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))
	router := dhcpv4.OptRouter(net.IPv4(10, 1, 192, 1))

	t.Run("full ACK yields IP, gateway and an explicit default route", func(t *testing.T) {
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask, router))
		require.NoError(t, err)
		require.Len(t, result.IPs, 1)
		assert.Equal(t, "10.1.192.213/24", result.IPs[0].Address.String())
		assert.Equal(t, "10.1.192.1", result.IPs[0].Gateway.String())
		// upstream appends the router as a default route when option 121 is absent
		require.Len(t, result.Routes, 1)
		assert.Equal(t, "0.0.0.0/0", result.Routes[0].Dst.String())
		assert.Equal(t, "10.1.192.1", result.Routes[0].GW.String())
	})

	t.Run("missing subnet mask is an error, like the delegated dhcp plugin", func(t *testing.T) {
		_, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, router))
		require.ErrorContains(t, err, "Subnet Mask not found in DHCPACK")
	})

	t.Run("malformed subnet mask is an error", func(t *testing.T) {
		badMask := dhcpv4.OptGeneric(dhcpv4.OptionSubnetMask, []byte{255, 255})
		_, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, badMask, router))
		require.ErrorContains(t, err, "Subnet Mask not found in DHCPACK")
	})

	t.Run("option 121 replaces the router-derived default route (RFC 3442)", func(t *testing.T) {
		opt121 := dhcpv4.OptGeneric(dhcpv4.OptionClasslessStaticRoute,
			[]byte{24, 192, 168, 5, 10, 0, 0, 1}) // 192.168.5.0/24 via 10.0.0.1
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask, router, opt121))
		require.NoError(t, err)
		// the router is still reported as the IPConfig gateway, as upstream does
		assert.Equal(t, "10.1.192.1", result.IPs[0].Gateway.String())
		require.Len(t, result.Routes, 1, "option 121 must be the entire route set")
		assert.Equal(t, "192.168.5.0/24", result.Routes[0].Dst.String())
	})

	t.Run("no router yields no gateway and no default route", func(t *testing.T) {
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask))
		require.NoError(t, err)
		assert.Nil(t, result.IPs[0].Gateway)
		assert.Empty(t, result.Routes)
	})

	t.Run("ACK without an IP is an error", func(t *testing.T) {
		_, err := dhcpACKToCNIResult(ifName, newACK(nil, mask, router))
		require.ErrorContains(t, err, "has no IP address")
	})
}
func TestBuildDHCPConf(t *testing.T) {
	pr := &PodRequest{
		CNIConf: &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				CNIVersion: "1.0.0",
				Name:       "localnet-dhcp",
			},
		},
	}

	confBytes, err := pr.buildDHCPConf()
	require.NoError(t, err)

	conf := map[string]any{}
	require.NoError(t, json.Unmarshal(confBytes, &conf))
	assert.Equal(t, "1.0.0", conf["cniVersion"])
	assert.Equal(t, "localnet-dhcp", conf["name"])
	assert.Equal(t, "ovn-k8s-cni-overlay", conf["type"])
	ipam, ok := conf["ipam"].(map[string]any)
	require.True(t, ok, "ipam section must be an object")
	assert.Equal(t, "dhcp", ipam["type"])
}

func TestGetCNIPath(t *testing.T) {
	t.Setenv("CNI_PATH", "/custom/cni/bin")
	assert.Equal(t, "/custom/cni/bin", getCNIPath())

	t.Setenv("CNI_PATH", "")
	assert.Equal(t, "/opt/cni/bin", getCNIPath())
}

func TestMergeDHCPResultIntoCNIResult(t *testing.T) {
	newDHCPResult := func() *current.Result {
		return &current.Result{
			IPs: []*current.IPConfig{
				{
					Address: net.IPNet{IP: net.IPv4(10, 1, 192, 213), Mask: net.CIDRMask(24, 32)},
					Gateway: net.IPv4(10, 1, 192, 1),
				},
			},
			Routes: []*cnitypes.Route{
				{Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, GW: net.IPv4(10, 1, 192, 1)},
			},
		}
	}

	t.Run("assigns the sandbox interface index", func(t *testing.T) {
		result := &current.Result{
			Interfaces: []*current.Interface{
				{Name: "host_eth0"},
				{Name: "eth0", Sandbox: "/var/run/netns/foo"},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		require.NotNil(t, result.IPs[0].Interface)
		assert.Equal(t, 1, *result.IPs[0].Interface)
		assert.Len(t, result.Routes, 1)
	})

	t.Run("leaves the interface index unset with an empty interfaces list", func(t *testing.T) {
		result := &current.Result{}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		assert.Nil(t, result.IPs[0].Interface,
			"an index into an empty interfaces list would be invalid per the CNI spec")
	})

	t.Run("leaves the interface index unset with host-side interfaces only", func(t *testing.T) {
		// Defensive: no current production path produces this shape —
		// ConfigureInterface always reports a sandbox-bearing container
		// interface, including a synthetic one for VFIO — but if a future
		// path ever omits it, the DHCP IP must not be attributed to a
		// host-side interface.
		result := &current.Result{
			Interfaces: []*current.Interface{
				{Name: "host_rep0"},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		assert.Nil(t, result.IPs[0].Interface,
			"the DHCP IP must not be attributed to a host-side interface")
	})

	t.Run("appends to existing IPs and routes", func(t *testing.T) {
		result := &current.Result{
			IPs: []*current.IPConfig{
				{Address: net.IPNet{IP: net.IPv4(100, 10, 10, 3), Mask: net.CIDRMask(24, 32)}},
			},
			Routes: []*cnitypes.Route{
				{Dst: net.IPNet{IP: net.IPv4(100, 10, 0, 0), Mask: net.CIDRMask(16, 32)}},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		assert.Len(t, result.IPs, 2)
		assert.Len(t, result.Routes, 2)
	})
}

func TestShouldRunLocalnetVFIODriverHandoff(t *testing.T) {
	validPR := func() *PodRequest {
		return &PodRequest{
			IsVFIO:  true,
			netName: "localnet-net",
			nadName: "foo-ns/localnet-nad",
			CNIConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{},
				DeviceID: "0000:65:00.2",
				Topology: ovntypes.LocalnetTopology,
			},
		}
	}

	tests := []struct {
		desc     string
		mutate   func(*PodRequest) *PodRequest
		expected bool
	}{
		{
			desc:     "all conditions met",
			mutate:   func(pr *PodRequest) *PodRequest { return pr },
			expected: true,
		},
		{
			desc:     "nil pod request",
			mutate:   func(*PodRequest) *PodRequest { return nil },
			expected: false,
		},
		{
			desc:     "nil CNI conf",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf = nil; return pr },
			expected: false,
		},
		{
			desc:     "no device ID",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf.DeviceID = ""; return pr },
			expected: false,
		},
		{
			desc:     "not VFIO",
			mutate:   func(pr *PodRequest) *PodRequest { pr.IsVFIO = false; return pr },
			expected: false,
		},
		{
			desc:     "default network name",
			mutate:   func(pr *PodRequest) *PodRequest { pr.netName = ovntypes.DefaultNetworkName; return pr },
			expected: false,
		},
		{
			desc:     "default NAD name",
			mutate:   func(pr *PodRequest) *PodRequest { pr.nadName = ovntypes.DefaultNetworkName; return pr },
			expected: false,
		},
		{
			desc:     "layer2 topology",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf.Topology = ovntypes.Layer2Topology; return pr },
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.mutate(validPR()).shouldRunLocalnetVFIODriverHandoff())
		})
	}
}

// fakeSysfsPCI builds a fake /sys/bus/pci tree with the given device bound to
// boundDriver (unbound if empty) and the listed drivers available, and points
// the package sysfs roots at it for the duration of the test.
func fakeSysfsPCI(t *testing.T, deviceID, boundDriver string, drivers ...string) (devicesRoot, driversRoot string) {
	t.Helper()
	root := t.TempDir()
	devicesRoot = filepath.Join(root, "devices")
	driversRoot = filepath.Join(root, "drivers")

	deviceDir := filepath.Join(devicesRoot, deviceID)
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "driver_override"), []byte("\n"), 0o644))
	// default to an NVIDIA/Mellanox VF; tests exercising other vendors
	// overwrite this file
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "vendor"), []byte(pciVendorMellanox+"\n"), 0o644))

	for _, driver := range drivers {
		driverDir := filepath.Join(driversRoot, driver)
		require.NoError(t, os.MkdirAll(driverDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(driverDir, "bind"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(driverDir, "unbind"), nil, 0o644))
	}

	if boundDriver != "" {
		require.NoError(t, os.Symlink(filepath.Join(driversRoot, boundDriver), filepath.Join(deviceDir, "driver")))
	}

	oldDevices, oldDrivers := sysBusPCIDevices, sysBusPCIDrivers
	sysBusPCIDevices, sysBusPCIDrivers = devicesRoot, driversRoot
	t.Cleanup(func() {
		sysBusPCIDevices, sysBusPCIDrivers = oldDevices, oldDrivers
	})
	return devicesRoot, driversRoot
}

func TestGetPCIDeviceDriver(t *testing.T) {
	const deviceID = "0000:65:00.2"

	t.Run("returns the bound driver", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		driver, err := getPCIDeviceDriver(deviceID)
		require.NoError(t, err)
		assert.Equal(t, vfioPCIDriver, driver)
	})

	t.Run("returns empty for an unbound device", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, "")
		driver, err := getPCIDeviceDriver(deviceID)
		require.NoError(t, err)
		assert.Empty(t, driver)
	})
}

// TestVFKernelDriverForDevice covers the vendor→driver detection for the
// VFIO DHCP handoff: the kernel driver must match the VF hardware, and
// unsupported vendors must be rejected explicitly (OKEP-6224) instead of
// force-probing a driver that cannot serve the device.
func TestVFKernelDriverForDevice(t *testing.T) {
	const deviceID = "0000:65:00.2"

	setVendor := func(t *testing.T, devicesRoot, vendor string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(devicesRoot, deviceID, "vendor"), []byte(vendor+"\n"), 0o644))
	}

	t.Run("maps an NVIDIA/Mellanox VF to mlx5_core", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		driver, err := vfKernelDriverForDevice(deviceID)
		require.NoError(t, err)
		assert.Equal(t, mlx5CoreDriver, driver)
	})

	t.Run("rejects an unsupported vendor", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		setVendor(t, devicesRoot, "0x8086") // Intel
		_, err := vfKernelDriverForDevice(deviceID)
		require.ErrorContains(t, err, "unsupported NIC vendor 0x8086")
	})

	t.Run("errors when the vendor attribute is unreadable", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		require.NoError(t, os.Remove(filepath.Join(devicesRoot, deviceID, "vendor")))
		_, err := vfKernelDriverForDevice(deviceID)
		require.ErrorContains(t, err, "failed to read PCI vendor")
	})
}

func TestWritePCIDeviceDriverOverride(t *testing.T) {
	const deviceID = "0000:65:00.2"
	devicesRoot, _ := fakeSysfsPCI(t, deviceID, "")
	overridePath := filepath.Join(devicesRoot, deviceID, "driver_override")

	require.NoError(t, writePCIDeviceDriverOverride(deviceID, mlx5CoreDriver))
	content, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, mlx5CoreDriver, string(content))

	// Clearing the override writes the kernel convention of a single newline.
	require.NoError(t, writePCIDeviceDriverOverride(deviceID, ""))
	content, err = os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, "\n", string(content))
}

func TestBindPCIDeviceDriver(t *testing.T) {
	const deviceID = "0000:65:00.2"

	t.Run("no-op when already bound to the requested driver", func(t *testing.T) {
		_, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver, mlx5CoreDriver)
		require.NoError(t, bindPCIDeviceDriver(deviceID, vfioPCIDriver))
		content, err := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "bind"))
		require.NoError(t, err)
		assert.Empty(t, string(content), "bind should not have been written")
	})

	t.Run("unbinds from the current driver and binds to the new one", func(t *testing.T) {
		devicesRoot, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver, mlx5CoreDriver)

		require.NoError(t, bindPCIDeviceDriver(deviceID, mlx5CoreDriver))

		unbind, err := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "unbind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(unbind))

		bind, err := os.ReadFile(filepath.Join(driversRoot, mlx5CoreDriver, "bind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(bind))

		override, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		assert.Equal(t, "\n", string(override), "override must be cleared after binding")
	})

	t.Run("binds an unbound device without unbinding", func(t *testing.T) {
		_, driversRoot := fakeSysfsPCI(t, deviceID, "", mlx5CoreDriver)
		require.NoError(t, bindPCIDeviceDriver(deviceID, mlx5CoreDriver))
		bind, err := os.ReadFile(filepath.Join(driversRoot, mlx5CoreDriver, "bind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(bind))
	})

	t.Run("restores the original driver when the target driver is missing", func(t *testing.T) {
		devicesRoot, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)

		err := bindPCIDeviceDriver(deviceID, "no_such_driver")
		require.Error(t, err)

		// the device was unbound from vfio-pci before the failed bind; it must
		// be handed back to vfio-pci instead of being left bound to nothing
		restored, readErr := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "bind"))
		require.NoError(t, readErr)
		assert.Equal(t, deviceID, string(restored), "device must be rebound to its original driver on failure")

		override, readErr := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, readErr)
		assert.Equal(t, "\n", string(override), "override must be cleared on failure")
	})
}

// TestRunLocalnetVFIODHCPAlwaysRebindsVFIO verifies the driver-handoff safety
// ordering: the rebind-to-vfio-pci cleanup must run on every failure path that
// may have touched the driver binding — including a failed swap to the kernel
// driver — so the VF is never left driverless (kubelet's retried CNI ADD checks
// for vfio-pci and could otherwise never self-heal). It must NOT run when the
// precondition check fails before any driver state was touched.
func TestRunLocalnetVFIODHCPAlwaysRebindsVFIO(t *testing.T) {
	const deviceID = "0000:65:00.2"

	newVFIOPodRequest := func(t *testing.T) *PodRequest {
		t.Helper()
		pr := newDHCPPodRequest(t)
		pr.IsVFIO = true
		pr.CNIConf.DeviceID = deviceID
		return pr
	}

	stubBind := func(t *testing.T, swapErr error) *bindStubDHCPOps {
		t.Helper()
		stub := &bindStubDHCPOps{swapErr: swapErr}
		dhcpOps = stub
		t.Cleanup(func() { dhcpOps = &defaultDHCPOps{} })
		return stub
	}

	readOverride := func(t *testing.T, devicesRoot string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("rebinds vfio-pci when the swap to the kernel driver fails", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, fmt.Errorf("mlx5_core module not loaded"))

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "failed to bind device")

		require.Equal(t, []string{mlx5CoreDriver, vfioPCIDriver}, stub.calls,
			"the rebind must run even though the swap failed")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the vfio pin must be in place after the rebind")
	})

	t.Run("rebinds vfio-pci when netdev discovery fails after the swap", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, nil)

		// the fake device does not exist on the host, so getNetdevName fails
		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "failed to find netdev")

		require.Equal(t, []string{mlx5CoreDriver, vfioPCIDriver}, stub.calls,
			"the rebind must run when a later step fails")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the vfio pin must be written during the handoff and kept at the end")
	})

	t.Run("keeps the vfio pin when the final rebind fails", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, nil)
		stub.vfioErr = fmt.Errorf("vfio-pci refused the device")

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "rebind")

		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the pin must survive a failed rebind so the retried ADD repairs the device")
	})

	t.Run("does not touch driver bindings when the device is not on vfio-pci", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		stub := stubBind(t, nil)

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "is not bound to vfio-pci")

		require.Empty(t, stub.calls, "no driver operation must happen when the precondition fails")
		require.Equal(t, "\n", readOverride(t, devicesRoot),
			"the override must stay untouched when the precondition fails")
	})

	t.Run("does not touch driver bindings for an unsupported NIC vendor", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		require.NoError(t, os.WriteFile(filepath.Join(devicesRoot, deviceID, "vendor"), []byte("0x8086\n"), 0o644))
		stub := stubBind(t, nil)

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "unsupported NIC vendor")

		require.Empty(t, stub.calls, "the VF must stay untouched on vfio-pci when the vendor is unsupported")
		require.Equal(t, "\n", readOverride(t, devicesRoot),
			"the override must stay untouched when the vendor is unsupported")
	})
}

// TestHealInterruptedVFIOHandoff covers the ADD-time crash repair: a device
// pinned to vfio-pci but bound elsewhere can only be the leftover of a
// handoff interrupted by a process crash, and must be rebound before the
// IsVFIO routing decision reads the binding. Everything else is a no-op.
func TestHealInterruptedVFIOHandoff(t *testing.T) {
	const deviceID = "0000:65:00.2"

	stubOps := func(t *testing.T) *bindStubDHCPOps {
		t.Helper()
		stub := &bindStubDHCPOps{}
		dhcpOps = stub
		t.Cleanup(func() { dhcpOps = &defaultDHCPOps{} })
		return stub
	}

	setOverride := func(t *testing.T, devicesRoot, value string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(devicesRoot, deviceID, "driver_override"), []byte(value), 0o644))
	}

	readOverride := func(t *testing.T, devicesRoot string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("repairs a kernel-bound device pinned to vfio-pci", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver, vfioPCIDriver)
		setOverride(t, devicesRoot, vfioPCIDriver)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Equal(t, []string{vfioPCIDriver}, stub.calls, "the device must be rebound to vfio-pci")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot), "the pin must be restored after the repair")
	})

	t.Run("repairs a driverless device pinned to vfio-pci", func(t *testing.T) {
		// a crash between the unbind and the bind leaves no driver at all
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, "", mlx5CoreDriver, vfioPCIDriver)
		setOverride(t, devicesRoot, vfioPCIDriver)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Equal(t, []string{vfioPCIDriver}, stub.calls,
			"the repair must fire for a driverless pinned device")
	})

	t.Run("does not touch a regular kernel-bound VF without the pin", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Empty(t, stub.calls, "a device without the vfio pin is not an interrupted handoff")
	})

	t.Run("does not touch a healthy vfio-bound device", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		setOverride(t, devicesRoot, vfioPCIDriver)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Empty(t, stub.calls, "a device already on vfio-pci needs no repair")
	})

	t.Run("fails and keeps the pin when the repair rebind fails", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver, vfioPCIDriver)
		setOverride(t, devicesRoot, vfioPCIDriver)
		stub := stubOps(t)
		stub.vfioErr = fmt.Errorf("vfio-pci module not loaded")

		require.ErrorContains(t, healInterruptedVFIOHandoff(deviceID), "failed to restore")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the pin must survive a failed repair so the next attempt fires again")
	})

	t.Run("is a no-op when the device has no sysfs entry", func(t *testing.T) {
		// only a different device exists in the fake sysfs tree
		fakeSysfsPCI(t, "0000:99:00.9", mlx5CoreDriver, mlx5CoreDriver)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))
		require.Empty(t, stub.calls)
	})
}

// fakeDHCPExec implements invoke.Exec, recording plugin invocations and
// returning a canned CNI result for ADD.
type fakeDHCPExec struct {
	addResultJSON []byte
	addErr        error
	calls         []fakeDHCPExecCall
}

type fakeDHCPExecCall struct {
	pluginPath string
	stdin      []byte
	command    string
}

func (f *fakeDHCPExec) ExecPlugin(_ context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	command := ""
	for _, env := range environ {
		if strings.HasPrefix(env, "CNI_COMMAND=") {
			command = strings.TrimPrefix(env, "CNI_COMMAND=")
		}
	}
	f.calls = append(f.calls, fakeDHCPExecCall{pluginPath: pluginPath, stdin: stdinData, command: command})
	if command == "ADD" {
		return f.addResultJSON, f.addErr
	}
	return []byte("{}"), nil
}

func (f *fakeDHCPExec) FindInPath(plugin string, _ []string) (string, error) {
	return "/opt/cni/bin/" + plugin, nil
}

func (f *fakeDHCPExec) Decode(jsonBytes []byte) (version.PluginInfo, error) {
	return (&version.PluginDecoder{}).Decode(jsonBytes)
}

func setFakeDHCPExec(t *testing.T, fake *fakeDHCPExec, apply func(netnsPath, ifName string, dhcpResult *current.Result) error) {
	t.Helper()
	dhcpPluginExec = fake
	if apply != nil {
		dhcpOps = &applyStubDHCPOps{apply: apply}
	}
	t.Cleanup(func() {
		dhcpPluginExec = nil
		dhcpOps = &defaultDHCPOps{}
	})
}

// bindStubDHCPOps overrides only the driver-bind seam of DHCPOps; everything
// else falls through to the real implementation.
type bindStubDHCPOps struct {
	defaultDHCPOps
	calls   []string
	swapErr error
	vfioErr error
}

func (b *bindStubDHCPOps) BindPCIDeviceDriver(_ string, driver string) error {
	b.calls = append(b.calls, driver)
	if driver == mlx5CoreDriver {
		return b.swapErr
	}
	if driver == vfioPCIDriver {
		return b.vfioErr
	}
	return nil
}

// applyStubDHCPOps overrides only the apply-result seam of DHCPOps.
type applyStubDHCPOps struct {
	defaultDHCPOps
	apply func(netnsPath, ifName string, dhcpResult *current.Result) error
}

func (a *applyStubDHCPOps) ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	return a.apply(netnsPath, ifName, dhcpResult)
}

func newDHCPPodRequest(t *testing.T) *PodRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return &PodRequest{
		Command:      CNIAdd,
		PodNamespace: "foo-ns",
		PodName:      "bar-pod",
		SandboxID:    "824bceff24af3",
		Netns:        "/var/run/netns/foo",
		IfName:       "net1",
		netName:      "localnet-net",
		nadName:      "foo-ns/localnet-nad",
		ctx:          ctx,
		CNIConf: &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				CNIVersion: "1.0.0",
				Name:       "localnet-net",
				Type:       "ovn-k8s-cni-overlay",
				IPAM:       cnitypes.IPAM{Type: "dhcp"},
			},
			Topology: ovntypes.LocalnetTopology,
		},
	}
}

const dhcpADDResultJSON = `{
	"cniVersion": "1.0.0",
	"ips": [{"address": "10.1.192.213/24", "gateway": "10.1.192.1"}],
	"routes": [
		{"dst": "0.0.0.0/0", "gw": "10.1.192.1"},
		{"dst": "192.168.5.0/24", "gw": "10.1.192.1"}
	]
}`

func TestExecDHCPAdd(t *testing.T) {
	t.Run("returns the DHCP result and applies it in the pod netns", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(dhcpADDResultJSON)}
		var appliedNetns, appliedIfName string
		var appliedResult *current.Result
		setFakeDHCPExec(t, fake, func(netnsPath, ifName string, dhcpResult *current.Result) error {
			appliedNetns, appliedIfName, appliedResult = netnsPath, ifName, dhcpResult
			return nil
		})

		pr := newDHCPPodRequest(t)

		dhcpResult, err := pr.execDHCPAdd()
		require.NoError(t, err)

		require.Len(t, fake.calls, 1)
		assert.Equal(t, "ADD", fake.calls[0].command)
		assert.Contains(t, string(fake.calls[0].stdin), `"dhcp"`)

		assert.Equal(t, pr.Netns, appliedNetns)
		assert.Equal(t, pr.IfName, appliedIfName)
		require.NotNil(t, appliedResult)

		require.Len(t, dhcpResult.IPs, 1)
		assert.Equal(t, "10.1.192.213/24", dhcpResult.IPs[0].Address.String())
		// the default route is dropped (secondary attachment must not compete
		// with the pod's primary network), the subnet route is kept — in both
		// the applied and the returned result
		require.Len(t, dhcpResult.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", dhcpResult.Routes[0].Dst.String())
		require.Len(t, appliedResult.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", appliedResult.Routes[0].Dst.String())
	})

	t.Run("fails when DHCP returns no IPs", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(`{"cniVersion": "1.0.0", "ips": []}`)}
		setFakeDHCPExec(t, fake, func(string, string, *current.Result) error {
			t.Fatal("apply must not be called without IPs")
			return nil
		})

		_, err := newDHCPPodRequest(t).execDHCPAdd()
		require.ErrorContains(t, err, "no IP addresses")
	})

	t.Run("releases the lease when applying the result fails", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(dhcpADDResultJSON)}
		setFakeDHCPExec(t, fake, func(string, string, *current.Result) error {
			return fmt.Errorf("netns exploded")
		})

		_, err := newDHCPPodRequest(t).execDHCPAdd()
		require.ErrorContains(t, err, "failed to apply DHCP result")

		require.Len(t, fake.calls, 2)
		assert.Equal(t, "ADD", fake.calls[0].command)
		assert.Equal(t, "DEL", fake.calls[1].command, "lease must be released on apply failure")
	})

	t.Run("fails when the plugin errors", func(t *testing.T) {
		fake := &fakeDHCPExec{addErr: fmt.Errorf("daemon unreachable")}
		setFakeDHCPExec(t, fake, nil)

		_, err := newDHCPPodRequest(t).execDHCPAdd()
		require.ErrorContains(t, err, "DHCP plugin ADD failed")
	})
}

func TestExecDHCPDel(t *testing.T) {
	fake := &fakeDHCPExec{}
	setFakeDHCPExec(t, fake, nil)

	pr := newDHCPPodRequest(t)
	require.NoError(t, pr.execDHCPDel())

	require.Len(t, fake.calls, 1)
	assert.Equal(t, "DEL", fake.calls[0].command)
	assert.Contains(t, string(fake.calls[0].stdin), `"dhcp"`)
}

// TestFilterDHCPDefaultRoutes covers the secondary-attachment route policy:
// DHCP IPAM networks are Secondary by CRD rule, so a default route handed out
// by the DHCP server must never compete with the pod's primary network —
// only subnet-scoped (e.g. option-121) routes are kept.
func TestFilterDHCPDefaultRoutes(t *testing.T) {
	pr := &PodRequest{PodNamespace: "foo-ns", PodName: "bar-pod", IfName: "net1"}
	defaultRoute := func() *cnitypes.Route {
		return &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			GW:  net.IPv4(10, 1, 192, 1),
		}
	}
	subnetRoute := func() *cnitypes.Route {
		return &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
			GW:  net.IPv4(10, 1, 192, 1),
		}
	}

	t.Run("drops default routes and keeps subnet routes", func(t *testing.T) {
		result := &current.Result{Routes: []*cnitypes.Route{defaultRoute(), subnetRoute()}}
		pr.filterDHCPDefaultRoutes(result)
		require.Len(t, result.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", result.Routes[0].Dst.String())
	})

	t.Run("only default routes yields an empty route list", func(t *testing.T) {
		result := &current.Result{Routes: []*cnitypes.Route{defaultRoute()}}
		pr.filterDHCPDefaultRoutes(result)
		assert.Empty(t, result.Routes)
	})

	t.Run("no routes is a no-op", func(t *testing.T) {
		result := &current.Result{}
		pr.filterDHCPDefaultRoutes(result)
		assert.Empty(t, result.Routes)
	})
}

// TestApplyDHCPResultInNS covers the route-programming half of the DHCP apply
// path against upstream ipam.ConfigureIface's contract: scope/priority/table
// from the result are honored (an on-link option-121 route must be installed
// scope link, not scope universe with gateway 0.0.0.0), EEXIST is tolerated
// for repeat-ADD idempotency, and any other failure is fatal so a pod missing
// its DHCP-advertised routes never reports a successful ADD.
func TestApplyDHCPResultInNS(t *testing.T) {
	const ifName = "net1"

	newResult := func() *current.Result {
		return &current.Result{
			IPs: []*current.IPConfig{{
				Address: net.IPNet{IP: net.IPv4(10, 1, 192, 213), Mask: net.CIDRMask(24, 32)},
				Gateway: net.IPv4(10, 1, 192, 1),
			}},
			Routes: []*cnitypes.Route{
				{
					Dst:      net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:       net.IPv4zero.To4(),
					Scope:    ptr.To(int(netlink.SCOPE_LINK)),
					Priority: 100,
					Table:    ptr.To(42),
				},
			},
		}
	}

	setup := func(t *testing.T) *util_mocks.NetLinkOps {
		t.Helper()
		mockNetLinkOps := new(util_mocks.NetLinkOps)
		mockLink := new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(mockNetLinkOps)
		mockLink.On("Attrs").Return(&netlink.LinkAttrs{Index: 7, Name: ifName})
		mockNetLinkOps.On("LinkByName", ifName).Return(mockLink, nil)
		return mockNetLinkOps
	}

	t.Run("honors scope, priority and table from the result", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		var captured *netlink.Route
		mockNetLinkOps.On("RouteAdd", mock.Anything).Run(func(args mock.Arguments) {
			captured = args.Get(0).(*netlink.Route)
		}).Return(nil)

		require.NoError(t, applyDHCPResultInNS(ifName, newResult()))
		require.NotNil(t, captured)
		assert.Equal(t, netlink.SCOPE_LINK, captured.Scope, "on-link route must be installed scope link")
		assert.Equal(t, 100, captured.Priority)
		assert.Equal(t, 42, captured.Table)
		assert.Equal(t, 7, captured.LinkIndex)
	})

	t.Run("route add failure is fatal", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		mockNetLinkOps.On("RouteAdd", mock.Anything).Return(fmt.Errorf("network is unreachable"))

		err := applyDHCPResultInNS(ifName, newResult())
		require.ErrorContains(t, err, "failed to add route")
	})

	t.Run("existing route is tolerated for idempotency", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		mockNetLinkOps.On("RouteAdd", mock.Anything).Return(os.ErrExist)

		require.NoError(t, applyDHCPResultInNS(ifName, newResult()))
	})

	t.Run("addr add failure is fatal", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(fmt.Errorf("permission denied"))

		err := applyDHCPResultInNS(ifName, newResult())
		require.ErrorContains(t, err, "failed to add IP")
	})
}

// TestDHCPLeaseMarkerFileBackend covers the DPU-host lease marker, which lives
// in a host-local file because those nodes have no host OVS. The record/exists/
// remove trio must round-trip and be keyed per sandbox+interface.
func TestDHCPLeaseMarkerFileBackend(t *testing.T) {
	prevMode := config.OvnKubeNode.Mode
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
	t.Cleanup(func() { config.OvnKubeNode.Mode = prevMode })

	oldDir := dhcpLeaseMarkerDir
	dhcpLeaseMarkerDir = t.TempDir()
	t.Cleanup(func() { dhcpLeaseMarkerDir = oldDir })

	pr := newDHCPPodRequest(t)

	markerExists := func(p *PodRequest) bool {
		exists, err := p.dhcpLeaseMarkerExists()
		require.NoError(t, err)
		return exists
	}

	assert.False(t, markerExists(pr), "no marker before it is recorded")

	require.NoError(t, pr.recordDHCPLeaseMarker(&current.Result{}),
		"the file backend must not need a host-side interface in the result")
	assert.True(t, markerExists(pr), "marker must exist after recording")

	// a different sandbox on the same node must not see this marker
	other := newDHCPPodRequest(t)
	other.SandboxID = "other-sandbox"
	assert.False(t, markerExists(other), "markers are keyed per sandbox")

	// and neither must the same sandbox with a different interface
	otherIf := newDHCPPodRequest(t)
	otherIf.IfName = "net2"
	assert.False(t, markerExists(otherIf), "markers are keyed per interface")

	pr.removeDHCPLeaseMarker()
	assert.False(t, markerExists(pr), "marker must be gone after removal")

	// removing an absent marker is a no-op, not an error
	pr.removeDHCPLeaseMarker()
}
