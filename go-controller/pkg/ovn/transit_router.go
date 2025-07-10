package ovn

import (
	"fmt"
	"net"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// layer2TransitNetworksPerNode calculates the gateway and cluster router networks for every node based on the node ID.
// we use transitSwitchSubnet to split it into smaller networks.
// For transit-subnet: 100.88.0.0/16, and nodeID=2, we will get:
// TODO use /31, cache results?
//   - Network: 			100.88.0.4/30
//   - Cluster Router IP:	100.88.0.5/30
//   - Gateway Router IP:   100.88.0.6/30
func layer2TransitNetworksPerNode(node *corev1.Node) (gatewayRouterNetworks, clusterRouterNetworks []*net.IPNet, err error) {
	nodeID := util.GetNodeID(node)
	if nodeID == util.InvalidNodeID {
		return nil, nil, fmt.Errorf("invalid node id calculating transit router networks")
	}
	if config.IPv4Mode {
		_, v4TransitSwitchCIDR, err := net.ParseCIDR(config.Gateway.V4JoinSubnet)
		if err != nil {
			return nil, nil, err
		}
		// We need to reserver 4 IPs since two of them will be
		// "network" aka .0 and "broadcast" aka .1,
		// we use 2 more for GW and transit router ports
		v4NumberOfIPs := 4
		v4Mask := net.CIDRMask(30, 32)
		// nodeIDs start from 1, netIP is the first IP of the subnet
		netIP := ipWithOffset(v4TransitSwitchCIDR.IP, (nodeID-1)*v4NumberOfIPs)
		clusterRouterIP := iputils.NextIP(netIP)
		gwRouterIP := iputils.NextIP(clusterRouterIP)

		clusterRouterNetworks = append(clusterRouterNetworks, &net.IPNet{IP: clusterRouterIP, Mask: v4Mask})
		gatewayRouterNetworks = append(gatewayRouterNetworks, &net.IPNet{IP: gwRouterIP, Mask: v4Mask})
	}

	if config.IPv6Mode {
		_, v6TransitSwitchCIDR, err := net.ParseCIDR(config.Gateway.V6JoinSubnet)
		if err != nil {
			return nil, nil, err
		}

		v6NumberOfIPs := 2
		v6Mask := net.CIDRMask(127, 128)
		// nodeIDs start from 1, clusterRouterIP is the first IP of the subnet
		clusterRouterIP := ipWithOffset(v6TransitSwitchCIDR.IP, (nodeID-1)*v6NumberOfIPs)
		gwRouterIP := iputils.NextIP(clusterRouterIP)

		clusterRouterNetworks = append(clusterRouterNetworks, &net.IPNet{IP: clusterRouterIP, Mask: v6Mask})
		gatewayRouterNetworks = append(gatewayRouterNetworks, &net.IPNet{IP: gwRouterIP, Mask: v6Mask})
	}
	return gatewayRouterNetworks, clusterRouterNetworks, nil
}

func ipWithOffset(ip net.IP, offset int) net.IP {
	return utilnet.AddIPOffset(utilnet.BigForIP(ip), offset)
}
