package udn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	udn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type TransitRouterInfo struct {
	GatewayRouterNets, TransitRouterNets []*net.IPNet
	NodeID                               int
}

// GetTransitRouterInfo calculates the gateway and cluster router networks for every node based on the node ID.
// we use transitSwitchSubnet to split it into smaller networks.
// For transit-subnet: 100.88.0.0/16, and nodeID=2, we will get:
// TODO use /31, cache results?
//   - Network: 			100.88.0.4/30
//   - Transit Router IP:	100.88.0.5/30
//   - Gateway Router IP:   100.88.0.6/30
func GetTransitRouterInfo(node *corev1.Node) (*TransitRouterInfo, error) {
	nodeID := util.GetNodeID(node)
	if nodeID == util.InvalidNodeID {
		return nil, fmt.Errorf("invalid node id calculating transit router networks")
	}
	routerInfo := &TransitRouterInfo{
		NodeID: nodeID,
	}
	if config.IPv4Mode {
		ipGenerator, err := udn.NewIPGenerator(config.ClusterManager.V4TransitSwitchSubnet)
		if err != nil {
			return nil, err
		}
		transitRouterIP, gatewayRouterIP, err := ipGenerator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, err
		}

		routerInfo.TransitRouterNets = append(routerInfo.TransitRouterNets, transitRouterIP)
		routerInfo.GatewayRouterNets = append(routerInfo.GatewayRouterNets, gatewayRouterIP)
	}

	if config.IPv6Mode {
		ipGenerator, err := udn.NewIPGenerator(config.ClusterManager.V6TransitSwitchSubnet)
		if err != nil {
			return nil, err
		}

		transitRouterIP, gatewayRouterIP, err := ipGenerator.GenerateIPPair(nodeID)
		if err != nil {
			return nil, err
		}

		routerInfo.TransitRouterNets = append(routerInfo.TransitRouterNets, transitRouterIP)
		routerInfo.GatewayRouterNets = append(routerInfo.GatewayRouterNets, gatewayRouterIP)
	}
	return routerInfo, nil
}
