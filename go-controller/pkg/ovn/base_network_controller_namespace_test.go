// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// primaryNADNetworkManager is a minimal mock for shouldFilterNamespace tests.
type primaryNADNetworkManager struct {
	networkmanager.Interface
	nadKey       string
	err          error
	nadToNetwork map[string]string
}

func (m *primaryNADNetworkManager) GetPrimaryNADForNamespace(string) (string, error) {
	return m.nadKey, m.err
}

func (m *primaryNADNetworkManager) GetNetworkNameForNADKey(nadKey string) string {
	if m.nadToNetwork == nil {
		return ""
	}
	return m.nadToNetwork[nadKey]
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

func TestBaseNetworkController_shouldFilterNamespace(t *testing.T) {
	const (
		nsName    = "test-ns"
		udnName   = "udn1"
		udnNADKey = "test-ns/primary-nad"
		otherUDN  = "other-udn"
		otherNAD  = "test-ns/other-nad"
	)

	mustNetInfo := func(t *testing.T, name, role string) util.NetInfo {
		t.Helper()
		ni, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: name},
			Topology: types.Layer3Topology,
			Role:     role,
		})
		require.NoError(t, err)
		return ni
	}

	tests := []struct {
		name           string
		netInfo        func(t *testing.T) util.NetInfo
		networkManager networkmanager.Interface
		wantFilter     bool
	}{
		{
			name: "secondary network never filters",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, "secondary", types.NetworkRoleSecondary)
			},
			networkManager: &primaryNADNetworkManager{err: apierrors.NewNotFound(corev1.Resource("namespaces"), nsName)},
			wantFilter:     false,
		},
		{
			name: "does not filter when namespace is NotFound in informer cache",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				err: apierrors.NewNotFound(corev1.Resource("namespaces"), nsName),
			},
			wantFilter: false,
		},
		{
			name: "filters on InvalidPrimaryNetworkError",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				err: util.NewInvalidPrimaryNetworkError(nsName),
			},
			wantFilter: true,
		},
		{
			name: "does not filter on unexpected lookup errors",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				err: fmt.Errorf("unexpected failure"),
			},
			wantFilter: false,
		},
		{
			name: "filters default-network namespaces for primary UDN controller",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				nadKey: types.DefaultNetworkName,
			},
			wantFilter: true,
		},
		{
			name: "does not filter when namespace primary NAD matches controller",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				nadKey:       udnNADKey,
				nadToNetwork: map[string]string{udnNADKey: udnName},
			},
			wantFilter: false,
		},
		{
			name: "filters when namespace primary NAD belongs to another network",
			netInfo: func(t *testing.T) util.NetInfo {
				t.Helper()
				return mustNetInfo(t, udnName, types.NetworkRolePrimary)
			},
			networkManager: &primaryNADNetworkManager{
				nadKey:       otherNAD,
				nadToNetwork: map[string]string{otherNAD: otherUDN},
			},
			wantFilter: true,
		},
	}

	t.Cleanup(func() {
		require.NoError(t, config.PrepareTestConfig())
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.PrepareTestConfig()
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true

			bnc := &BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(tt.netInfo(t)),
				networkManager:      tt.networkManager,
			}
			assert.Equal(t, tt.wantFilter, bnc.shouldFilterNamespace(nsName))
		})
	}
}
