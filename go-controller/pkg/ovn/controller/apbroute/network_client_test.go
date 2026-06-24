// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package apbroute

import (
	"testing"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// TestPolicyComparisonInPredicate tests that the predicate function in createOrUpdateBFDStaticRoute
// correctly compares Policy values (not pointers) to avoid creating duplicate routes.
// This is a regression test for https://issues.redhat.com/browse/OCPBUGS-70272
func TestPolicyComparisonInPredicate(t *testing.T) {
	tests := []struct {
		name     string
		route1   *nbdb.LogicalRouterStaticRoute
		route2   *nbdb.LogicalRouterStaticRoute
		expected bool
	}{
		{
			name: "same policy values but different pointers",
			route1: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP),
			},
			route2: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP),
			},
			expected: true,
		},
		{
			name: "both policies are nil",
			route1: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     nil,
			},
			route2: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     nil,
			},
			expected: true,
		},
		{
			name: "nil policy equals DstIP policy",
			route1: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     nil,
			},
			route2: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policyPtr(nbdb.LogicalRouterStaticRoutePolicyDstIP),
			},
			expected: true,
		},
		{
			name: "different policy values",
			route1: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP),
			},
			route2: &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policyPtr(nbdb.LogicalRouterStaticRoutePolicyDstIP),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify pointer addresses are different when both policies are non-nil
			if tt.route1.Policy != nil && tt.route2.Policy != nil && tt.route1.Policy == tt.route2.Policy {
				t.Fatal("Test setup error: policy pointers should be different when both are non-nil")
			}

			// The predicate function from createOrUpdateBFDStaticRoute
			// This uses the actual production helper libovsdbops.PolicyEqualPredicate
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == tt.route2.IPPrefix &&
					item.Nexthop == tt.route2.Nexthop &&
					item.OutputPort != nil &&
					*item.OutputPort == *tt.route2.OutputPort &&
					libovsdbops.PolicyEqualPredicate(item.Policy, tt.route2.Policy)
			}

			result := p(tt.route1)
			if result != tt.expected {
				t.Errorf("Predicate returned %v, expected %v", result, tt.expected)
			}
		})
	}
}

func stringPtr(s string) *string {
	return &s
}

func policyPtr(p nbdb.LogicalRouterStaticRoutePolicy) *nbdb.LogicalRouterStaticRoutePolicy {
	return &p
}
