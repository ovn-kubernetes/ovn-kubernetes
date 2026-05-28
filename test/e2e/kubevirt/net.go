// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	kubevirtv1 "kubevirt.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func RetrieveCachedGatewayMAC(cli *Client, vmi *kubevirtv1.VirtualMachineInstance, dev, cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}

	gatewayIP := util.GetNodeGatewayIfAddr(ipNet).IP.String()

	output, err := cli.RunCommand(vmi, fmt.Sprintf("ip neigh get %s dev %s", gatewayIP, dev), 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("%s: %v", output, err)
	}
	outputSplit := strings.Split(output, " ")
	if len(outputSplit) < 5 {
		return "", fmt.Errorf("unexpected 'ip neigh' output %q", output)
	}
	return outputSplit[4], nil
}

func RetrieveIPv6Gateways(cli *Client, vmi *v1.VirtualMachineInstance) ([]string, error) {
	routes := []struct {
		Gateway string `json:"gateway"`
	}{}

	output, err := cli.RunCommand(vmi, "ip -6 -j route list default", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	paths := []string{}
	for _, route := range routes {
		paths = append(paths, route.Gateway)
	}
	return paths, nil
}

func GenerateGatewayMAC(subnets []string) (string, error) {
	config.IPv4Mode = true
	defaultGWIPs, err := GetLayer2UDNDefaultGWIPs(subnets)
	if err != nil {
		return "", err
	}

	if len(defaultGWIPs) == 0 {
		return "", fmt.Errorf("can't find default GW IP for subnets %v", subnets)
	}

	return util.IPAddrToHWAddr(*defaultGWIPs[0]).String(), nil
}

func GenerateGatewayIPv6RouterLLA(subnets []string) (string, error) {
	config.IPv4Mode = true
	defaultGWIPs, err := GetLayer2UDNDefaultGWIPs(subnets)
	if err != nil {
		return "", err
	}
	if len(defaultGWIPs) == 0 {
		return "", fmt.Errorf("can't find default GW IP for subnets %v", subnets)
	}
	return util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(*defaultGWIPs[0])).String(), nil
}

// GetLayer2UDNDefaultGWIPs returns the default gateway IPs (.1) for a Layer2 UDN subnet
func GetLayer2UDNDefaultGWIPs(subnets []string) ([]*net.IP, error) {
	var udnJoinNetv4, udnJoinNetv6 net.IP
	for _, subnet := range subnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %q: %v", subnet, err)
		}
		if ip.To4() != nil {
			udnJoinNetv4 = iputils.NextIP(ip)
		} else {
			udnJoinNetv6 = iputils.NextIP(ip)
		}
	}
	res := []*net.IP{}
	if config.IPv4Mode {
		res = append(res, &udnJoinNetv4)
	}
	if config.IPv6Mode {
		res = append(res, &udnJoinNetv6)
	}
	return res, nil
}
