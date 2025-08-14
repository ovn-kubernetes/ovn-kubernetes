package udn

import (
	"fmt"
	"math/big"
	"net"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ipgenerator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func GetGWRouterIPv4(node *corev1.Node, netInfo util.NetInfo) (net.IP, error) {
	gwRouterIPs, err := GetGWRouterIPs(node, netInfo)
	if err != nil {
		return nil, err
	}
	var gwRouterIPv4 net.IP
	for _, gwRouterIP := range gwRouterIPs {
		if utilnet.IsIPv4(gwRouterIP.IP) {
			gwRouterIPv4 = gwRouterIP.IP
			break
		}
	}
	if gwRouterIPv4 == nil {
		return nil, fmt.Errorf("failed to find an IPv4 address for gateway router in node: %s, net: %s",
			node.Name, netInfo.GetNetworkName())
	}
	return gwRouterIPv4, nil
}

func GetGWRouterIPv6(node *corev1.Node, netInfo util.NetInfo) (net.IP, error) {
	gwRouterIPs, err := GetGWRouterIPs(node, netInfo)
	if err != nil {
		return nil, err
	}
	var gwRouterIPv6 net.IP
	for _, gwRouterIP := range gwRouterIPs {
		if utilnet.IsIPv6(gwRouterIP.IP) {
			gwRouterIPv6 = gwRouterIP.IP
			break
		}
	}
	if gwRouterIPv6 == nil {
		return nil, fmt.Errorf("failed to find an IPv6 address for gateway router in node: %s, net: %s",
			node.Name, netInfo.GetNetworkName())
	}
	return gwRouterIPv6, nil
}

// TODO this can be moved to netInfo to avoid instantiating the same IPGenerator
func GetGWRouterIPs(node *corev1.Node, netInfo util.NetInfo) ([]*net.IPNet, error) {
	var gwRouterAddrs []*net.IPNet
	// we allocate join subnets for L3/L2 primary user defined networks or default network
	if !(netInfo.IsDefault() || (util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork())) {
		return gwRouterAddrs, nil
	}
	// Allocate the IP address(es) for the node Gateway router port connecting
	// to the Join switch
	nodeID, err := util.GetNodeID(node)
	if err != nil {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return nil, fmt.Errorf("failed to generate gateway router port address for node %s: %w", node.Name, err)
	}
	if config.IPv4Mode {
		gwRouterAddr, err := getGWRouterIP(netInfo.JoinSubnetV4().String(), nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate gateway router port ipv4 address for node %s : err - %w", node.Name, err)
		}
		gwRouterAddrs = append(gwRouterAddrs, gwRouterAddr)
	}
	if config.IPv6Mode {
		gwRouterAddr, err := getGWRouterIP(netInfo.JoinSubnetV6().String(), nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate gateway router port ipv6 address for node %s : err - %w", node.Name, err)
		}
		gwRouterAddrs = append(gwRouterAddrs, gwRouterAddr)
	}
	return gwRouterAddrs, nil
}

func getGWRouterIP(subnet string, nodeID int) (*net.IPNet, error) {
	nodeGWRouterLRPIPGenerator, err := ipgenerator.NewIPGenerator(subnet)
	if err != nil {
		return nil, fmt.Errorf("error creating IP Generator for subnet %s: %w", subnet, err)
	}
	return nodeGWRouterLRPIPGenerator.GenerateIP(nodeID)
}

func GetLastIPsFromJoinSubnet(netInfo util.NetInfo) []*net.IPNet {
	var gwRouterAddrs []*net.IPNet
	if config.IPv4Mode {
		gwRouterAddrs = append(gwRouterAddrs, getLastIPOfSubnet(netInfo.JoinSubnetV4()))
	}
	if config.IPv6Mode {
		gwRouterAddrs = append(gwRouterAddrs, getLastIPOfSubnet(netInfo.JoinSubnetV6()))
	}
	return gwRouterAddrs
}

func getLastIPOfSubnet(subnet *net.IPNet) *net.IPNet {
	mask, total := subnet.Mask.Size()
	base := big.NewInt(1)
	totalIPs := new(big.Int).Lsh(base, uint(total-mask))
	lastIPIndex := totalIPs.Sub(totalIPs, big.NewInt(int64(2)))
	// this is copied form utilnet.AddIPOffset but to allow big.Int offset
	r := big.NewInt(0).Add(utilnet.BigForIP(subnet.IP), lastIPIndex).Bytes()
	r = append(make([]byte, 16), r...)
	lastIP := net.IP(r[len(r)-16:])
	return &net.IPNet{IP: lastIP, Mask: subnet.Mask}
}
