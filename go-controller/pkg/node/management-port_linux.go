//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	iptableMgmPortChain = "OVN-KUBE-SNAT-MGMTPORT"
)

type managementPortIPFamilyConfig struct {
	ipt        util.IPTablesHelper
	allSubnets []*net.IPNet
	ifAddr     *net.IPNet
	gwIP       net.IP
}

type managementPortConfig struct {
	ifName    string
	link      netlink.Link
	routerMAC net.HardwareAddr

	ipv4 *managementPortIPFamilyConfig
	ipv6 *managementPortIPFamilyConfig
}

func newManagementPortIPFamilyConfig(hostSubnet *net.IPNet, isIPv6 bool) (*managementPortIPFamilyConfig, error) {
	var err error

	cfg := &managementPortIPFamilyConfig{
		ifAddr: util.GetNodeManagementIfAddr(hostSubnet),
		gwIP:   util.GetNodeGatewayIfAddr(hostSubnet).IP,
	}

	// capture all the subnets for which we need to add routes through management port
	for _, subnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv6CIDR(subnet.CIDR) == isIPv6 {
			cfg.allSubnets = append(cfg.allSubnets, subnet.CIDR)
		}
	}
	// add the .3 masqueradeIP to add the route via mp0 for ETP=local case
	// used only in LGW but we create it in SGW as well to maintain parity.
	if isIPv6 {
		_, masqueradeSubnet, err := net.ParseCIDR(types.V6HostETPLocalMasqueradeIP + "/128")
		if err != nil {
			return nil, err
		}
		cfg.allSubnets = append(cfg.allSubnets, masqueradeSubnet)
	} else {
		_, masqueradeSubnet, err := net.ParseCIDR(types.V4HostETPLocalMasqueradeIP + "/32")
		if err != nil {
			return nil, err
		}
		cfg.allSubnets = append(cfg.allSubnets, masqueradeSubnet)
	}

	if utilnet.IsIPv6CIDR(cfg.ifAddr) {
		cfg.ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv6)
	} else {
		cfg.ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv4)
	}
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func newManagementPortConfig(interfaceName string, hostSubnets []*net.IPNet) (*managementPortConfig, error) {
	var err error

	mpcfg := &managementPortConfig{
		ifName: interfaceName,
	}
	if mpcfg.link, err = util.LinkSetUp(mpcfg.ifName); err != nil {
		return nil, err
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

		cfg, err := newManagementPortIPFamilyConfig(hostSubnet, isIPv6)
		if err != nil {
			return nil, err
		}
		if len(cfg.allSubnets) == 0 {
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
		mpcfg.routerMAC = util.IPAddrToHWAddr(mpcfg.ipv4.gwIP)
	} else if mpcfg.ipv6 != nil {
		mpcfg.routerMAC = util.IPAddrToHWAddr(mpcfg.ipv6.gwIP)
	} else {
		klog.Fatalf("Management port configured with neither IPv4 nor IPv6 subnets")
	}

	return mpcfg, nil
}

func tearDownManagementPortConfig(mpcfg *managementPortConfig, desiredRules ...iptRule) error {
	// for the initial setup we need to start from the clean slate, so flush
	// all (non-LL) addresses on this link, routes through this link, and
	// finally any IPtable rules for this link.
	if err := util.LinkAddrFlush(mpcfg.link); err != nil {
		return err
	}

	if err := util.LinkRoutesDel(mpcfg.link, nil); err != nil {
		return err
	}
	if mpcfg.ipv4 != nil {
		if err := leaveOnlyTheseRules(
			mpcfg.ipv4,
			iptableMgmPortChain,
			filterRulesByPredicate(func(rule iptRule) bool {
				return rule.protocol == iptables.ProtocolIPv4
			}, desiredRules...)...); err != nil {
			return fmt.Errorf("could not stomp on all rules in the management chain for IPv4 protocol: %v", err)
		}
	}

	if mpcfg.ipv6 != nil {
		if err := leaveOnlyTheseRules(
			mpcfg.ipv6,
			iptableMgmPortChain,
			filterRulesByPredicate(func(rule iptRule) bool {
				return rule.protocol == iptables.ProtocolIPv6
			}, desiredRules...)...); err != nil {
			return fmt.Errorf("could not stomp on all rules in the management chain for IPv6 protocol: %v", err)
		}
	}

	return nil
}

func leaveOnlyTheseRules(iptablesManager *managementPortIPFamilyConfig, chain string, desiredRules ...iptRule) error {
	if rules, err := iptablesManager.ipt.List("nat", chain); err == nil {
		for _, rule := range rules {
			if shouldSkipRule(rule) {
				continue
			}
			for _, desiredRule := range desiredRules {
				if !desiredRule.IsEqual(rule) {
					if err := iptablesManager.ipt.Delete("nat", chain, ruleSpec(rule)...); err != nil {
						return err
					}
				}
			}
		}
	} else {
		return err
	}
	return nil
}

func setupManagementPortIPFamilyConfig(mpcfg *managementPortConfig, cfg *managementPortIPFamilyConfig) ([]string, error) {
	var warnings []string
	var err error
	var exists bool

	if exists, err = util.LinkAddrExist(mpcfg.link, cfg.ifAddr); err == nil && !exists {
		// we should log this so that one can debug as to why addresses are
		// disappearing
		warnings = append(warnings, fmt.Sprintf("missing IP address %s on the interface %s, adding it...",
			cfg.ifAddr, mpcfg.ifName))
		err = util.LinkAddrAdd(mpcfg.link, cfg.ifAddr)
	}
	if err != nil {
		return warnings, err
	}

	for _, subnet := range cfg.allSubnets {
		if exists, err = util.LinkRouteExists(mpcfg.link, cfg.gwIP, subnet); err == nil && !exists {
			// we need to warn so that it can be debugged as to why routes are disappearing
			warnings = append(warnings, fmt.Sprintf("missing route entry for subnet %s via gateway %s on link %v",
				subnet, cfg.gwIP, mpcfg.ifName))
		}
		if err != nil {
			return warnings, err
		}

		err = util.LinkRoutesAddOrUpdateSourceOrMTU(mpcfg.link, cfg.gwIP, []*net.IPNet{subnet}, config.Default.RoutableMTU, nil)
		if err != nil {
			return warnings, err
		}
	}

	// Add a neighbour entry on the K8s node to map routerIP with routerMAC. This is
	// required because in certain cases ARP requests from the K8s Node to the routerIP
	// arrives on OVN Logical Router pipeline with ARP source protocol address set to
	// K8s Node IP. OVN Logical Router pipeline drops such packets since it expects
	// source protocol address to be in the Logical Switch's subnet.
	if exists, err = util.LinkNeighExists(mpcfg.link, cfg.gwIP, mpcfg.routerMAC); err == nil && !exists {
		warnings = append(warnings, fmt.Sprintf("missing arp entry for MAC/IP binding (%s/%s) on link %s",
			mpcfg.routerMAC.String(), cfg.gwIP, types.K8sMgmtIntfName))
		err = util.LinkNeighAdd(mpcfg.link, cfg.gwIP, mpcfg.routerMAC)
	}
	if err != nil {
		return warnings, err
	}

	if _, err = cfg.ipt.List("nat", iptableMgmPortChain); err != nil {
		warnings = append(warnings, fmt.Sprintf("missing iptables chain %s in the nat table, adding it",
			iptableMgmPortChain))
		err = cfg.ipt.NewChain("nat", iptableMgmPortChain)
	}
	if err != nil {
		return warnings, fmt.Errorf("could not create iptables nat chain %q for management port: %v",
			iptableMgmPortChain, err)
	}

	if err := ensureIptRules(mngtPortRules(mpcfg, cfg)); err != nil {
		return warnings, err
	}

	return warnings, nil
}

func mngtPortRules(mpcfg *managementPortConfig, cfg *managementPortIPFamilyConfig) []iptRule {
	return []iptRule{
		generateNATPostRoutingJumpToMgmtPortChain(mpcfg.ifName, getIPTablesProtocol(cfg.gwIP.String())),
		generateNATMgmtChainSNATRule(mpcfg.ifName, cfg.ifAddr.IP),
	}
}

func setupManagementPortConfig(cfg *managementPortConfig) ([]string, error) {
	var warnings, allWarnings []string
	var err error

	if cfg.ipv4 != nil {
		warnings, err = setupManagementPortIPFamilyConfig(cfg, cfg.ipv4)
		allWarnings = append(allWarnings, warnings...)
	}
	if cfg.ipv6 != nil && err == nil {
		warnings, err = setupManagementPortIPFamilyConfig(cfg, cfg.ipv6)
		allWarnings = append(allWarnings, warnings...)
	}

	return allWarnings, err
}

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(interfaceName string, localSubnets []*net.IPNet) (*managementPortConfig, error) {
	var cfg *managementPortConfig
	var err error

	if cfg, err = newManagementPortConfig(interfaceName, localSubnets); err != nil {
		return nil, err
	}

	if cfg.ipv4 != nil {
		if err = tearDownManagementPortConfig(cfg, mngtPortRules(cfg, cfg.ipv4)...); err != nil {
			return nil, err
		}
	}
	if cfg.ipv6 != nil {
		if err = tearDownManagementPortConfig(cfg, mngtPortRules(cfg, cfg.ipv6)...); err != nil {
			return nil, err
		}
	}

	if _, err = setupManagementPortConfig(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// DelMgtPortIptRules delete all the iptable rules for the management port.
func DelMgtPortIptRules() {
	// Clean up all iptables and ip6tables remnants that may be left around
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return
	}
	ipt6, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return
	}
	rule := []string{"-o", types.K8sMgmtIntfName, "-j", iptableMgmPortChain}
	_ = ipt.Delete("nat", "POSTROUTING", rule...)
	_ = ipt6.Delete("nat", "POSTROUTING", rule...)
	_ = ipt.ClearChain("nat", iptableMgmPortChain)
	_ = ipt6.ClearChain("nat", iptableMgmPortChain)
	_ = ipt.DeleteChain("nat", iptableMgmPortChain)
	_ = ipt6.DeleteChain("nat", iptableMgmPortChain)
}

// checks to make sure that following configurations are present on the k8s node
// 1. route entries to cluster CIDR and service CIDR through management port
// 2. ARP entry for the node subnet's gateway ip
// 3. IPtables chain and rule for SNATing packets entering the logical topology
func checkManagementPortHealth(cfg *managementPortConfig) {
	warnings, err := setupManagementPortConfig(cfg)
	for _, warning := range warnings {
		klog.Warningf(warning)
	}
	if err != nil {
		klog.Errorf(err.Error())
	}
}

func generateNATPostRoutingJumpToMgmtPortChain(ifaceName string, proto iptables.Protocol) iptRule {
	return iptRule{
		table:    "nat",
		chain:    "POSTROUTING",
		args:     []string{"-o", ifaceName, "-j", iptableMgmPortChain},
		protocol: proto,
	}
}

func generateNATMgmtChainSNATRule(ifaceName string, snatIP net.IP) iptRule {
	return iptRule{
		table: "nat",
		chain: iptableMgmPortChain,
		args: []string{
			"-o",
			ifaceName,
			"-j",
			"SNAT",
			"--to-source",
			snatIP.String(),
			"-m",
			"comment",
			"--comment",
			"OVN SNAT to Management Port",
		},
		protocol: getIPTablesProtocol(snatIP.String()),
	}
}
