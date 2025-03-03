package kubevirt

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

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

func RetrieveCachedGatewayMAC(vmi *kubevirtv1.VirtualMachineInstance, dev, cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}

	gatewayIP := util.GetNodeGatewayIfAddr(ipNet).IP.String()

	output, err := RunCommand(vmi, fmt.Sprintf("ip neigh get %s dev %s", gatewayIP, dev), 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("%s: %v", output, err)
	}
	outputSplit := strings.Split(output, " ")
	if len(outputSplit) < 5 {
		return "", fmt.Errorf("unexpected 'ip neigh' output %q", output)
	}
	return outputSplit[4], nil
}

func RetrieveIPv6Gateways(vmi *v1.VirtualMachineInstance) ([]string, error) {
	routes := []struct {
		Gateway string `json:"gateway"`
	}{}

	output, err := RunCommand(vmi, "ip -6 -j route list default", 2*time.Second)
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

func GenerateGatewayMAC(node *corev1.Node, networkName string) (string, error) {
	config.IPv4Mode = true
	lrpJoinAddress, err := util.ParseNodeGatewayRouterJoinNetwork(node, networkName)
	if err != nil {
		return "", err
	}

	lrpJoinIPString := lrpJoinAddress.IPv4
	if lrpJoinIPString == "" {
		lrpJoinIPString = lrpJoinAddress.IPv6
	}

	if lrpJoinIPString == "" {
		return "", fmt.Errorf("missing lrp join ip at node %q with network %q", node.Name)
	}

	lrpJoinIP, _, err := net.ParseCIDR(lrpJoinIPString)
	if err != nil {
		return "", err
	}

	return util.IPAddrToHWAddr(lrpJoinIP).String(), nil
}

func GenerateGatewayIPv6RouterLLA(node *corev1.Node, networkName string) (string, error) {
	joinAddresses, err := util.ParseNodeGatewayRouterJoinAddrs(node, networkName)
	if err != nil {
		return "", err
	}
	if len(joinAddresses) == 0 {
		return "", fmt.Errorf("missing join addresses at node %q for network %q", node.Name, networkName)
	}
	return util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(joinAddresses[0].IP)).String(), nil
}
