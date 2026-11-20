// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

// TestGetACLSampleFeature covers the owner type -> feature mapping that lets the observability
// manager find the sampled ACLs of a feature without involving the controller that owns them.
func TestGetACLSampleFeature(t *testing.T) {
	tests := []struct {
		desc      string
		ownerType string
		expected  SampleFeature
	}{
		{"network policy", NetworkPolicyOwnerType, NetworkPolicySample},
		{"network policy default deny", NetpolNamespaceOwnerType, NetworkPolicySample},
		{"network policy node", NetpolNodeOwnerType, NetworkPolicySample},
		{"admin network policy", AdminNetworkPolicyOwnerType, AdminNetworkPolicySample},
		{"baseline admin network policy", BaselineAdminNetworkPolicyOwnerType, AdminNetworkPolicySample},
		{"egress firewall", EgressFirewallOwnerType, EgressFirewallSample},
		{"multicast namespace", MulticastNamespaceOwnerType, MulticastSample},
		{"multicast cluster", MulticastClusterOwnerType, MulticastSample},
		{"udn isolation", UDNIsolationOwnerType, UDNIsolationSample},
		{"owner type that is not sampled", ClusterNetworkConnectOwnerType, ""},
		{"no owner type at all", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			acl := &nbdb.ACL{ExternalIDs: map[string]string{OwnerTypeKey.String(): tt.ownerType}}
			require.Equal(t, tt.expected, GetACLSampleFeature(acl))
		})
	}
}

// TestGetACLSampleNamespace covers how the namespace an ACL's sampling resolves against is
// recovered from its external IDs. Only the namespaced features encode one; everything else is
// cluster-scoped and must resolve to the empty namespace so that it picks up cluster-wide
// collectors only.
func TestGetACLSampleNamespace(t *testing.T) {
	tests := []struct {
		desc       string
		ownerType  string
		objectName string
		expected   string
	}{
		{"network policy is keyed by namespace and name", NetworkPolicyOwnerType, BuildNamespaceNameKey("foo", "policy"), "foo"},
		{"default deny is keyed by namespace", NetpolNamespaceOwnerType, "foo", "foo"},
		{"egress firewall is keyed by namespace", EgressFirewallOwnerType, "foo", "foo"},
		{"multicast namespace is keyed by namespace", MulticastNamespaceOwnerType, "foo", "foo"},
		{"node ACLs are keyed by node name, not a namespace", NetpolNodeOwnerType, "node1", ""},
		{"admin network policy is cluster scoped", AdminNetworkPolicyOwnerType, "anp1", ""},
		{"udn isolation is cluster scoped", UDNIsolationOwnerType, "SecondaryPods", ""},
		{"multicast cluster is cluster scoped", MulticastClusterOwnerType, "DefaultDeny", ""},
		{"an unparsable namespace+name key resolves to no namespace", NetworkPolicyOwnerType, "not-a-valid-key", ""},
		{"a missing object name resolves to no namespace", NetworkPolicyOwnerType, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			acl := &nbdb.ACL{ExternalIDs: map[string]string{
				OwnerTypeKey.String():  tt.ownerType,
				ObjectNameKey.String(): tt.objectName,
			}}
			require.Equal(t, tt.expected, GetACLSampleNamespace(acl))
		})
	}
}

// TestUpdateACLsSamplingOps covers re-pointing an existing ACL's Sample without disturbing the
// rest of it. This is what lets an ObservabilityConfig change reach ACLs that already exist
// without their owning controller re-reconciling the object they came from.
func TestUpdateACLsSamplingOps(t *testing.T) {
	// The test harness rewrites named UUIDs, so the collectors are seeded by named UUID and the
	// real ones are resolved from the DB afterwards. Cases pick collectors by index into that list.
	collector1, collector2 := buildNamedUUID(), buildNamedUUID()

	// newACL builds a sampled ACL, optionally already pointing at a Sample.
	newACL := func(sampleUUID *string) *nbdb.ACL {
		return &nbdb.ACL{
			UUID:        buildNamedUUID(),
			Action:      nbdb.ACLActionAllow,
			Direction:   nbdb.ACLDirectionToLport,
			Match:       "ip4.src == 10.0.0.1",
			Priority:    1,
			ExternalIDs: map[string]string{OwnerTypeKey.String(): NetworkPolicyOwnerType},
			SampleNew:   sampleUUID,
			SampleEst:   sampleUUID,
		}
	}

	tests := []struct {
		desc string
		// sampled is whether the ACL already references a Sample before the update.
		sampled bool
		// collectorIDs picks the desired collectors by their Sample_Collector ID; empty means
		// detach sampling.
		collectorIDs []int
		// expectSample is whether the ACL must reference a Sample afterwards.
		expectSample bool
	}{
		{desc: "attaches a Sample to an unsampled ACL", sampled: false, collectorIDs: []int{1}, expectSample: true},
		{desc: "attaches several collectors", sampled: false, collectorIDs: []int{1, 2}, expectSample: true},
		{desc: "repoints an already sampled ACL at new collectors", sampled: true, collectorIDs: []int{2}, expectSample: true},
		{desc: "detaches sampling when no collector applies", sampled: true, collectorIDs: nil, expectSample: false},
		{desc: "leaves an unsampled ACL unsampled", sampled: false, collectorIDs: nil, expectSample: false},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			collectorRows := []libovsdbtest.TestData{
				&nbdb.SampleCollector{UUID: collector1, ID: 1, SetID: 1, Probability: 65535},
				&nbdb.SampleCollector{UUID: collector2, ID: 2, SetID: 2, Probability: 65535},
			}

			var acl *nbdb.ACL
			initialData := collectorRows
			if tt.sampled {
				existingSample := &nbdb.Sample{UUID: buildNamedUUID(), Collectors: []string{collector1}}
				acl = newACL(&existingSample.UUID)
				existingSample.Metadata = int(GetACLSampleID(acl))
				initialData = append(initialData, existingSample)
			} else {
				acl = newACL(nil)
			}
			// A port group keeps the ACL referenced, the way a real one always is.
			pg := &nbdb.PortGroup{UUID: buildNamedUUID(), Name: "pg", ACLs: []string{acl.UUID}}
			initialData = append(initialData, pg, acl)

			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: initialData}, nil)
			require.NoError(t, err, "failed to set up the test harness")
			t.Cleanup(cleanup.Cleanup)

			// Resolve the collector UUIDs the harness actually assigned.
			dbCollectors, err := ListSampleCollectors(nbClient)
			require.NoError(t, err, "failed to list collectors")
			collectorUUIDByID := map[int]string{}
			for _, c := range dbCollectors {
				collectorUUIDByID[c.ID] = c.UUID
			}
			var collectors []string
			for _, id := range tt.collectorIDs {
				require.Contains(t, collectorUUIDByID, id, "collector %d must exist in the test DB", id)
				collectors = append(collectors, collectorUUIDByID[id])
			}

			found, err := FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
			require.NoError(t, err, "failed to list ACLs")
			require.Len(t, found, 1, "expected exactly one ACL in the test DB")
			before := found[0]

			ops, err := UpdateACLsSamplingOps(nbClient, nil, ACLSampling{ACL: before, Collectors: collectors})
			require.NoError(t, err, "failed to build the sampling update ops")
			_, err = TransactAndCheck(nbClient, ops)
			require.NoError(t, err, "failed to transact the sampling update")

			found, err = FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
			require.NoError(t, err, "failed to list ACLs after the update")
			require.Len(t, found, 1, "the update must not create or delete ACLs")
			after := found[0]

			// Nothing but the Sample columns may change.
			require.Equal(t, acl.Match, after.Match, "Match must be left untouched")
			require.Equal(t, acl.Action, after.Action, "Action must be left untouched")
			require.Equal(t, acl.Priority, after.Priority, "Priority must be left untouched")
			require.Equal(t, acl.ExternalIDs, after.ExternalIDs, "ExternalIDs must be left untouched")

			if !tt.expectSample {
				require.Nil(t, after.SampleNew, "SampleNew must be cleared")
				require.Nil(t, after.SampleEst, "SampleEst must be cleared")
				samples, err := ListSamples(nbClient)
				require.NoError(t, err, "failed to list Samples")
				require.Empty(t, samples, "an unreferenced Sample must not survive")
				return
			}

			require.NotNil(t, after.SampleNew, "SampleNew must be set")
			require.NotNil(t, after.SampleEst, "SampleEst must be set")
			require.Equal(t, *after.SampleNew, *after.SampleEst, "both directions must share one Sample")

			samples, err := ListSamples(nbClient)
			require.NoError(t, err, "failed to list Samples")
			require.Len(t, samples, 1, "exactly one Sample must back the ACL")
			require.Equal(t, *after.SampleNew, samples[0].UUID, "the ACL must point at that Sample")
			require.ElementsMatch(t, collectors, samples[0].Collectors, "the Sample must carry the desired collectors")
			require.Equal(t, int(GetACLSampleID(after)), samples[0].Metadata,
				"the Sample metadata must stay derived from the ACL identity")
		})
	}
}

// TestUpdateACLsSamplingOpsMissingACL covers an ACL deleted between being listed and the update
// being built: the caller must see an error so it can retry against a fresh list, rather than
// the ACL being silently recreated.
func TestUpdateACLsSamplingOpsMissingACL(t *testing.T) {
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	require.NoError(t, err, "failed to set up the test harness")
	t.Cleanup(cleanup.Cleanup)

	absent := &nbdb.ACL{
		UUID:        "2a3b4c5d-0000-0000-0000-000000000001",
		Match:       "ip4.src == 10.0.0.1",
		ExternalIDs: map[string]string{OwnerTypeKey.String(): NetworkPolicyOwnerType},
	}
	_, err = UpdateACLsSamplingOps(nbClient, nil, ACLSampling{ACL: absent, Collectors: []string{"2a3b4c5d-0000-0000-0000-000000000002"}})
	require.Error(t, err, "updating sampling on an ACL that no longer exists must fail")

	acls, err := FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
	require.NoError(t, err, "failed to list ACLs")
	require.Empty(t, acls, "a missing ACL must not be recreated")
}
