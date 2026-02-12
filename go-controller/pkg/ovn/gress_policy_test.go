package ovn

import (
	"testing"

	"github.com/stretchr/testify/assert"

	knet "k8s.io/api/networking/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetMatchFromIPBlockFallback(t *testing.T) {
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
			DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
		for _, ipBlock := range tc.ipBlocks {
			gressPolicy.addIPBlock(ipBlock)
		}
		output := gressPolicy.getMatchFromIPBlock(tc.lportMatch, tc.l4Match)
		assert.Equal(t, tc.expected, output)
	}
}

func TestGetMatchFromIPBlockAddressSets(t *testing.T) {
	testcases := []struct {
		desc            string
		ipBlocks        []*knet.IPBlock
		ipv4BlockAllow  string
		ipv6BlockAllow  string
		ipv4BlockExcept string
		ipv6BlockExcept string
		ipv4Mode        bool
		ipv6Mode        bool
		lportMatch      string
		l4Match         string
		expected        string
	}{
		{
			desc: "IPv4 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "0.0.0.0/0",
				},
			},
			ipv4BlockAllow: "abcdefgh",
			ipv4Mode:       true,
			lportMatch:     "fake",
			l4Match:        "input",
			expected:       "ip4.src == $abcdefgh && input && fake",
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
			ipv4BlockAllow: "abcdefgh",
			ipv4Mode:       true,
			lportMatch:     "fake",
			l4Match:        "input",
			expected:       "ip4.src == $abcdefgh && input && fake",
		},
		{
			desc: "IPv6 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "fd00:10:244:3::49/32",
				},
			},
			ipv6BlockAllow: "ijklmnop",
			lportMatch:     "fake",
			ipv6Mode:       true,
			l4Match:        "input",
			expected:       "ip6.src == $ijklmnop && input && fake",
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
			ipv4BlockAllow: "abcdefgh",
			ipv6BlockAllow: "ijklmnop",
			ipv4Mode:       true,
			ipv6Mode:       true,
			lportMatch:     "fake",
			l4Match:        "input",
			expected:       "(ip4.src == $abcdefgh || ip6.src == $ijklmnop) && input && fake",
		},
		{
			desc: "IPv4 only with except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			ipv4BlockAllow:  "abcdefgh",
			ipv4BlockExcept: "qrstuvwx",
			ipv4Mode:        true,
			lportMatch:      "fake",
			l4Match:         "input",
			expected:        "(ip4.src == $abcdefgh && ip4.src != $qrstuvwx) && input && fake",
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
			ipv4BlockAllow:  "abcdefgh",
			ipv4BlockExcept: "qrstuvwx",
			ipv4Mode:        true,
			lportMatch:      "fake",
			l4Match:         "input",
			expected:        "(ip4.src == $abcdefgh && ip4.src != $qrstuvwx) && input && fake",
		},
		{
			desc: "IPv4 with IPv4 except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			ipv4BlockAllow:  "abcdefgh",
			ipv4BlockExcept: "qrstuvwx",
			ipv4Mode:        true,
			lportMatch:      "fake",
			l4Match:         "input",
			expected:        "(ip4.src == $abcdefgh && ip4.src != $qrstuvwx) && input && fake",
		},
	}

	for _, tc := range testcases {
		gressPolicy := newGressPolicy(knet.PolicyTypeIngress, 5, "testing", "test",
			DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
		for _, ipBlock := range tc.ipBlocks {
			gressPolicy.addIPBlock(ipBlock)
		}
		gressPolicy.ipv4BlockAllow = tc.ipv4BlockAllow
		gressPolicy.ipv6BlockAllow = tc.ipv6BlockAllow
		gressPolicy.ipv4BlockExcept = tc.ipv4BlockExcept
		gressPolicy.ipv6BlockExcept = tc.ipv6BlockExcept
		gressPolicy.ipv4Mode = tc.ipv4Mode
		gressPolicy.ipv6Mode = tc.ipv6Mode
		output := gressPolicy.getL3MatchFromIPBlockAddressSets(tc.l4Match, tc.lportMatch)
		assert.Equal(t, tc.expected, output)
	}
}
