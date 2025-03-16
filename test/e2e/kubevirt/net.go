package kubevirt

import (
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func GenerateAddressDiscoveryConfigurationCommand(iface string) string {
	return fmt.Sprintf(`
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
