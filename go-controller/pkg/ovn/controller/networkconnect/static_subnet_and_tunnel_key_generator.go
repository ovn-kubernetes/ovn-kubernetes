package networkconnect

import (
	"fmt"
	"math/big"
	"net"

	utilnet "k8s.io/utils/net"

	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// Helper functions

// findMatchingConnectSubnet returns the networkPrefix and connectCIDR for the given IP family.
// It finds the matching connect subnet based on whether the subnet is IPv4 or IPv6.
func getNetworkPrefixAndConnectCIDR(connectSubnets []networkconnectv1.ConnectSubnet, subnet *net.IPNet) (networkPrefix int, connectCIDR *net.IPNet, err error) {
	isIPv4 := subnet.IP.To4() != nil

	for _, cs := range connectSubnets {
		_, cidr, err := net.ParseCIDR(string(cs.CIDR))
		if err != nil {
			return 0, nil, fmt.Errorf("failed to parse connect subnet %s: %v", cs.CIDR, err)
		}
		if (cidr.IP.To4() != nil) == isIPv4 {
			return int(cs.NetworkPrefix), cidr, nil
		}
	}
	return 0, nil, fmt.Errorf("no connect subnet found for IP family IPv4=%v", isIPv4)
}

// getTotalBits returns 32 for IPv4 or 128 for IPv6.
func getTotalBits(ip net.IP) int {
	if ip.To4() != nil {
		return 32
	}
	return 128
}

// getSubnetIndexInParent calculates the index of a child subnet within a parent subnet.
// For example, finding the index of a /24 subnet within a /16 block, or a /31 within a /24.
// The index is calculated as: (childIP - parentIP) >> (totalBits - childPrefixLen)
func getSubnetIndexInParent(parentBaseIP net.IP, childSubnet *net.IPNet) int {
	parentInt := utilnet.BigForIP(parentBaseIP)
	childInt := utilnet.BigForIP(childSubnet.IP)

	offset := new(big.Int).Sub(childInt, parentInt)

	childOnes, totalBits := childSubnet.Mask.Size()
	shift := totalBits - childOnes

	if shift > 0 {
		offset.Rsh(offset, uint(shift))
	}

	return int(offset.Int64())
}

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
func getNetworkIndexAndMaxNodes(subnet *net.IPNet, networkPrefix int, connectCIDR *net.IPNet) (networkIndex, maxNodes int, err error) {
	totalBits := getTotalBits(subnet.IP)

	// Calculate maxNodes based on network prefix
	// maxNodes = 2^(TotalBits - networkPrefix)
	maxNodes = 1 << (totalBits - networkPrefix)
	if maxNodes > 5000 { // limit max as claimed by Kubernetes
		maxNodes = 5000
	}

	// Calculate network index from the subnet's position within the connect subnet range
	connectOnes, _ := connectCIDR.Mask.Size()

	// Validate configuration (CRD CEL validation should prevent this, but check for defense in depth)
	shift := totalBits - networkPrefix
	if shift <= 0 || networkPrefix <= connectOnes {
		return 0, 0, fmt.Errorf("invalid configuration: networkPrefix (%d) must be greater than connect CIDR prefix (%d) and less than %d",
			networkPrefix, connectOnes, totalBits)
	}

	// Create a temporary IPNet with /networkPrefix mask to use with getSubnetIndexInParent
	networkPrefixSubnet := &net.IPNet{
		IP:   subnet.IP,
		Mask: net.CIDRMask(networkPrefix, totalBits),
	}
	networkIndex = getSubnetIndexInParent(connectCIDR.IP, networkPrefixSubnet)

	return networkIndex, maxNodes, nil
}

// getLayer2SubIndex returns the index for a Layer2 /31 (or /127) subnet within its /networkPrefix block.
func getLayer2SubIndex(subnet *net.IPNet, networkPrefix int) int {
	// Calculate the base IP of the /networkPrefix block this /31 belongs to
	totalBits := getTotalBits(subnet.IP)
	blockBaseIP := subnet.IP.Mask(net.CIDRMask(networkPrefix, totalBits))
	return getSubnetIndexInParent(blockBaseIP, subnet)
}

// GetTunnelKey calculates the tunnel key for a network based on its topology type.
// For Layer3: tunnelKey = networkIndex * maxNodes + nodeID + 1
// For Layer2: tunnelKey = networkIndex * maxNodes + subIndex + 1 (where subIndex is derived from subnet)
// The +1 ensures tunnel keys are always > 0 (0 is reserved/invalid).
func GetTunnelKey(connectSubnets []networkconnectv1.ConnectSubnet, allocatedSubnets []*net.IPNet, topologyType string, nodeID int) (int, error) {
	if len(allocatedSubnets) == 0 {
		return 0, fmt.Errorf("no allocated subnets provided")
	}
	subnet := allocatedSubnets[0]

	// Get connect subnet info once for use by both calculations.
	// In dual-stack, the first subnet is IPv4 (added first during annotation parsing).
	// The tunnel key calculation is consistent across IP families because CNC has CEL validation
	// ensuring (32 - IPv4NetworkPrefix) == (128 - IPv6NetworkPrefix), so maxNodes is the same.
	networkPrefix, connectCIDR, err := getNetworkPrefixAndConnectCIDR(connectSubnets, subnet)
	if err != nil {
		return 0, err
	}

	networkIndex, maxNodes, err := getNetworkIndexAndMaxNodes(subnet, networkPrefix, connectCIDR)
	if err != nil {
		return 0, fmt.Errorf("failed to get network index and max nodes: %v", err)
	}

	if topologyType == ovntypes.Layer2Topology {
		subIndex := getLayer2SubIndex(subnet, networkPrefix)
		return networkIndex*maxNodes + subIndex + 1, nil
	}
	// Layer3
	return networkIndex*maxNodes + nodeID + 1, nil
}

// connectPortPairInfo contains the IP addresses for the connect port and the network port
// and the corresponding nodeID (layer3 and 0 for layer2), tunnelKey
type connectPortPairInfo struct {
	connectPortIPs []*net.IPNet
	networkPortIPs []*net.IPNet
}

// GetP2PAddresses calculates /31 (IPv4) or /127 (IPv6) point-to-point addresses for a node.
// It takes the allocated subnets and nodeID, and returns both IPs of the P2P subnet.
// The first IP is typically used for the router side, the second for the network side.
func GetP2PAddresses(subnets []*net.IPNet, nodeID int) (*connectPortPairInfo, error) {
	portPairInfo := &connectPortPairInfo{
		connectPortIPs: make([]*net.IPNet, 0),
		networkPortIPs: make([]*net.IPNet, 0),
	}
	for _, subnet := range subnets {
		generator, err := ip.NewIPGenerator(subnet.String())
		if err != nil {
			return nil, fmt.Errorf("failed to create IP generator: %v", err)
		}
		// Use GenerateIPPair to get two IPs forming a /31 or /127 subnet
		p2pSubnet, _, err := generator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate P2P subnet for node ID %d: %v", nodeID, err)
		}

		// First IP of the P2P subnet
		firstIPNet := &net.IPNet{
			IP:   p2pSubnet.IP,
			Mask: p2pSubnet.Mask,
		}
		portPairInfo.connectPortIPs = append(portPairInfo.connectPortIPs, firstIPNet)

		// Second IP (increment the last byte)
		secondIP := make(net.IP, len(p2pSubnet.IP))
		copy(secondIP, p2pSubnet.IP)
		secondIP[len(secondIP)-1]++
		secondIPNet := &net.IPNet{
			IP:   secondIP,
			Mask: p2pSubnet.Mask,
		}
		portPairInfo.networkPortIPs = append(portPairInfo.networkPortIPs, secondIPNet)
	}
	return portPairInfo, nil
}
