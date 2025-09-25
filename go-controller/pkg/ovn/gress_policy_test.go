package ovn

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	knet "k8s.io/api/networking/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetMatchFromIPBlock(t *testing.T) {
	testcases := []struct {
		desc       string
		ipBlocks   []*knet.IPBlock
		lportMatch string
		l4Match    string
		expected   []string
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
			expected:   []string{"ip4.src == {0.0.0.0/0} && input && fake"},
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
			expected: []string{"ip4.src == {0.0.0.0/0} && input && fake",
				"ip4.src == {10.1.0.0/16} && input && fake"},
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
			expected:   []string{"ip6.src == {fd00:10::/32} && input && fake"},
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
			expected: []string{"ip6.src == {::/0} && input && fake",
				"ip4.src == {0.0.0.0/0} && input && fake"},
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
			expected:   []string{"ip4.src == {0.0.0.0/5, 8.0.0.0/7, 10.0.0.0/16, 10.2.0.0/15, 10.4.0.0/14, 10.8.0.0/13, 10.16.0.0/12, 10.32.0.0/11, 10.64.0.0/10, 10.128.0.0/9, 11.0.0.0/8, 12.0.0.0/6, 16.0.0.0/4, 32.0.0.0/3, 64.0.0.0/2, 128.0.0.0/1} && input && fake"},
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
			expected: []string{
				"ip4.src == {0.0.0.0/5, 8.0.0.0/7, 10.0.0.0/16, 10.2.0.0/15, 10.4.0.0/14, 10.8.0.0/13, 10.16.0.0/12, 10.32.0.0/11, 10.64.0.0/10, 10.128.0.0/9, 11.0.0.0/8, 12.0.0.0/6, 16.0.0.0/4, 32.0.0.0/3, 64.0.0.0/2, 128.0.0.0/1} && input && fake",
				"ip4.src == {10.1.0.0/16} && input && fake",
			},
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
			expected:   []string{"ip4.src == {0.0.0.0/5, 8.0.0.0/7, 10.0.0.0/16, 10.2.0.0/15, 10.4.0.0/14, 10.8.0.0/13, 10.16.0.0/12, 10.32.0.0/11, 10.64.0.0/10, 10.128.0.0/9, 11.0.0.0/8, 12.0.0.0/6, 16.0.0.0/4, 32.0.0.0/3, 64.0.0.0/2, 128.0.0.0/1} && input && fake"},
		},
	}

	for _, tc := range testcases {
		gressPolicy := newGressPolicy(knet.PolicyTypeIngress, 5, "testing", "test",
			DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
		for _, ipBlock := range tc.ipBlocks {
			gressPolicy.addIPBlock(ipBlock)
		}
		output, _ := gressPolicy.getMatchFromIPBlock(tc.lportMatch, tc.l4Match)

		// Log actual output for reference
		t.Logf("%s: actual=%v", tc.desc, output)

		// sort for deterministic comparison
		sort.Strings(output)
		sort.Strings(tc.expected)

		assert.Equal(t, tc.expected, output, tc.desc)
	}
}
