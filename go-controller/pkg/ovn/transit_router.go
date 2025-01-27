package ovn

import (
	"fmt"
	"net"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func layer2TransitNetworksPerNode(node *corev1.Node) (gatewayRouterNetworks, clusterRouterNetworks []*net.IPNet, err error) {
	nodeID := util.GetNodeID(node)
	if nodeID == util.InvalidNodeID {
		return nil, nil, fmt.Errorf("invalid node id calculating transit router networks")
	}
	if config.IPv4Mode {
		_, v4TransitSwitchCIDR, err := net.ParseCIDR(config.ClusterManager.V4TransitSwitchSubnet)
		if err != nil {
			return nil, nil, err
		}
		// We need to reserver 4 since two of them will be
		// "network" aka .0 and "broadcast" aka .1
		v4NumberOfIPs := 4
		v4Mask := net.CIDRMask(32-v4NumberOfIPs/2, 32)
		v4Max := nodeID * v4NumberOfIPs

		v4GatewayRouterNetwork := *v4TransitSwitchCIDR
		v4GatewayRouterNetwork.Mask = v4Mask
		for range v4Max - 2 {
			v4GatewayRouterNetwork.IP = iputils.NextIP(v4GatewayRouterNetwork.IP)
		}
		gatewayRouterNetworks = append(gatewayRouterNetworks, &v4GatewayRouterNetwork)

		v4ClusterRouterNetwork := *v4TransitSwitchCIDR
		v4ClusterRouterNetwork.Mask = v4Mask
		for range v4Max - 3 {
			v4ClusterRouterNetwork.IP = iputils.NextIP(v4ClusterRouterNetwork.IP)
		}
		clusterRouterNetworks = append(clusterRouterNetworks, &v4ClusterRouterNetwork)
	}

	if config.IPv6Mode {
		_, v6TransitSwitchCIDR, err := net.ParseCIDR(config.ClusterManager.V6TransitSwitchSubnet)
		if err != nil {
			return nil, nil, err
		}

		v6NumberOfIPs := 2
		v6Mask := net.CIDRMask(128-v6NumberOfIPs/2, 128)
		v6Max := nodeID * v6NumberOfIPs
		v6GatewayRouterNetwork := *v6TransitSwitchCIDR
		v6GatewayRouterNetwork.Mask = v6Mask
		for range v6Max + 1 {
			v6GatewayRouterNetwork.IP = iputils.NextIP(v6GatewayRouterNetwork.IP)
		}
		gatewayRouterNetworks = append(gatewayRouterNetworks, &v6GatewayRouterNetwork)

		v6ClusterRouterNetwork := *v6TransitSwitchCIDR
		v6ClusterRouterNetwork.Mask = v6Mask
		for range v6Max {
			v6ClusterRouterNetwork.IP = iputils.NextIP(v6ClusterRouterNetwork.IP)
		}
		clusterRouterNetworks = append(clusterRouterNetworks, &v6ClusterRouterNetwork)
	}
	return gatewayRouterNetworks, clusterRouterNetworks, nil
}
