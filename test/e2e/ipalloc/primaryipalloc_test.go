// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	utilsnet "k8s.io/utils/net"
)

func TestUtilSuite(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "node ip alloc suite")
}

func TestAllocateNext(t *testing.T) {
	tests := []struct {
		desc   string
		input  *net.IPNet
		output []net.IP
	}{
		{
			desc:   "increments IPv4 address",
			input:  mustParseCIDRIncIP("192.168.1.5/16"), // mask /24 would fail
			output: []net.IP{net.ParseIP("192.168.1.6"), net.ParseIP("192.168.1.7"), net.ParseIP("192.168.1.8")},
		},
		{
			desc:   "increments IPv6 address",
			input:  mustParseCIDRIncIP("fc00:f853:ccd:e793::6/64"),
			output: []net.IP{net.ParseIP("fc00:f853:ccd:e793::7"), net.ParseIP("fc00:f853:ccd:e793::8"), net.ParseIP("fc00:f853:ccd:e793::9")},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			nodeIPAlloc := newIPAllocator(tc.input)
			for _, expectedIP := range tc.output {
				allocatedIP, err := nodeIPAlloc.AllocateNextIP()
				if err != nil {
					t.Errorf("failed to allocated next IP: %v", err)
				}
				if !allocatedIP.Equal(expectedIP) {
					t.Errorf("Expected IP %q, but got %q", expectedIP.String(), allocatedIP.String())
				}
			}
		})
	}
}

// mustParseCIDRIncIP parses the IP and CIDR. It adds the IP to the returned IPNet.
func mustParseCIDRIncIP(cidr string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse CIDR %q: %v", cidr, err))
	}
	ipNet.IP = ip
	return ipNet
}

type network struct {
	ip   string
	mask string
}

type node struct {
	v4 network
	v6 network
}

func TestIPAlloc(t *testing.T) {
	tests := []struct {
		desc                     string
		existingPrimaryNodeIPs   []node
		expectedFromAllocateNext []string
	}{
		{
			desc:                     "IPv4",
			existingPrimaryNodeIPs:   []node{{v4: network{ip: "192.168.1.1", mask: "16"}}, {v4: network{ip: "192.168.1.2", mask: "16"}}},
			expectedFromAllocateNext: []string{"192.168.2.3", "192.168.2.4"},
		},
		{
			// The bump leaves the subnet here, so allocation starts at a Node IP and steps over the Nodes.
			desc: "IPv4 /24",
			existingPrimaryNodeIPs: []node{
				{v4: network{ip: "192.168.104.1", mask: "24"}},
				{v4: network{ip: "192.168.104.3", mask: "24"}},
				{v4: network{ip: "192.168.104.4", mask: "24"}},
				{v4: network{ip: "192.168.104.5", mask: "24"}},
			},
			expectedFromAllocateNext: []string{"192.168.104.6", "192.168.104.7"},
		},
		{
			desc:                     "IPv6",
			existingPrimaryNodeIPs:   []node{{v6: network{ip: "fc00:f853:ccd:e793::5", mask: "64"}}, {v6: network{ip: "fc00:f853:ccd:e793::6", mask: "64"}}},
			expectedFromAllocateNext: []string{"fc00:f853:ccd:e793::107", "fc00:f853:ccd:e793::108"},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cs := fake.NewSimpleClientset(getNodesWithIPs(tc.existingPrimaryNodeIPs))
			pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "")
			if err != nil {
				t.Error(err)
				return
			}
			for _, expectedIPStr := range tc.expectedFromAllocateNext {
				expectedIP := net.ParseIP(expectedIPStr)
				var nextIP net.IP
				var err error
				if utilsnet.IsIPv6(expectedIP) {
					nextIP, err = pipa.AllocateNextV6()
				} else {
					nextIP, err = pipa.AllocateNextV4()
				}
				if err != nil || nextIP == nil {
					t.Errorf("failed to allocated next IPv4 or IPv6 address. err %v", err)
					return
				}
				if !nextIP.Equal(expectedIP) {
					t.Errorf("expected IP %q, but found %q", expectedIP, nextIP)
				}
			}
		})
	}

}

var disjointRoutedNodes = []node{
	{v4: network{ip: "10.1.253.3", mask: "31"}},
	{v4: network{ip: "10.1.253.5", mask: "31"}},
	{v4: network{ip: "10.1.1.1", mask: "31"}},
}

func TestRoutedNodesLeaveNothingToAllocate(t *testing.T) {
	cs := fake.NewSimpleClientset(getNodesWithIPs(disjointRoutedNodes))
	_, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "")
	if !IsNoRangeError(err) {
		t.Errorf("expected no range to be derivable from disjoint Node subnets, but found %v", err)
	}
}

func TestUnreadableNodeIsAFault(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.NodeList{Items: []corev1.Node{
		getNodeObj("node0", map[string]string{}, map[string]string{})}})
	_, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "")
	if err == nil || IsNoRangeError(err) {
		t.Errorf("expected an unannotated Node to fail the run, but found %v", err)
	}
}

func TestPoolReplacesNodeSubnets(t *testing.T) {
	cs := fake.NewSimpleClientset(getNodesWithIPs(disjointRoutedNodes))
	pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "10.100.0.0/24, fd00:10:100::/64")
	if err != nil {
		t.Fatalf("failed to allocate from a pool: %v", err)
	}
	for _, tc := range []struct {
		allocate allocNextFn
		expected string
	}{
		{allocate: pipa.AllocateNextV4, expected: "10.100.0.2"},
		{allocate: pipa.AllocateNextV6, expected: "fd00:10:100::2"},
	} {
		nextIP, err := tc.allocate()
		if err != nil {
			t.Fatalf("failed to allocate next address: %v", err)
		}
		if expectedIP := net.ParseIP(tc.expected); !nextIP.Equal(expectedIP) {
			t.Errorf("expected IP %q, but found %q", expectedIP, nextIP)
		}
	}
}

func TestPoolIsRejected(t *testing.T) {
	for _, pool := range []string{"10.100.0/24", "10.100.0.0/24,", "10.100.0.0/31", "10.100.0.0/24,10.101.0.0/24", "fd00:10:100::/64,fd00:10:101::/64"} {
		t.Run(pool, func(t *testing.T) {
			cs := fake.NewSimpleClientset(getNodesWithIPs(disjointRoutedNodes))
			_, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), pool)
			if err == nil {
				t.Errorf("expected pool %q to be rejected", pool)
			}
			if IsNoRangeError(err) {
				t.Errorf("expected pool %q to fail the run, but it skips the specs: %v", pool, err)
			}
		})
	}
}

func TestAllocationStaysInRange(t *testing.T) {
	cs := fake.NewSimpleClientset(getNodesWithIPs(disjointRoutedNodes))
	pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "10.100.0.0/25")
	if err != nil {
		t.Fatalf("failed to allocate from a pool: %v", err)
	}
	pool := &net.IPNet{IP: net.ParseIP("10.100.0.0"), Mask: net.CIDRMask(25, 32)}
	var got int
	for range 200 {
		nextIP, err := pipa.AllocateNextV4()
		if err != nil {
			if got != 125 {
				t.Errorf("allocated %d addresses from %s, want 125", got, pool)
			}
			return
		}
		got++
		if !pool.Contains(nextIP) {
			t.Fatalf("allocated %s from outside pool %s", nextIP, pool)
		}
	}
	t.Errorf("expected allocation from %s to run out", pool)
}

func TestPoolDoesNotAllocateBroadcast(t *testing.T) {
	cs := fake.NewSimpleClientset(getNodesWithIPs(disjointRoutedNodes))
	pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "10.100.0.0/30")
	if err != nil {
		t.Fatalf("failed to allocate from a pool: %v", err)
	}
	got, err := pipa.AllocateNextV4()
	if err != nil {
		t.Fatalf("expected one address from 10.100.0.0/30: %v", err)
	}
	if !got.Equal(net.ParseIP("10.100.0.2")) {
		t.Errorf("expected 10.100.0.2, got %s", got)
	}
	if _, err := pipa.AllocateNextV4(); err == nil {
		t.Error("expected 10.100.0.0/30 to refuse its broadcast address")
	}
}

func TestIPv6WhenIPv4HasNoRange(t *testing.T) {
	cs := fake.NewSimpleClientset(getNodesWithIPs([]node{
		{v4: network{ip: "10.1.253.3", mask: "31"}, v6: network{ip: "fc00:f853:ccd:e793::5", mask: "64"}},
		{v4: network{ip: "10.1.253.5", mask: "31"}, v6: network{ip: "fc00:f853:ccd:e793::6", mask: "64"}},
	}))
	pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), "")
	if err != nil {
		t.Fatalf("expected IPv6 to still initialise: %v", err)
	}
	if pipa.v4 != nil {
		t.Error("expected no IPv4 range from disjoint /31s")
	}
	got, err := pipa.AllocateNextV6()
	if err != nil {
		t.Fatalf("failed to allocate IPv6: %v", err)
	}
	if got == nil {
		t.Fatal("expected an IPv6 address")
	}
}

func getNodesWithIPs(nodesSpec []node) runtime.Object {
	nodeObjs := make([]corev1.Node, 0, len(nodesSpec))
	getIPMaskFn := func(ip, mask string) string {
		if ip == "" || mask == "" {
			return ""
		}
		return fmt.Sprintf("%s/%s", ip, mask)
	}

	getArrayForHostCIDRs := func(n node) string {
		cidrs := []string{}
		if cidr := getIPMaskFn(n.v4.ip, n.v4.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		if cidr := getIPMaskFn(n.v6.ip, n.v6.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		return fmt.Sprintf("[%s]", strings.Join(cidrs, ","))
	}

	for i, node := range nodesSpec {
		nodePrimaryIfAddrValue := fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
			getIPMaskFn(node.v4.ip, node.v4.mask), getIPMaskFn(node.v6.ip, node.v6.mask))
		node1Annotations := map[string]string{
			"k8s.ovn.org/node-primary-ifaddr": nodePrimaryIfAddrValue,
			util.OVNNodeHostCIDRs:             getArrayForHostCIDRs(node),
		}
		nodeObjs = append(nodeObjs, getNodeObj(fmt.Sprintf("node%d", i), node1Annotations, map[string]string{}))
	}
	nl := &corev1.NodeList{Items: nodeObjs}
	return nl
}

func getNodeObj(nodeName string, annotations, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
			Labels:      labels,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}
