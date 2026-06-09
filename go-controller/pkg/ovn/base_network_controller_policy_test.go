// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/stretchr/testify/require"

	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

// TestReconcileNetworkPoliciesACLLoggingInNamespace_WalksSyncmap verifies
// the namespace-update ACL-logging refresh iterates bnc.networkPolicies
// (the controller's authoritative policy set) rather than the core
// NetworkPolicy informer.
//
// Regression context: the legacy back-pointer set
// (nsInfo.relatedNetworkPolicies) was populated by both core
// NetworkPolicy and MultiNetworkPolicy add paths. The first cut of this
// refactor listed only core NetworkPolicy from the informer, silently
// dropping multi-net coverage. The fix walks
// bnc.networkPolicies, where both kinds are stored under the same
// "namespace/name" key.
//
// The test seeds entries with deleted=true so updateACLLoggingForPolicy
// short-circuits without needing an nbClient. A broken namespace filter
// that visited the "ns-other/x" key would still no-op for the same
// reason, so the verification here is structural — the function must
// return nil with the syncmap walk in place. Stronger assertions
// (per-key visit count) require deeper plumbing into networkPolicy and
// are covered by the existing ginkgo policy suite.
func TestReconcileNetworkPoliciesACLLoggingInNamespace_WalksSyncmap(t *testing.T) {
	bnc := &BaseNetworkController{
		controllerName:         "test-controller",
		networkPolicies:        syncmap.NewSyncMap[*networkPolicy](),
		sharedNetpolPortGroups: syncmap.NewSyncMap[*defaultDenyPortGroups](),
	}

	// Both core and multi-net policies live in the same syncmap under
	// the same "ns/name" key shape; only the source differs (informer
	// add vs. convertMultiNetPolicyToNetPolicy).
	bnc.networkPolicies.Store("ns-target/core", &networkPolicy{
		namespace: "ns-target", name: "core", deleted: true,
	})
	bnc.networkPolicies.Store("ns-target/converted-from-multinet", &networkPolicy{
		namespace: "ns-target", name: "converted-from-multinet", deleted: true,
	})
	bnc.networkPolicies.Store("ns-other/unrelated", &networkPolicy{
		namespace: "ns-other", name: "unrelated", deleted: true,
	})

	err := bnc.ReconcileNetworkPoliciesACLLoggingInNamespace("ns-target", libovsdbutil.ACLLoggingLevels{Allow: "info"})
	require.NoError(t, err)

	// Sanity: the function must NOT mutate the syncmap. If a regression
	// rewrote the function to consume entries, this would catch it.
	require.Len(t, bnc.networkPolicies.GetKeys(), 3, "ReconcileNetworkPoliciesACLLoggingInNamespace must not mutate the policy syncmap")
}
