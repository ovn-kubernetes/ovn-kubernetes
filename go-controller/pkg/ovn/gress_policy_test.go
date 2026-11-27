// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetMatchFromIPBlock(t *testing.T) {
	testcases := []struct {
		desc       string
		ipBlocks   []*knet.IPBlock
		lportMatch string
		l4Match    string
		expected   string
	}{
		{
			desc: "IPv4 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "0.0.0.0/0",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "ip4.src == 0.0.0.0/0 && input && fake",
		},
		{
			desc: "multiple IPv4 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "0.0.0.0/0",
				},
				{
					CIDR: "10.1.0.0/16",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "(ip4.src == 0.0.0.0/0 || ip4.src == 10.1.0.0/16) && input && fake",
		},
		{
			desc: "IPv6 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "fd00:10:244:3::49/32",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "ip6.src == fd00:10:244:3::49/32 && input && fake",
		},
		{
			desc: "mixed IPv4 and IPv6  no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "::/0",
				},
				{
					CIDR: "0.0.0.0/0",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "(ip6.src == ::/0 || ip4.src == 0.0.0.0/0) && input && fake",
		},
		{
			desc: "IPv4 only with except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "(ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16}) && input && fake",
		},
		{
			desc: "multiple IPv4 with except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
				{
					CIDR: "10.1.0.0/16",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "((ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16}) || ip4.src == 10.1.0.0/16) && input && fake",
		},
		{
			desc: "IPv4 with IPv4 except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   "(ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16}) && input && fake",
		},
	}

	for _, tc := range testcases {
		gressPolicy := newGressPolicy(knet.PolicyTypeIngress, 5, "testing", "test",
			types.DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
		for _, ipBlock := range tc.ipBlocks {
			gressPolicy.addIPBlock(ipBlock)
		}
		output := gressPolicy.getMatchFromIPBlock(tc.lportMatch, tc.l4Match)
		assert.Equal(t, tc.expected, output)
	}
}

func TestBuildLocalPodACLsNamedPorts(t *testing.T) {
	named := knet.NetworkPolicyPort{Port: &intstr.IntOrString{Type: intstr.String, StrVal: "no-such-port"}}
	tcp := corev1.ProtocolTCP
	numeric := knet.NetworkPolicyPort{Protocol: &tcp, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 80}}

	testcases := []struct {
		desc         string
		ports        []knet.NetworkPolicyPort
		expectedACLs int
	}{
		{desc: "only a named port allows nothing", ports: []knet.NetworkPolicyPort{named}, expectedACLs: 0},
		{desc: "named and numeric ports allow only the numeric port", ports: []knet.NetworkPolicyPort{named, numeric}, expectedACLs: 1},
		{desc: "numeric port is allowed", ports: []knet.NetworkPolicyPort{numeric}, expectedACLs: 1},
		{desc: "no ports allows all ports", expectedACLs: 1},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			gp := newGressPolicy(knet.PolicyTypeIngress, 0, "testing", "test",
				types.DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
			for i := range tc.ports {
				gp.addPortPolicy(&tc.ports[i])
			}
			acls, _ := gp.buildLocalPodACLs("pg", &libovsdbutil.ACLLoggingLevels{})
			assert.Len(t, acls, tc.expectedACLs)
			for _, acl := range acls {
				if len(tc.ports) > 0 {
					assert.Contains(t, acl.Match, "tcp.dst==80")
				}
			}
		})
	}
}
