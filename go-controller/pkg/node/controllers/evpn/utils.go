package evpn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	bridgePrefix  = "evbr-"
	vxlan4Prefix  = "evx4-"
	vxlan6Prefix  = "evx6-"
	dummyPrefix   = "evlo-"
	l3SVIPrefix   = "svl3-"
	l2SVIPrefix   = "svl2-"
	ovsPortPrefix = "evpn-"
)

// GetEVPNBridgeName returns the EVPN bridge name for a VTEP.
// Uses VTEP name if it fits within the interface name limit, otherwise uses a hash.
func GetEVPNBridgeName(vtepName string) string {
	return getEVPNVTEPDeviceName(vtepName, bridgePrefix)
}

// GetEVPNVXLANName returns the VXLAN name for a VTEP for the given IP family.
// Uses VTEP name if it fits within the interface name limit, otherwise uses a hash.
func GetEVPNVXLANName(vtepName string, family utilnet.IPFamily) string {
	if family == utilnet.IPv6 {
		return getEVPNVTEPDeviceName(vtepName, vxlan6Prefix)
	}
	return getEVPNVTEPDeviceName(vtepName, vxlan4Prefix)
}

// GetEVPNDummyName returns the dummy device name for a managed VTEP IP.
// Uses VTEP name if it fits within the interface name limit, otherwise uses a hash.
func GetEVPNDummyName(vtepName string) string {
	return getEVPNVTEPDeviceName(vtepName, dummyPrefix)
}

// getEVPNVTEPDeviceName generates a device name from the VTEP name.
// Uses the name directly if it fits, otherwise uses first 8 chars of sha256 hash.
func getEVPNVTEPDeviceName(vtepName, prefix string) string {
	candidate := prefix + vtepName
	if len(candidate) <= 15 {
		return candidate
	}
	h := sha256.Sum256([]byte(vtepName))
	return prefix + hex.EncodeToString(h[:])[:8]
}

// GetEVPNL3SVIName returns the L3 (IP-VRF) SVI name for an EVPN network.
// Uses CUDN name if it fits within the interface name limit, otherwise falls back to NetworkID.
func GetEVPNL3SVIName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, l3SVIPrefix)
}

// GetEVPNL2SVIName returns the L2 (MAC-VRF) SVI name for an EVPN network.
// Uses CUDN name if it fits within the interface name limit, otherwise falls back to NetworkID.
func GetEVPNL2SVIName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, l2SVIPrefix)
}

// GetEVPNOVSPortName returns the OVS port name for an EVPN network.
// Uses CUDN name if it fits within the interface name limit, otherwise falls back to NetworkID.
func GetEVPNOVSPortName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, ovsPortPrefix)
}

// getEVPNNetworkDeviceName generates device names with a given prefix.
// It uses the CUDN name if available and fits, otherwise falls back to NetworkID.
func getEVPNNetworkDeviceName(netInfo util.NetInfo, prefix string) string {
	udnNamespace, udnName := util.ParseNetworkName(netInfo.GetNetworkName())
	if udnName != "" && udnNamespace == "" {
		candidate := prefix + udnName
		if len(candidate) <= 15 {
			return candidate
		}
	}
	return fmt.Sprintf("%s%d", prefix, netInfo.GetNetworkID())
}
