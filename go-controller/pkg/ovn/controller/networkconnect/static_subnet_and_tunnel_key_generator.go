package networkconnect

import (
	"fmt"
	"math/big"
	"net"

	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// Helper functions

// getNetworkIndexAndMaxNodes calculates the network index and maxNodes from the CNC's connect subnets.
// This is used for deterministic tunnel key allocation per the OKEP.
//
// Algorithm Overview (from OKEP):
//
//  1. Calculate maxNodes: maxNodes = 2^(bits - NetworkPrefix)
//     - For IPv4 with NetworkPrefix=24: maxNodes = 2^(32-24) = 256
//     - For IPv6 with NetworkPrefix=96: maxNodes = 2^(128-96) = 4 billion (capped at 5000 as claimed by Kubernetes)
//
//  2. Calculate networkIndex: Based on the subnet's position in the connectSubnet range
//     - For a connect CIDR 192.168.0.0/16 with /24 prefix, subnet 192.168.5.0/24 has networkIndex=5
//
//  3. Tunnel key allocation (done by caller):
//     - Layer3 networks: tunnelKey = networkIndex * maxNodes + nodeID + 1
//     - Layer2 networks: tunnelKey = networkIndex * maxNodes + subIndex + 1
//
// Example with NetworkPrefix=24 (maxNodes=256):
//
//	| Network   | Subnet         | Type   | Index | Tunnel Key Range |
//	|-----------|----------------|--------|-------|------------------|
//	| network1  | 192.168.0.0/24 | Layer3 | 0     | [1, 256]         |
//	| network2  | 192.168.1.0/24 | Layer3 | 1     | [257, 512]       |
//	| network40 | 192.168.4.0/31 | Layer2 | 4     | [1025]           |
func getNetworkIndexAndMaxNodes(connectSubnets []networkconnectv1.ConnectSubnet, subnets []*net.IPNet) (networkIndex, maxNodes int) {
	// Determine IP family from the first allocated subnet.
	// In dual-stack, subnets[0] is IPv4 (added first during annotation parsing).
	// The tunnel key calculation is consistent across IP families because CNC has CEL validation
	// ensuring (32 - IPv4NetworkPrefix) == (128 - IPv6NetworkPrefix), so maxNodes is the same.
	isIPv4 := subnets[0].IP.To4() != nil

	// Find the matching connect subnet by family and get its network prefix
	var networkPrefix int
	var connectCIDR *net.IPNet
	for _, cs := range connectSubnets {
		_, cidr, err := net.ParseCIDR(string(cs.CIDR))
		if err != nil {
			klog.Warningf("Failed to parse connect subnet %s: %v", cs.CIDR, err)
			continue
		}
		cidrIsIPv4 := cidr.IP.To4() != nil
		if isIPv4 == cidrIsIPv4 {
			networkPrefix = int(cs.NetworkPrefix)
			connectCIDR = cidr
			break
		}
	}

	// Calculate maxNodes based on network prefix
	// maxNodes = 2^(bits - networkPrefix)
	if isIPv4 {
		maxNodes = 1 << (32 - networkPrefix)
	} else {
		maxNodes = 1 << (128 - networkPrefix)
	}
	if maxNodes > 5000 { // limit max as claimed by Kubernetes
		maxNodes = 5000
	}

	// Calculate network index from the subnet's position within the connect subnet range
	// The network index is the subnet's offset from the connect CIDR base, divided by the block size
	if connectCIDR != nil {
		subnetIP := subnets[0].IP
		connectIP := connectCIDR.IP
		connectOnes, _ := connectCIDR.Mask.Size()

		// Use big.Int for both IPv4 and IPv6 to handle arbitrary bit arithmetic
		subnetInt := ipToBigInt(subnetIP)
		connectInt := ipToBigInt(connectIP)

		// Calculate offset: subnetIP - connectIP
		offset := new(big.Int).Sub(subnetInt, connectInt)

		// Calculate shift amount: total bits - networkPrefix
		var totalBits int
		if isIPv4 {
			totalBits = 32
		} else {
			totalBits = 128
		}
		shift := totalBits - networkPrefix

		// Network index = offset >> shift (equivalent to offset / blockSize)
		if shift > 0 && networkPrefix > connectOnes {
			offset.Rsh(offset, uint(shift))
			networkIndex = int(offset.Int64())
		}
	}

	return networkIndex, maxNodes
}

// ipToBigInt converts an IP address (IPv4 or IPv6) to a big.Int.
func ipToBigInt(ip net.IP) *big.Int {
	// Normalize to 16-byte representation for consistency
	if v4 := ip.To4(); v4 != nil {
		return new(big.Int).SetBytes(v4)
	}
	return new(big.Int).SetBytes(ip.To16())
}

// getLayer2SubIndex returns the index for a Layer2 subnet within its /networkPrefix block.
func getLayer2SubIndex(subnets []*net.IPNet) int {
	if len(subnets) == 0 {
		return 0
	}
	// For /31 subnets, the last octet divided by 2 gives the index within the block
	ip := subnets[0].IP
	if ip.To4() != nil {
		return int(ip[3]) / 2
	}
	// IPv6
	return int(ip[15]) / 2
}

// GetTunnelKey calculates the tunnel key for a network based on its topology type.
// For Layer3: tunnelKey = networkIndex * maxNodes + nodeID + 1
// For Layer2: tunnelKey = networkIndex * maxNodes + subIndex + 1 (where subIndex is derived from subnet)
// The +1 ensures tunnel keys are always > 0 (0 is reserved/invalid).
func GetTunnelKey(connectSubnets []networkconnectv1.ConnectSubnet, allocatedSubnets []*net.IPNet, topologyType string, nodeID int) int {
	networkIndex, maxNodes := getNetworkIndexAndMaxNodes(connectSubnets, allocatedSubnets)

	if topologyType == ovntypes.Layer2Topology {
		subIndex := getLayer2SubIndex(allocatedSubnets)
		return networkIndex*maxNodes + subIndex + 1
	}
	// Layer3
	return networkIndex*maxNodes + nodeID + 1
}

// calculateP2PSubnets calculates /31 (IPv4) or /127 (IPv6) subnets for a node from the allocated subnet.
func calculateP2PSubnets(subnets []*net.IPNet, nodeID int) ([]*net.IPNet, error) {
	var result []*net.IPNet
	for _, subnet := range subnets {
		generator, err := ip.NewIPGenerator(subnet.String())
		if err != nil {
			return nil, fmt.Errorf("failed to create IP generator: %v", err)
		}
		// Use GenerateIPPair to get two IPs forming a /31 or /127 subnet
		// nodeID * 2 is used as the offset to get the correct /31 or /127 slice
		firstIP, _, err := generator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate P2P subnet for node ID %d: %v", nodeID, err)
		}
		result = append(result, firstIP)
	}
	return result, nil
}

// getP2PIPs returns the first and second usable IPs from P2P subnets as IPNets.
// For /31 (IPv4) or /127 (IPv6) subnets, both IPs are usable.
// Returns (firstIPNets, secondIPNets) where each IPNet has the IP with the original mask.
func getP2PIPs(subnets []*net.IPNet) (first, second []*net.IPNet) {
	for _, subnet := range subnets {
		// First IP
		firstIPNet := &net.IPNet{
			IP:   subnet.IP,
			Mask: subnet.Mask,
		}
		first = append(first, firstIPNet)

		// Second IP (increment the last byte)
		secondIP := make(net.IP, len(subnet.IP))
		copy(secondIP, subnet.IP)
		secondIP[len(secondIP)-1]++
		secondIPNet := &net.IPNet{
			IP:   secondIP,
			Mask: subnet.Mask,
		}
		second = append(second, secondIPNet)
	}
	return
}
