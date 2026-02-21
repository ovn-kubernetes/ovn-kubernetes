package evpn

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	utilnet "k8s.io/utils/net"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetEVPNVTEPDeviceNames(t *testing.T) {
	tests := []struct {
		name           string
		vtepName       string
		expectedBridge string
		expectedVXLAN4 string
		expectedVXLAN6 string
		expectedDummy  string
	}{
		{
			name:           "short name fits",
			vtepName:       "vtepBlue",
			expectedBridge: "evbr-vtepBlue",
			expectedVXLAN4: "evx4-vtepBlue",
			expectedVXLAN6: "evx6-vtepBlue",
			expectedDummy:  "evlo-vtepBlue",
		},
		{
			name:           "long name uses hash",
			vtepName:       "production-evpn-vtep",
			expectedBridge: "evbr-e12de77c", // sha256("production-evpn-vtep")[:8]
			expectedVXLAN4: "evx4-e12de77c",
			expectedVXLAN6: "evx6-e12de77c",
			expectedDummy:  "evlo-e12de77c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectedBridge, GetEVPNBridgeName(tt.vtepName))
			assert.Equal(t, tt.expectedVXLAN4, GetEVPNVXLANName(tt.vtepName, utilnet.IPv4))
			assert.Equal(t, tt.expectedVXLAN6, GetEVPNVXLANName(tt.vtepName, utilnet.IPv6))
			assert.Equal(t, tt.expectedDummy, GetEVPNDummyName(tt.vtepName))
		})
	}
}

func TestGetEVPNNetworkDeviceNames(t *testing.T) {
	tests := []struct {
		name            string
		networkName     string
		networkID       int
		expectedL3SVI   string
		expectedL2SVI   string
		expectedOVSPort string
	}{
		{
			name:            "short CUDN name fits",
			networkName:     "cluster_udn_blue",
			networkID:       100,
			expectedL3SVI:   "svl3-blue",
			expectedL2SVI:   "svl2-blue",
			expectedOVSPort: "evpn-blue",
		},
		{
			name:            "long name uses ID",
			networkName:     "cluster_udn_my-long-network-name",
			networkID:       42,
			expectedL3SVI:   "svl3-42",
			expectedL2SVI:   "svl2-42",
			expectedOVSPort: "evpn-42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: tt.networkName},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			})
			require.NoError(t, err)
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetNetworkID(tt.networkID)

			assert.Equal(t, tt.expectedL3SVI, GetEVPNL3SVIName(mutableNetInfo))
			assert.Equal(t, tt.expectedL2SVI, GetEVPNL2SVIName(mutableNetInfo))
			assert.Equal(t, tt.expectedOVSPort, GetEVPNOVSPortName(mutableNetInfo))
		})
	}
}
