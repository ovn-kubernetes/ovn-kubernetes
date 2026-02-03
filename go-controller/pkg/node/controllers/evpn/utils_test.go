package evpn

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	utilnet "k8s.io/utils/net"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetEVPNVTEPDeviceNames(t *testing.T) {
	tests := []struct {
		name           string
		vtepName       string
		expectedBridge string
		expectedVXLAN4 string
		expectedVXLAN6 string
	}{
		{
			name:           "short name fits",
			vtepName:       "vtepBlue",
			expectedBridge: "evbr-vtepBlue",
			expectedVXLAN4: "evx4-vtepBlue",
			expectedVXLAN6: "evx6-vtepBlue",
		},
		{
			name:           "long name uses hash",
			vtepName:       "production-evpn-vtep",
			expectedBridge: "evbr.e12de77c", // sha256("production-evpn-vtep")[:8]
			expectedVXLAN4: "evx4.e12de77c",
			expectedVXLAN6: "evx6.e12de77c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectedBridge, GetEVPNBridgeName(tt.vtepName))
			assert.Equal(t, tt.expectedVXLAN4, GetEVPNVXLANName(tt.vtepName, utilnet.IPv4))
			assert.Equal(t, tt.expectedVXLAN6, GetEVPNVXLANName(tt.vtepName, utilnet.IPv6))
		})
	}
}

func TestGetEVPNVTEPDeviceNames_NoCollisionBetweenNameAndHash(t *testing.T) {
	// A short VTEP whose name looks like a hash must not collide with
	// a long VTEP that hashes to the same 8 chars. The "-" vs "."
	// separator guarantees uniqueness.
	shortVTEP := "e12de77c" // name that looks like a hash
	longVTEP := "production-evpn-vtep"

	shortBridge := GetEVPNBridgeName(shortVTEP)
	longBridge := GetEVPNBridgeName(longVTEP)

	assert.Equal(t, "evbr-e12de77c", shortBridge, "short VTEP uses '-' separator")
	assert.Equal(t, "evbr.e12de77c", longBridge, "long VTEP uses '.' separator")
	assert.NotEqual(t, shortBridge, longBridge, "name-based and hash-based must not collide")
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
			expectedOVSPort: "ovl2-blue",
		},
		{
			name:            "long name uses ID",
			networkName:     "cluster_udn_my-long-network-name",
			networkID:       42,
			expectedL3SVI:   "svl3.42",
			expectedL2SVI:   "svl2.42",
			expectedOVSPort: "ovl2.42",
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

func TestGetEVPNNetworkDeviceNames_NoCollisionBetweenNameAndID(t *testing.T) {
	// A CUDN named "42" must not collide with a long-named CUDN that
	// falls back to networkID 42.
	cudnNamed42, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "cluster_udn_42"},
		Topology: ovntypes.Layer2Topology,
		Role:     ovntypes.NetworkRolePrimary,
	})
	require.NoError(t, err)
	mutableNamed42 := util.NewMutableNetInfo(cudnNamed42)
	mutableNamed42.SetNetworkID(100)

	longCUDNWithID42, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "cluster_udn_very-long-network-name"},
		Topology: ovntypes.Layer2Topology,
		Role:     ovntypes.NetworkRolePrimary,
	})
	require.NoError(t, err)
	mutableLong := util.NewMutableNetInfo(longCUDNWithID42)
	mutableLong.SetNetworkID(42)

	named42SVI := GetEVPNL3SVIName(mutableNamed42)
	longSVI := GetEVPNL3SVIName(mutableLong)

	assert.Equal(t, "svl3-42", named42SVI, "CUDN named '42' uses '-' separator")
	assert.Equal(t, "svl3.42", longSVI, "long CUDN with ID 42 uses '.' separator")
	assert.NotEqual(t, named42SVI, longSVI, "name-based and ID-based must not collide")
}
