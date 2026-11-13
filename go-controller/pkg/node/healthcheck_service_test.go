// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/util/sets"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	factorymocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// The shared fake models a missing network as an error. A node's real network
// manager returns nil without an error when Dynamic UDN filters the network.
type healthCheckNetworkManager struct {
	networkmanager.Interface
	filteredNamespaces sets.Set[string]
}

func (m *healthCheckNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if m.filteredNamespaces.Has(namespace) {
		return nil, nil
	}
	return m.Interface.GetActiveNetworkForNamespace(namespace)
}

func TestLoadBalancerHealthCheckNetworkAvailability(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "udn"},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.128.0.0/16/24",
	})
	require.NoError(t, err)
	manager := &healthCheckNetworkManager{
		Interface: &networkmanager.FakeNetworkManager{
			PrimaryNetworks:    map[string]util.NetInfo{"udn": netInfo, "unresolved": nil},
			NotFoundNamespaces: sets.New("missing"),
		},
		filteredNamespaces: sets.New("udn"),
	}
	checker := newLoadBalancerHealthChecker("node", factorymocks.NewNodeWatchFactory(t), manager)
	require.False(t, checker.networkAvailable("udn"))
	require.True(t, checker.networkAvailable("default"))
	require.False(t, checker.networkAvailable("unresolved"))
	require.False(t, checker.networkAvailable("missing"))

	// A rendered network remains available during its inactivity grace period;
	// the lookup stops returning it only when Dynamic UDN removes it.
	manager.filteredNamespaces.Delete("udn")
	require.True(t, checker.networkAvailable("udn"))
	manager.filteredNamespaces.Insert("udn")
	require.False(t, checker.networkAvailable("udn"))
	manager.filteredNamespaces.Delete("udn")
	require.True(t, checker.networkAvailable("udn"))

	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false
	manager.filteredNamespaces.Insert("udn")
	require.True(t, checker.networkAvailable("udn"))
}
