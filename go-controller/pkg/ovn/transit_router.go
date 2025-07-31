package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	udn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type transitRouterInfo struct {
	gatewayRouterNets, transitRouterNets []*net.IPNet
	nodeID                               int
}

// getTransitRouterInfo calculates the gateway and cluster router networks for every node based on the node ID.
// we use transitSwitchSubnet to split it into smaller networks.
// For transit-subnet: 100.88.0.0/16, and nodeID=2, we will get:
//   - Transit Router IP:	100.88.0.4/31
//   - Gateway Router IP:   100.88.0.5/31
func getTransitRouterInfo(node *corev1.Node) (*transitRouterInfo, error) {
	nodeID, _ := util.GetNodeID(node)
	if nodeID == util.InvalidNodeID {
		return nil, fmt.Errorf("invalid node id calculating transit router networks")
	}
	routerInfo := &transitRouterInfo{
		nodeID: nodeID,
	}
	if config.IPv4Mode {
		ipGenerator, err := udn.NewIPGenerator(config.ClusterManager.V4TransitSubnet)
		if err != nil {
			return nil, err
		}
		transitRouterIP, gatewayRouterIP, err := ipGenerator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, err
		}

		routerInfo.transitRouterNets = append(routerInfo.transitRouterNets, transitRouterIP)
		routerInfo.gatewayRouterNets = append(routerInfo.gatewayRouterNets, gatewayRouterIP)
	}

	if config.IPv6Mode {
		ipGenerator, err := udn.NewIPGenerator(config.ClusterManager.V6TransitSubnet)
		if err != nil {
			return nil, err
		}

		transitRouterIP, gatewayRouterIP, err := ipGenerator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, err
		}

		routerInfo.transitRouterNets = append(routerInfo.transitRouterNets, transitRouterIP)
		routerInfo.gatewayRouterNets = append(routerInfo.gatewayRouterNets, gatewayRouterIP)
	}
	return routerInfo, nil
}
