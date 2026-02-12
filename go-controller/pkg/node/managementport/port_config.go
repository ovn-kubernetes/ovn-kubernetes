package managementport

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type managementPortConfig struct {
	nodeName    string
	hostSubnets []*net.IPNet

	mpMAC net.HardwareAddr
	gwMAC net.HardwareAddr

	ipv4 *managementPortIPFamilyConfig
	ipv6 *managementPortIPFamilyConfig

	netInfo util.NetInfo
}

func newManagementPortConfig(node *corev1.Node, hostSubnets []*net.IPNet, netInfo util.NetInfo) (*managementPortConfig, error) {
	// Kubernetes emits events when pods are created. The event will contain
	// only lowercase letters of the hostname even though the kubelet is
	// started with a hostname that contains lowercase and uppercase letters.
	// When the kubelet is started with a hostname containing lowercase and
	// uppercase letters, this causes a mismatch between what the watcher
	// will try to fetch and what kubernetes provides, thus failing to
	// create the port on the logical switch.
	// Until the above is changed, switch to a lowercase hostname
	nodeName := strings.ToLower(node.Name)

	// find suitable MAC address
	// check node annotation first, to ensure we are not picking a new MAC when one was already configured
	mpMAC, err := util.ParseNodeManagementPortMACAddresses(node, types.DefaultNetworkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return nil, err
	}
	if len(mpMAC) == 0 {
		// calculate mac from subnets
		if len(hostSubnets) == 0 {
			return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", types.DefaultNetworkName)
		}
		mpMAC = util.IPAddrToHWAddr(netInfo.GetNodeManagementIP(hostSubnets[0]).IP)
	}

	mpcfg := &managementPortConfig{
		nodeName:    nodeName,
		hostSubnets: hostSubnets,
		mpMAC:       mpMAC,
		netInfo:     netInfo,
	}

	for _, hostSubnet := range hostSubnets {
		isIPv6 := utilnet.IsIPv6CIDR(hostSubnet)

		var family string
		if isIPv6 {
			if mpcfg.ipv6 != nil {
				klog.Warningf("Ignoring duplicate IPv6 hostSubnet %s", hostSubnet)
				continue
			}
			family = "IPv6"
		} else {
			if mpcfg.ipv4 != nil {
				klog.Warningf("Ignoring duplicate IPv4 hostSubnet %s", hostSubnet)
				continue
			}
			family = "IPv4"
		}

		cfg, err := newManagementPortIPFamilyConfig(hostSubnet, isIPv6, netInfo)
		if err != nil {
			return nil, err
		}
		if len(cfg.clusterSubnets) == 0 {
			klog.Warningf("Ignoring %s hostSubnet %s due to lack of %s cluster networks", family, hostSubnet, family)
			continue
		}

		if isIPv6 {
			mpcfg.ipv6 = cfg
		} else {
			mpcfg.ipv4 = cfg
		}
	}

	if mpcfg.ipv4 != nil {
		mpcfg.gwMAC = util.IPAddrToHWAddr(mpcfg.ipv4.gwIP)
	} else if mpcfg.ipv6 != nil {
		mpcfg.gwMAC = util.IPAddrToHWAddr(mpcfg.ipv6.gwIP)
	} else {
		return nil, fmt.Errorf("management port configured with neither IPv4 nor IPv6 subnets")
	}

	return mpcfg, nil
}

func (mpcfg *managementPortConfig) getAddresses() []*net.IPNet {
	var addresses []*net.IPNet
	if mpcfg.ipv4 != nil && mpcfg.ipv4.ifAddr != nil {
		addresses = append(addresses, mpcfg.ipv4.ifAddr)
	}
	if mpcfg.ipv6 != nil && mpcfg.ipv6.ifAddr != nil {
		addresses = append(addresses, mpcfg.ipv6.ifAddr)
	}
	return addresses
}

type managementPortIPFamilyConfig struct {
	clusterSubnets []*net.IPNet
	ifAddr         *net.IPNet
	gwIP           net.IP
}

func newManagementPortIPFamilyConfig(hostSubnet *net.IPNet, isIPv6 bool, netInfo util.NetInfo) (*managementPortIPFamilyConfig, error) {
	cfg := &managementPortIPFamilyConfig{
		ifAddr: netInfo.GetNodeManagementIP(hostSubnet),
		gwIP:   netInfo.GetNodeGatewayIP(hostSubnet).IP,
	}

	// capture all the subnets for which we need to add routes through management port
	for _, subnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv6CIDR(subnet.CIDR) == isIPv6 {
			cfg.clusterSubnets = append(cfg.clusterSubnets, subnet.CIDR)
		}
	}
	// add the .3 masqueradeIP to add the route via mp0 for ETP=local case
	// used only in LGW but we create it in SGW as well to maintain parity.
	if isIPv6 {
		_, masqueradeSubnet, err := net.ParseCIDR(config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String() + "/128")
		if err != nil {
			return nil, err
		}
		cfg.clusterSubnets = append(cfg.clusterSubnets, masqueradeSubnet)
	} else {
		_, masqueradeSubnet, err := net.ParseCIDR(config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String() + "/32")
		if err != nil {
			return nil, err
		}
		cfg.clusterSubnets = append(cfg.clusterSubnets, masqueradeSubnet)
	}

	return cfg, nil
}
