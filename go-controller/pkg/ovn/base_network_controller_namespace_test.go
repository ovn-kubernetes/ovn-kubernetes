// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestBaseUserDefinedNetworkController_ClaimsNamespace(t *testing.T) {
	util.PrepareTestConfig()
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.IPv4Mode = true

	const myNetwork = "mine"
	const otherNetwork = "other"
	const myNAD = "ns-mine/nad"
	const otherNAD = "ns-other/nad"

	mineNetConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: myNetwork},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  myNAD,
		Subnets:  "10.128.0.0/14",
		MTU:      1400,
	}
	otherNetConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: otherNetwork},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  otherNAD,
		Subnets:  "10.132.0.0/14",
		MTU:      1400,
	}

	mineNetInfo, err := util.NewNetInfo(mineNetConf)
	require.NoError(t, err)
	otherNetInfo, err := util.NewNetInfo(otherNetConf)
	require.NoError(t, err)

	fnm := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			// Happy paths: each namespace maps to a concrete primary
			// network. The fake resolves the namespace's primary NAD by
			// looking up NADNetworks for entries whose nadKey namespace
			// matches the namespace name.
			"ns-mine":  mineNetInfo,
			"ns-other": otherNetInfo,
			// Transient parse failure: nil value is the fake's signal
			// to return util.NewInvalidPrimaryNetworkError.
			"ns-pending": nil,
		},
		NADNetworks: map[string]util.NetInfo{
			myNAD:    mineNetInfo,
			otherNAD: otherNetInfo,
		},
	}

	oc := &BaseUserDefinedNetworkController{
		BaseNetworkController: BaseNetworkController{
			ReconcilableNetInfo: util.NewReconcilableNetInfo(mineNetInfo),
			networkManager:      fnm,
		},
	}

	t.Run("matching primary NAD → claims", func(t *testing.T) {
		claims, err := oc.ClaimsNamespace("ns-mine")
		require.NoError(t, err)
		assert.True(t, claims, "namespace whose primary NAD is this network must be claimed")
	})

	t.Run("different primary NAD → does not claim", func(t *testing.T) {
		claims, err := oc.ClaimsNamespace("ns-other")
		require.NoError(t, err)
		assert.False(t, claims, "namespace whose primary NAD belongs to a different network must not be claimed")
	})

	t.Run("default-network namespace → does not claim", func(t *testing.T) {
		// Namespace not present in PrimaryNetworks → FakeNetworkManager
		// returns types.DefaultNetworkName, which ClaimsNamespace must
		// treat as "not mine".
		claims, err := oc.ClaimsNamespace("ns-default-only")
		require.NoError(t, err)
		assert.False(t, claims, "default-network-only namespace must not be claimed by a UDN handler")
	})

	t.Run("transient lookup error → bubbles", func(t *testing.T) {
		// This is the load-bearing case for the new (bool, error)
		// signature: shouldFilterNamespace would have swallowed this
		// as "don't filter" (i.e. would have claimed the namespace),
		// causing the shared controller to force-add or keep cached
		// state on stale data.
		claims, err := oc.ClaimsNamespace("ns-pending")
		require.Error(t, err)
		assert.False(t, claims, "predicate must return false on error so callers cannot mistake stale data for a positive claim")
		var invalid *util.InvalidPrimaryNetworkError
		assert.ErrorAs(t, err, &invalid, "expected InvalidPrimaryNetworkError, got %T: %v", err, err)
	})
}

func TestBaseUserDefinedNetworkController_ReconcileNamespaceDeleteFiresWhenNoLongerClaimed(t *testing.T) {
	// The transition gate in pkg/controllers/namespace dispatches a
	// delete reconcile (oldNS != nil, newNS == nil) when a namespace
	// transitions from active → inactive for the handler — typically
	// because a NAD change moved it to a different UDN. The handler
	// must run the delete leg regardless of whether the namespace
	// currently belongs to this network; otherwise the gate would
	// also clear active+cache and the OVN state for the previous
	// owner would leak. shouldFilterNamespace must not short-circuit
	// the delete path.
	util.PrepareTestConfig()
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.IPv4Mode = true

	const myNetwork = "mine"
	const otherNetwork = "other"
	const otherNAD = "ns1/other-nad"
	const nsName = "ns1"

	mineNetConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: myNetwork},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "mine-ns/nad",
		Subnets:  "10.128.0.0/14",
		MTU:      1400,
	}
	otherNetConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: otherNetwork},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  otherNAD,
		Subnets:  "10.132.0.0/14",
		MTU:      1400,
	}
	mineNetInfo, err := util.NewNetInfo(mineNetConf)
	require.NoError(t, err)
	otherNetInfo, err := util.NewNetInfo(otherNetConf)
	require.NoError(t, err)

	// Simulate the post-NAD-transition world: ns1's primary network is
	// now "other", not "mine". shouldFilterNamespace(ns1) for the
	// "mine" controller would return true. Before this fix, that
	// short-circuited the delete leg.
	fnm := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{nsName: otherNetInfo},
		NADNetworks:     map[string]util.NetInfo{otherNAD: otherNetInfo},
	}

	oc := &BaseUserDefinedNetworkController{
		BaseNetworkController: BaseNetworkController{
			ReconcilableNetInfo: util.NewReconcilableNetInfo(mineNetInfo),
			networkManager:      fnm,
			namespaces:          map[string]*namespaceInfo{},
		},
	}

	// Sanity: confirm the predicate would have filtered.
	require.True(t, oc.shouldFilterNamespace(nsName), "test premise: ns must be filtered by current-membership check")
	claims, err := oc.ClaimsNamespace(nsName)
	require.NoError(t, err)
	require.False(t, claims, "test premise: handler must not currently claim ns")

	// Seed prior applied state: an nsInfo entry that an earlier
	// add-leg would have produced. portGroupName empty and multicast
	// disabled so the delete path only touches the in-memory map.
	oc.namespaces[nsName] = &namespaceInfo{}

	// Drive the delete leg through ReconcileNamespace, as the shared
	// controller's had && !has branch would.
	oldNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := oc.ReconcileNamespace(oldNS, nil, nil, nil); err != nil {
		t.Fatalf("ReconcileNamespace delete: %v", err)
	}
	if _, present := oc.namespaces[nsName]; present {
		t.Fatal("delete leg must remove nsInfo entry even when handler no longer claims the namespace")
	}
}

func TestNADNamespacesNeedingReconcile(t *testing.T) {
	// Regression guard for the NAD-transition enqueue gap. The legacy
	// computation (old.Difference(new)) only surfaced newly-added
	// namespaces, so the shared NamespaceController's had && !has
	// branch never fired for namespaces that LEFT the network's NAD
	// set and the OVN state programmed for the previous owner leaked.
	// SymmetricDifference enqueues both transitions.
	cases := []struct {
		name             string
		oldNADs, newNADs []string
		want             []string
	}{
		{
			name:    "added only",
			oldNADs: []string{"ns-a"},
			newNADs: []string{"ns-a", "ns-b"},
			want:    []string{"ns-b"},
		},
		{
			name:    "removed only — load-bearing for delete leg",
			oldNADs: []string{"ns-a", "ns-b"},
			newNADs: []string{"ns-a"},
			want:    []string{"ns-b"},
		},
		{
			name:    "both directions — added and removed in one transition",
			oldNADs: []string{"ns-a", "ns-b"},
			newNADs: []string{"ns-a", "ns-c"},
			want:    []string{"ns-b", "ns-c"},
		},
		{
			name:    "no change — intersection is not re-enqueued",
			oldNADs: []string{"ns-a", "ns-b"},
			newNADs: []string{"ns-a", "ns-b"},
			want:    []string{},
		},
		{
			name:    "empty old — full add",
			oldNADs: nil,
			newNADs: []string{"ns-a", "ns-b"},
			want:    []string{"ns-a", "ns-b"},
		},
		{
			name:    "empty new — full delete",
			oldNADs: []string{"ns-a", "ns-b"},
			newNADs: nil,
			want:    []string{"ns-a", "ns-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nadNamespacesNeedingReconcile(tc.oldNADs, tc.newNADs)
			// .List() returns sorted; compare sorted slices for deterministic diffs.
			assert.ElementsMatch(t, tc.want, got, "namespace transition set mismatch")
		})
	}
}

func TestBaseNetworkController_shouldWatchNamespaces(t *testing.T) {
	tests := []struct {
		name                                                 string
		netCfg                                               *ovncnitypes.NetConf
		enableNetSeg, enableMultiNetPolicies, expectedReturn bool
	}{
		{
			name: "should watch namespaces for default network",
			netCfg: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
			},
			expectedReturn: true,
		},
		{
			name: "should watch namespaces for primary network when network segmentation is enabled",
			netCfg: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "primary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
			},
			enableNetSeg:   true,
			expectedReturn: true,
		},
		{
			name: "should watch namespaces for secondary network when multi NetworkPolicies are enabled",
			netCfg: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "secondary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRoleSecondary,
			},
			enableMultiNetPolicies: true,
			expectedReturn:         true,
		},
		{
			name: "should not watch namespaces for primary network when network segmentation is disabled",
			netCfg: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "primary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
			},
			expectedReturn: false,
		},
		{
			name: "should not watch namespaces for secondary network when multi NetworkPolicies is disabled",
			netCfg: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "secondary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRoleSecondary,
			},
			expectedReturn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.PrepareTestConfig()
			config.OVNKubernetesFeature.EnableMultiNetwork = tt.enableNetSeg || tt.enableMultiNetPolicies
			config.OVNKubernetesFeature.EnableNetworkSegmentation = tt.enableNetSeg
			config.OVNKubernetesFeature.EnableMultiNetworkPolicy = tt.enableMultiNetPolicies
			netInfo, err := util.NewNetInfo(tt.netCfg)
			require.NoError(t, err, "failed to create network info")
			bnc := &BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
			}
			if tt.expectedReturn != bnc.shouldWatchNamespaces() {
				t.Fail()
			}
			assert.Equal(t, tt.expectedReturn, bnc.shouldWatchNamespaces())
		})
	}
}
