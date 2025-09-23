package kubevirt

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	corev1 "k8s.io/api/core/v1"

	kubevirtv1 "kubevirt.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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

func GenerateGatewayMAC(node *corev1.Node, joinSubnets []string) (string, error) {
	config.IPv4Mode = true
	lrpJoinAddress, err := GetDefaultUDNGWRouterIPs(node, joinSubnets)
	if err != nil {
		return "", err
	}

	if len(lrpJoinAddress) == 0 {
		return "", fmt.Errorf("missing lrp join ip at node %q", node.Name)
	}

	return util.IPAddrToHWAddr(*lrpJoinAddress[0]).String(), nil
}

func GenerateGatewayIPv6RouterLLA(node *corev1.Node, joinSubnets []string) (string, error) {
	config.IPv4Mode = true
	joinAddresses, err := GetDefaultUDNGWRouterIPs(node, joinSubnets)
	if err != nil {
		return "", err
	}
	if len(joinAddresses) == 0 {
		return "", fmt.Errorf("missing join addresses at node %q", node.Name)
	}
	return util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(*joinAddresses[0])).String(), nil
}

func GetDefaultUDNGWRouterIPs(node *corev1.Node, joinSubnets []string) ([]*net.IP, error) {
	nodeID, err := util.GetNodeID(node)
	if err != nil {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return nil, err
	}
	var udnJoinNetv4, udnJoinNetv6 net.IP
	for _, subnet := range joinSubnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %q: %v", subnet, err)
		}
		if ip.To4() != nil {
			udnJoinNetv4 = ip
		} else {
			udnJoinNetv6 = ip
		}
	}
	res := []*net.IP{}
	if config.IPv4Mode {
		for range nodeID {
			udnJoinNetv4 = iputils.NextIP(udnJoinNetv4)
		}
		res = append(res, &udnJoinNetv4)
	}
	if config.IPv6Mode {
		for range nodeID {
			udnJoinNetv6 = iputils.NextIP(udnJoinNetv6)
		}
		res = append(res, &udnJoinNetv6)
	}
	return res, nil
}
