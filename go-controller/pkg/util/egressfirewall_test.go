package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
)

type output struct {
	dnsName string
}

func TestValidateAndGetEgressFirewallDNSDestination(t *testing.T) {
	testcases := []struct {
		name                      string
		egressFirewallDestination egressfirewallapi.EgressFirewallDestination
		dnsNameResolverEnabled    bool
		expectedErr               bool
		expectedOutput            output
	}{
		{
			name: "should correctly validate dns name",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "www.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "www.example.com",
			},
		},
		{
			name: "should throw an error for wildcard dns name when dns name resolver is not enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            true,
		},
		{
			name: "should correctly validate wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "*.example.com",
			},
		},
		{
			name: "should throw an error for tld dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for tld wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for dns name with more than 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz123456789012.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should validate dns name with 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
		},
		{
			name: "should throw an error for a dns name with a label starting with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "-example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for a dns name with a label ending with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "example-.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
	}

	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to PrepareTestConfig: %v", err)
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {

			config.OVNKubernetesFeature.EnableDNSNameResolver = tc.dnsNameResolverEnabled

			dnsName, err :=
				ValidateAndGetEgressFirewallDNSDestination(tc.egressFirewallDestination)
			if tc.expectedErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedOutput.dnsName, dnsName)
			}
		})
	}
}

func TestIsWildcard(t *testing.T) {
	tests := []struct {
		dnsName        string
		expectedOutput bool
	}{
		// success
		{
			dnsName:        "*.example.com",
			expectedOutput: true,
		},
		{
			dnsName:        "*.sub1.example.com",
			expectedOutput: true,
		},
		// negative
		{
			dnsName:        "www.example.com",
			expectedOutput: false,
		},
		{
			dnsName:        "sub2.sub1.example.com",
			expectedOutput: false,
		},
	}

	for _, tc := range tests {
		actualOutput := IsWildcard(tc.dnsName)
		assert.Equal(t, tc.expectedOutput, actualOutput)
	}
}

func TestGetNames(t *testing.T) {
	tests := []struct {
		name            string
		ef              *egressfirewallapi.EgressFirewall
		expectedDNSName []string
	}{
		{
			name: "DNS names in allow DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
		{
			name: "DNS names in deny DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
		{
			name: "DNS names in allow and deny DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dnsNames := GetDNSNames(tc.ef)
			dnsNameSet := sets.New(dnsNames...)
			expectedDNSNameSet := sets.New(tc.expectedDNSName...)
			if dnsNameSet.Intersection(expectedDNSNameSet).Len() != dnsNameSet.Len() {
				t.Fatalf("Unexpected DNS names. Expected: %v, Actual: %v", tc.expectedDNSName, dnsNames)
			}
		})
	}
}
