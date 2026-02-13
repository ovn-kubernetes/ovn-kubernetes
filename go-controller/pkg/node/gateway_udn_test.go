package node

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	rafakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	coreinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func getCreationFakeCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15" +
			" -- --may-exist add-port br-int " + mgtPort +
			" -- set interface " + mgtPort +
			fmt.Sprintf(" mac=\"%s\"", mgtPortMAC) +
			" type=internal mtu_request=" + fmt.Sprintf("%d", mtu) +
			" external-ids:iface-id=" + types.K8sPrefix + netName + "_" + nodeName +
			fmt.Sprintf(" external-ids:%s=%s", types.NetworkExternalID, netName),
	})

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net/ipv4/conf/" + mgtPort + "/forwarding=1",
		Output: "net.ipv4.conf." + mgtPort + ".forwarding = 1",
	})
}

func getRPFilterLooseModeFakeCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net/ipv4/conf/ovn-k8s-mp3/rp_filter=2",
		Output: "net.ipv4.conf.ovn-k8s-mp3.rp_filter = 2",
	})
}

func getDeletionFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgtPort,
	})
}

func setManagementPortFakeCommands(fexec *ovntest.FakeExec, nodeName string) {
	// management port commands
	mpPortName := types.K8sMgmtIntfName
	mpPortRepName := types.K8sMgmtIntfName + "_0"
	mpPortLegacyName := types.K8sPrefix + nodeName
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortName,
		Output: "internal," + mpPortName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortRepName,
		Output: "internal," + mpPortRepName,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mpPortLegacyName + " -- --may-exist add-port br-int " + mpPortName + " -- set interface " + mpPortName + " mac=\"0a:58:64:80:00:02\" type=internal mtu_request=1400 external-ids:iface-id=" + mpPortLegacyName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net/ipv4/conf/ovn-k8s-mp0/forwarding=1",
		Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip route replace table 7 172.16.1.0/24 via 100.128.0.1 dev ovn-k8s-mp0",
		Output: "0",
	})
	if config.IPv6Mode {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 fd02::/112 via ae70::1 dev ovn-k8s-mp0",
			Output: "0",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net/ipv4/conf/ovn-k8s-mp0/rp_filter=2",
		Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
	})
}

func setUpGatewayFakeOVSCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br breth0",
		Output: "breth0",
	})
	// getIntfName
	// GetNicName
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 list-ports breth0",
		Output: "breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Port breth0 Interfaces",
		Output: "breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface breth0 Type",
		Output: "system",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
		Output: "00:00:00:55:66:99",
	})
	if config.IPv4Mode {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net/ipv4/conf/breth0/forwarding=1",
			Output: "net.ipv4.conf.breth0.forwarding = 1",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
		Output: "",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
		Output: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl dpif/show-dp-features breth0",
		Output: "Check pkt length action: Yes",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
		Output: "false",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl fdb/add breth0 breth0 0 00:00:00:55:66:99",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get interface breth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
		Output: "192.168.1.10",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})
}

func setUpUDNOpenflowManagerFakeOVSCommands(fexec *ovntest.FakeExec) {
	// UDN patch port
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "15",
	})
}

func setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec *ovntest.FakeExec) {
	// Default and UDN patch port
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "15",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})

	// After simulated deletion.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})
}

func deleteStaleManagementPortFakeCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns name find Interface external-ids:%s=%s", types.OvnManagementPortNameExternalID, mgtPort),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns name find Interface type=internal name=%s", mgtPort),
	})
}

func openflowManagerCheckPorts(ofMgr *openflowManager) {
	GinkgoHelper()
	netConfigs, uplink, ofPortPhys := ofMgr.getDefaultBridgePortConfigurations()
	sort.SliceStable(netConfigs, func(i, j int) bool {
		return netConfigs[i].PatchPort < netConfigs[j].PatchPort
	})
	Expect(checkPorts(netConfigs, uplink, ofPortPhys)).To(Succeed())
}

func getDummyOpenflowManager() *openflowManager {
	gwBridge := bridgeconfig.TestBridgeConfig("breth0")
	ofm := &openflowManager{
		defaultBridge: gwBridge,
	}
	return ofm
}

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName               = "bluenet"
		netID                 = "3"
		nodeName       string = "worker1"
		mgtPortMAC     string = "00:00:00:55:66:77" // dummy MAC used for fake commands
		fexec          *ovntest.FakeExec
		testNS         ns.NetNS
		factoryMock    factoryMocks.NodeWatchFactory
		kubeMock       kubemocks.Interface
		nodeLister     v1mocks.NodeLister
		vrf            *vrfmanager.Controller
		rm             *routemanager.Controller
		ipRulesManager *iprulemanager.Controller
		wg             sync.WaitGroup
		stopCh         chan struct{}
		v4NodeSubnet   = "100.128.0.0/24"
		v6NodeSubnet   = "ae70::/64"
		mgtPort        = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
		v4NodeIP       = "192.168.1.10/24"
		v6NodeIP       = "fc00:f853:ccd:e793::3/64"
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		Expect(err).NotTo(HaveOccurred())

		// Set dual-stack service CIDRs directly after PrepareTestConfig
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("172.16.1.0/24", "fd02::/112")

		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OvnKubeNode.MgmtPortDPResourceName = ""
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		// Setup mock filesystem for ovs-vswitchd.pid file needed by ovs-appctl commands
		Expect(util.SetupMockOVSPidFile()).To(Succeed())
		Expect(util.SetExec(fexec)).To(Succeed())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			// given the netdevice is created using add-port command in OVS
			// we need to mock create a dummy link for things to work in unit tests
			ovntest.AddLink(mgtPort)
			link := ovntest.AddLink("breth0")
			addr, _ := netlink.ParseAddr(v4NodeIP)
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr(v6NodeIP)
			return netlink.AddrAdd(link, addr)
		})
		Expect(err).NotTo(HaveOccurred())
		factoryMock = factoryMocks.NodeWatchFactory{}
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister = v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		kubeMock = kubemocks.Interface{}
		wg = sync.WaitGroup{}
		stopCh = make(chan struct{})
		rm = routemanager.NewController()
		vrf = vrfmanager.NewController(rm)
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				return vrf.Run(stopCh, &wg)
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		ipRulesManager = iprulemanager.NewController(true, true)
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				ipRulesManager.Run(stopCh, 4*time.Minute)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer().Lister(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeLister, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Create()
			Expect(err).NotTo(HaveOccurred())
			mpLink, err := util.LinkByName(util.GetNetworkScopedK8sMgmtHostIntfName(uint(udnGateway.GetNetworkID())))
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/24"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/64"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L3 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer().Lister(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeLister, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			Expect(udnGateway.mgmtPortController.Delete()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer().Lister(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeLister, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Create()
			Expect(err).NotTo(HaveOccurred())
			mpLink, err := util.LinkByName(util.GetNetworkScopedK8sMgmtHostIntfName(uint(udnGateway.GetNetworkID())))
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/16"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/60"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L2 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer().Lister(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeLister, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Delete()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		// need this for getGatewayNextHops
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
			Output: "breth0",
		})

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, ipRulesManager, localGw)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			Expect(udnGateway.AddNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(70))                                      // 18 UDN Flows are added by default
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(16))
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for table 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			// See setUpUDNOpenflowManagerCheckPortsFakeOVSCommands() for the order of ofPort query results.
			openflowManagerCheckPorts(udnGateway.openflowManager)

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))                                      // only default network flows are present
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for table 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should handle gateway delete idempotently for a L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		// need this for getGatewayNextHops
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
			Output: "breth0",
		})

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			By("injecting error into ipRulesManager to ensure everything else still cleans up")
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, &iprulemanager.FakeControllerWithError{}, localGw)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeLister, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network
			By("Deleting the gateway network with injected error")
			err = udnGateway.DelNetwork()
			Expect(err).To(MatchError(ContainSubstring("fake delete metadata error")))
			By("Ensuring everything else was still cleaned up correctly")
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))                                      // only default network flows are present
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			By("Ensure UDN management port was removed")
			_, err = netlink.LinkByName(mgtPort)
			Expect(err).To(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L2 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":   fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()

		// need this for getGatewayNextHops
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
			Output: "breth0",
		})

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wf.Shutdown()
			wg.Wait()
		}()
		err = wf.Start()

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		Expect(err).NotTo(HaveOccurred())

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			Expect(localGw.initFunc()).To(Succeed())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, ipRulesManager, localGw)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))
			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			Expect(udnGateway.AddNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(70))                                      // 18 UDN Flows are added by default
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(16))
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for tables 0 and 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			// See setUpUDNOpenflowManagerCheckPortsFakeOVSCommands() for the order of ofPort query results.
			openflowManagerCheckPorts(udnGateway.openflowManager)

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))                                      // only default network flows are present
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for tables 0 and 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	// TODO: There is opportunity to fold some of these tests into describetables to cut down code duplication and test plumbing
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for an advertised L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {netName}})
		// need this for getGatewayNextHops
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
			Output: "breth0",
		})

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, mutableNetInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:                kubeFakeClient,
			NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient:  udnfakeclient.NewSimpleClientset(),
			RouteAdvertisementsClient: rafakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(node, nodeSubnets, "", "", rm, mutableNetInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, ipRulesManager, localGw)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			Expect(udnGateway.AddNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(80))                                      // 18 UDN Flows, 5 advertisedUDN flows, and 2 packet mark flows (IPv4+IPv6) are added by default
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(18))
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly two flow per advertised UDN for table 2 and table 0 for service isolation.
				// but one of the flows used by advertised UDNs is already tracked and used by default UDNs hence not
				// counted here but in the check above for default svc isolation flows.
				bridgeconfig.CheckAdvertisedUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 2)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			// See setUpUDNOpenflowManagerCheckPortsFakeOVSCommands() for the order of ofPort query results.
			openflowManagerCheckPorts(udnGateway.openflowManager)

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(50))                                      // only default network flows are present
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for table 2 and table0 for service isolation.
				bridgeconfig.CheckAdvertisedUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct masquerade reply traffic routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, nil, nil, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))

			Expect(*routes[0].Dst).To(Equal(*ovntest.MustParseIPNet("172.16.1.0/24"))) // default service subnet
			Expect(routes[0].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[0].Gw).To(Equal(config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP))

			Expect(*routes[1].Dst).To(Equal(*ovntest.MustParseIPNet("fd02::/112"))) // default service subnet
			Expect(routes[1].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[1].Gw).To(Equal(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP))

			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[3].Dst).To(Equal(*cidr))
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, nil, nil, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			link, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())

			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))
			Expect(err).NotTo(HaveOccurred())
			// 1st and 2nd routes are the service routes from user-provided config value
			Expect(*routes[0].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[0]))
			Expect(routes[0].LinkIndex).To(Equal(link.Attrs().Index))
			Expect(*routes[1].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[1]))
			Expect(routes[1].LinkIndex).To(Equal(link.Attrs().Index))
			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[3].Dst).To(Equal(*cidr))
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv4 default unreachable route
			Expect(*routes[8].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0"))) // cluster subnet route
			Expect(routes[8].Priority).To(Equal(4278198272))
			Expect(routes[8].Type).To(Equal(unix.RTN_UNREACHABLE))

			// IPv6 default unreachable route
			Expect(*routes[9].Dst).To(Equal(*ovntest.MustParseIPNet("::/0"))) // cluster subnet route
			Expect(routes[9].Priority).To(Equal(4278198272))
			Expect(routes[9].Type).To(Equal(unix.RTN_UNREACHABLE))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should have default route when network is advertised on default VRF", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.NextHop = "10.0.0.11"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {types.DefaultNetworkName}})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, nil, nil, vrf, nil,
				&gateway{openflowManager: ofm, nextHops: ovntest.MustParseIPs(config.Gateway.NextHop)})
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(11))
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0")))
			Expect(routes[2].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[2].Gw.Equal(ovntest.MustParseIP(config.Gateway.NextHop))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should omit default route when network is advertised on any other vrf than default", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.NextHop = "10.0.0.11"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {netName}})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, nil, nil, vrf, nil, &gateway{openflowManager: ofm})
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[1].Dst).To(Not(Equal(*ovntest.MustParseIPNet("0.0.0.0/0"))))
			Expect(routes[1].Gw.Equal(ovntest.MustParseIP(config.Gateway.NextHop))).To(BeFalse())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should set rp filter to loose mode for management port interface", func() {
		getRPFilterLooseModeFakeCommands(fexec)
		err := testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err := util.SetRPFilterLooseModeForInterface(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should sync node port watcher successfully if a namespaces network is invalid", func() {
		// create new gateway, add ns with primary UDN, pod, expose pod via Node port service, delete pod, delete udn, ensure sync should succeeds
		namespace := util.NewNamespace("udn")
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		namespace.Labels[types.RequiredUDNNamespaceLabel] = ""
		service := newService("udn-svc", namespace.Name, "10.96.0.10", []corev1.ServicePort{{NodePort: int32(30300),
			Protocol: corev1.ProtocolTCP, Port: int32(8080)}}, corev1.ServiceTypeNodePort, []string{}, corev1.ServiceStatus{},
			true, false)
		fakeClient := util.GetOVNClientset(service, namespace)
		wf, err := factory.NewNodeWatchFactory(fakeClient.GetNodeClientset(), "node")
		Expect(err).ToNot(HaveOccurred(), "must get new node watch factory")
		Expect(wf.Start()).NotTo(HaveOccurred(), "must start Node watch factory")
		defer func() {
			wf.Shutdown()
		}()
		iptV4, iptV6 := util.SetFakeIPTablesHelpers()
		nodenft.SetFakeNFTablesHelper()
		fNPW := initFakeNodePortWatcher(iptV4, iptV6)
		fNPW.watchFactory = wf
		// in-order to simulate a namespace with an Invalid UDN (when GetActiveNamespace is called), we add an entry
		// to the fake network manager but no specified network. GetActiveNetwork will return the appropriate error of Invalid Network for namespace.
		// network manager may have a different implementation that fake network manager but both will return the same error.
		fNPW.networkManager = &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{namespace.Name: nil}}
		services := append([]interface{}{}, service)
		Expect(fNPW.SyncServices(services)).NotTo(HaveOccurred(), "must sync services")
	})
})

func TestConstructUDNVRFIPRules(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      net.IPNet
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:          "empty rules test",
			vrftableID:    1007,
			expectedRules: nil,
		},
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60/64"
			} else if config.IPv6Mode {
				cidr = "ae70::/60/64"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(HaveOccurred())
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, nil, nil, nil, nil, &gateway{openflowManager: ofm})
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst != nil {
					g.Expect(*rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(*rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestConstructUDNVRFIPRulesPodNetworkAdvertisedToDefaultVRF(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      net.IPNet
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60/64"
			} else if config.IPv6Mode {
				cidr = "ae70::/60/64"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {"bluenet"}})
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, nil, nil, nil, nil, &gateway{openflowManager: ofm})
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			udnGateway.isNetworkAdvertised = true
			udnGateway.isNetworkAdvertisedToDefaultVRF = true
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst != nil {
					g.Expect(*rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(*rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestConstructUDNVRFIPRulesPodNetworkAdvertisedToNonDefaultVRF(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      net.IPNet
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *ovntest.MustParseIPNet("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60"
			} else if config.IPv6Mode {
				cidr = "ae70::/60"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {"bluenet"}})
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, nil, nil, nil, nil, &gateway{openflowManager: ofm})
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			udnGateway.isNetworkAdvertised = true
			udnGateway.isNetworkAdvertisedToDefaultVRF = false
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rules).To(HaveLen(len(test.expectedRules)))
			g.Expect(delRules).To(HaveLen(len(test.deleteRules)))
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst != nil {
					g.Expect(*rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(*rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestUserDefinedNetworkGateway_updateAdvertisedUDNIsolationRules(t *testing.T) {
	tests := []struct {
		name                string
		nad                 *nadapi.NetworkAttachmentDefinition
		isNetworkAdvertised bool
		initialElements     []*knftables.Element
		expectedV4Elements  []*knftables.Element
		expectedV6Elements  []*knftables.Element
	}{
		{
			name: "Should add V4 and V6 entries to the set for advertised L3 network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should add V4 and V6 entries to the set for advertised L2 network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should not add duplicate V4 and V6 entries to the for advertised network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should remove V4 and V6 entries from the set when network for not advertised network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: false,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			nft := nodenft.SetFakeNFTablesHelper()
			config.IPv4Mode = true
			config.IPv6Mode = true

			netInfo, err := util.ParseNADInfo(tt.nad)
			g.Expect(err).NotTo(HaveOccurred())

			err = configureAdvertisedUDNIsolationNFTables()
			g.Expect(err).ToNot(HaveOccurred())
			tx := nft.NewTransaction()
			for _, element := range tt.initialElements {
				tx.Add(element)
			}
			if tx.NumOperations() > 0 {
				err = nft.Run(context.TODO(), tx)
				g.Expect(err).NotTo(HaveOccurred())
			}
			udng := &UserDefinedNetworkGateway{
				NetInfo: netInfo,
			}
			udng.isNetworkAdvertised = tt.isNetworkAdvertised
			err = udng.updateAdvertisedUDNIsolationRules()
			g.Expect(err).NotTo(HaveOccurred())

			v4Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v4Elems).To(HaveLen(len(tt.expectedV4Elements)))

			v6Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v6Elems).To(HaveLen(len(tt.expectedV6Elements)))

			for i, element := range tt.expectedV4Elements {
				g.Expect(element.Key).To(HaveLen(len(v4Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v4Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v4Elems[i].Comment))
			}
			for i, element := range tt.expectedV6Elements {
				g.Expect(element.Key).To(HaveLen(len(v6Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v6Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v6Elems[i].Comment))
			}
		})
	}
}
