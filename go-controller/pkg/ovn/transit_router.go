package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"

	udn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type transitRouterInfo struct {
	gatewayRouterNets, transitRouterNets []*net.IPNet
	nodeID                               int
}

// getTransitRouterInfo calculates the gateway and cluster router networks for every node based on the node ID.
// we use netInfo.TransitSubnets() to split it into smaller networks.
// For transit-subnet: 100.88.0.0/16, and nodeID=2, we will get:
//   - Transit Router IP:	100.88.0.4/31
//   - Gateway Router IP:   100.88.0.5/31
func getTransitRouterInfo(netInfo util.NetInfo, node *corev1.Node) (*transitRouterInfo, error) {
	if netInfo.TopologyType() != types.Layer2Topology || !netInfo.IsPrimaryNetwork() {
		return nil, fmt.Errorf("transit router networks are only calculated for primary L2 user defined networks")
	}
	nodeID, _ := util.GetNodeID(node)
	if nodeID == util.InvalidNodeID {
		return nil, fmt.Errorf("invalid node id calculating transit router networks")
	}
	routerInfo := &transitRouterInfo{
		nodeID: nodeID,
	}
	for _, transitSubnet := range netInfo.TransitSubnets() {
		ipGenerator, err := udn.NewIPGenerator(transitSubnet.String())
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
	if len(routerInfo.transitRouterNets) == 0 || len(routerInfo.gatewayRouterNets) == 0 {
		return nil, fmt.Errorf("network %s has no transit subnets defined", netInfo.GetNetworkName())
	}
	return routerInfo, nil
}
