package bridgeconfig

import (
	"fmt"
	utilnet "k8s.io/utils/net"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDefaultBridgeConfig() *BridgeConfiguration {
	defaultNetConfig := &BridgeUDNConfiguration{
		OfPortPatch: "patch-breth0_ov",
	}
	return &BridgeConfiguration{
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
	}
}

func TestBridgeConfig(brName string) *BridgeConfiguration {
	return &BridgeConfiguration{
		BridgeName: brName,
	}
}

func (bridge *BridgeConfiguration) GetNetConfigLen() int {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return len(bridge.netConfig)
}

func CheckUDNSvcIsolationOVSFlows(flows []string, netConfig *BridgeUDNConfiguration, netName, bridgeMAC string, svcCIDR *net.IPNet, expectedNFlows int) {
	By(fmt.Sprintf("Checking UDN %s service isolation flows for %s; expected %d flows",
		netName, svcCIDR.String(), expectedNFlows))

	var mgmtMasqIP string
	var protoPrefix string
	if utilnet.IsIPv4CIDR(svcCIDR) {
		mgmtMasqIP = netConfig.v4MasqIPs.ManagementPort.IP.String()
		protoPrefix = "ip"
	} else {
		mgmtMasqIP = netConfig.v6MasqIPs.ManagementPort.IP.String()
		protoPrefix = "ip6"
	}

	var nFlows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=200, table=2, %s, %s_src=%s, actions=set_field:%s->eth_dst,output:%s",
			protoPrefix, protoPrefix, mgmtMasqIP, bridgeMAC, netConfig.OfPortPatch)) {
			nFlows++
		}
	}

	Expect(nFlows).To(Equal(expectedNFlows))
}

func CheckAdvertisedUDNSvcIsolationOVSFlows(flows []string, netConfig *BridgeUDNConfiguration, netName, bridgeMAC string, svcCIDR *net.IPNet, expectedNFlows int) {
	By(fmt.Sprintf("Checking advertsised UDN %s service isolation flows for %s; expected %d flows",
		netName, svcCIDR.String(), expectedNFlows))

	var matchingIPFamilySubnet *net.IPNet
	var protoPrefix string
	var udnAdvertisedSubnets []*net.IPNet
	var err error
	for _, clusterEntry := range netConfig.Subnets {
		udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
	}
	if utilnet.IsIPv4CIDR(svcCIDR) {
		matchingIPFamilySubnet, err = util.MatchFirstIPNetFamily(false, udnAdvertisedSubnets)
		Expect(err).ToNot(HaveOccurred())
		protoPrefix = "ip"
	} else {
		matchingIPFamilySubnet, err = util.MatchFirstIPNetFamily(false, udnAdvertisedSubnets)
		Expect(err).ToNot(HaveOccurred())
		protoPrefix = "ip6"
	}

	var nFlows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=200, table=2, %s, %s_src=%s, actions=set_field:%s->eth_dst,output:%s",
			protoPrefix, protoPrefix, matchingIPFamilySubnet, bridgeMAC, netConfig.OfPortPatch)) {
			nFlows++
		}
		if strings.Contains(flow, fmt.Sprintf("priority=550, in_port=LOCAL, %s, %s_src=%s, %s_dst=%s, actions=ct(commit,zone=64001,table=2)",
			protoPrefix, protoPrefix, matchingIPFamilySubnet, protoPrefix, svcCIDR)) {
			nFlows++
		}
	}

	Expect(nFlows).To(Equal(expectedNFlows))
}

func CheckDefaultSvcIsolationOVSFlows(flows []string, defaultConfig *BridgeUDNConfiguration, ofPortHost, bridgeMAC string, svcCIDR *net.IPNet) {
	By(fmt.Sprintf("Checking default service isolation flows for %s", svcCIDR.String()))

	var masqIP string
	var masqSubnet string
	var protoPrefix string
	if utilnet.IsIPv4CIDR(svcCIDR) {
		protoPrefix = "ip"
		masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V4MasqueradeSubnet
	} else {
		protoPrefix = "ip6"
		masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V6MasqueradeSubnet
	}

	var nTable0DefaultFlows int
	var nTable0UDNMasqFlows int
	var nTable2Flows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=500, in_port=%s, %s, %s_dst=%s, actions=ct(commit,zone=%d,nat(src=%s),table=2)",
			ofPortHost, protoPrefix, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone,
			masqIP)) {
			nTable0DefaultFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, actions=ct(commit,zone=%d,table=2)",
			ofPortHost, protoPrefix, protoPrefix, masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone)) {
			nTable0UDNMasqFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=100, table=2, actions=set_field:%s->eth_dst,output:%s",
			bridgeMAC, defaultConfig.OfPortPatch)) {
			nTable2Flows++
		}
	}

	Expect(nTable0DefaultFlows).To(Equal(1))
	Expect(nTable0UDNMasqFlows).To(Equal(1))
	Expect(nTable2Flows).To(Equal(1))
}
