package ovn

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestBaseNetworkController_shouldWatchNamespaces(t *testing.T) {
	tests := []struct {
		name                                                 string
		netCfg                                               *ovntypes.NetConf
		enableNetSeg, enableMultiNetPolicies, expectedReturn bool
	}{
		{
			name: "should watch namespaces for default network",
			netCfg: &ovntypes.NetConf{
				NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
			},
			expectedReturn: true,
		},
		{
			name: "should watch namespaces for primary network when network segmentation is enabled",
			netCfg: &ovntypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "primary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
			},
			enableNetSeg:   true,
			expectedReturn: true,
		},
		{
			name: "should watch namespaces for secondary network when multi NetworkPolicies are enabled",
			netCfg: &ovntypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "secondary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRoleSecondary,
			},
			enableMultiNetPolicies: true,
			expectedReturn:         true,
		},
		{
			name: "should not watch namespaces for primary network when network segmentation is disabled",
			netCfg: &ovntypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "primary"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
			},
			expectedReturn: false,
		},
		{
			name: "should not watch namespaces for secondary network when multi NetworkPolicies is disabled",
			netCfg: &ovntypes.NetConf{
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
