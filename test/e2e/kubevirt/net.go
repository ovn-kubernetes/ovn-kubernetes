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

func GenerateAddressDiscoveryConfigurationCommand(iface string) string {
	// since kindest/node v1.32+, which was upgraded with containerd v2, /sys is
	// mounted into kind pods as read/write which activates udev in them
	// including a rule that sets as unmanaged any veth device with a name not
	// starting with 'eth*'. To workaround, we force the device as managed here.
	return fmt.Sprintf(`
nmcli d set %[1]s managed true
nmcli c mod %[1]s ipv4.addresses "" ipv6.addresses "" ipv4.gateway "" ipv6.gateway "" ipv6.method auto ipv4.method auto ipv6.addr-gen-mode eui64
nmcli d reapply %[1]s`, iface)
}

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
