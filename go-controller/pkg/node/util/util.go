package util

import (
	"fmt"
	"net"

	net2 "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	pkgutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// GetNetworkInterfaceIPAddresses returns the IP addresses for the network interface 'iface'.
func GetNetworkInterfaceIPAddresses(iface string) ([]*net.IPNet, error) {
	allIPs, err := pkgutil.GetFilteredInterfaceV4V6IPs(iface)
	if err != nil {
		return nil, fmt.Errorf("could not find IP addresses: %v", err)
	}

	var ips []*net.IPNet
	var foundIPv4 bool
	var foundIPv6 bool
	for _, ip := range allIPs {
		if net2.IsIPv6CIDR(ip) {
			if config.IPv6Mode && !foundIPv6 {
				// For IPv6 addresses with 128 prefix, let's try to find an appropriate subnet
				// in the routing table
				subnetIP, err := pkgutil.GetIPv6OnSubnet(iface, ip)
				if err != nil {
					return nil, fmt.Errorf("could not find IPv6 address on subnet: %v", err)
				}
				ips = append(ips, subnetIP)
				foundIPv6 = true
			}
		} else if config.IPv4Mode && !foundIPv4 {
			ips = append(ips, ip)
			foundIPv4 = true
		}
	}
	if config.IPv4Mode && !foundIPv4 {
		return nil, fmt.Errorf("failed to find IPv4 address on interface %s", iface)
	} else if config.IPv6Mode && !foundIPv6 {
		return nil, fmt.Errorf("failed to find IPv6 address on interface %s", iface)
	}
	return ips, nil
}

// GetDPUHostPrimaryIPAddresses returns the DPU host IP/Network based on K8s Node IP
// and DPU IP subnet overriden by config config.Gateway.RouterSubnet
func GetDPUHostPrimaryIPAddresses(k8sNodeIP net.IP, ifAddrs []*net.IPNet) ([]*net.IPNet, error) {
	// Note(adrianc): No Dual-Stack support at this point as we rely on k8s node IP to derive gateway information
	// for each node.
	var gwIps []*net.IPNet
	isIPv4 := net2.IsIPv4(k8sNodeIP)

	// override subnet mask via config
	if config.Gateway.RouterSubnet != "" {
		_, addr, err := net.ParseCIDR(config.Gateway.RouterSubnet)
		if err != nil {
			return nil, err
		}
		if net2.IsIPv4CIDR(addr) != isIPv4 {
			return nil, fmt.Errorf("unexpected gateway router subnet provided (%s). "+
				"does not match Node IP address format", config.Gateway.RouterSubnet)
		}
		if !addr.Contains(k8sNodeIP) {
			return nil, fmt.Errorf("unexpected gateway router subnet provided (%s). "+
				"subnet does not contain Node IP address (%s)", config.Gateway.RouterSubnet, k8sNodeIP)
		}
		addr.IP = k8sNodeIP
		gwIps = append(gwIps, addr)
	} else {
		// Assume Host and DPU share the same subnet
		// in this case just update the matching IPNet with the Host's IP address
		for _, addr := range ifAddrs {
			if net2.IsIPv4CIDR(addr) != isIPv4 {
				continue
			}
			// expect k8s Node IP to be contained in the given subnet
			if !addr.Contains(k8sNodeIP) {
				continue
			}
			newAddr := *addr
			newAddr.IP = k8sNodeIP
			gwIps = append(gwIps, &newAddr)
		}
		if len(gwIps) == 0 {
			return nil, fmt.Errorf("could not find subnet on DPU matching node IP %s", k8sNodeIP)
		}
	}
	return gwIps, nil
}

func GenerateICMPFragmentationFlow(ipAddr, outputPort, inPort, cookie string, priority int) string {
	// we send any ICMP destination unreachable, fragmentation needed to the OVN pipeline too so that
	// path MTU discovery continues to work.
	icmpMatch := "icmp"
	icmpType := 3
	icmpCode := 4
	nwDst := "nw_dst"
	if net2.IsIPv6String(ipAddr) {
		icmpMatch = "icmp6"
		icmpType = 2
		icmpCode = 0
		nwDst = "ipv6_dst"
	}

	action := fmt.Sprintf("output:%s", outputPort)
	if outputPort == nodetypes.OutputPortDrop {
		action = "drop"
	}

	icmpFragmentationFlow := fmt.Sprintf("cookie=%s, priority=%d, in_port=%s, %s, %s=%s, icmp_type=%d, "+
		"icmp_code=%d, actions=%s",
		cookie, priority, inPort, icmpMatch, nwDst, ipAddr, icmpType, icmpCode, action)
	return icmpFragmentationFlow
}
