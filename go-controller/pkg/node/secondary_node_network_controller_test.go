package node

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SecondaryNodeNetworkController", func() {
	var (
		networkID = "3"
		nad       = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fexec      *ovntest.FakeExec
		mgtPortMAC string = "00:00:00:55:66:77" // dummy MAC used for fake commands
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
		ovntest.AnnotateNADWithNetworkID(networkID, nad)
	})
	AfterEach(func() {
		util.ResetRunner()
	})

	It("ensure UDNGateway is not invoked when feature gate is OFF", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller, err := NewSecondaryNodeNetworkController(&cnnci, NetInfo, nil, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
	It("ensure UDNGateway is invoked for Primary UDNs when feature gate is ON", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister := v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeCommands(fexec, "ovn-k8s-mp3", mgtPortMAC, NetInfo.GetNetworkName(), "worker1", NetInfo.MTU())
		controller, err := NewSecondaryNodeNetworkController(&cnnci, NetInfo, nil, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).To(HaveOccurred()) // we don't have the gateway pieces setup so its expected to fail here
		Expect(err.Error()).To(ContainSubstring("could not create management port"), err.Error())
		Expect(controller.gateway).To(Not(BeNil()))
	})
	It("ensure UDNGateway is not invoked for Primary UDNs when feature gate is ON but network is not Primary", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller, err := NewSecondaryNodeNetworkController(&cnnci, NetInfo, nil, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
})

var _ = Describe("SecondaryNodeNetworkController: UserDefinedPrimaryNetwork Gateway functionality", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netName                 = "bluenet"
		netID                   = 3
		nodeName         string = "worker1"
		mgtPortMAC       string = "00:00:00:55:66:77"
		fexec            *ovntest.FakeExec
		testNS           ns.NetNS
		vrf              *vrfmanager.Controller
		ipRulesManager   *iprulemanager.Controller
		v4NodeSubnet     = "10.128.0.0/24"
		v6NodeSubnet     = "ae70::66/112"
		mgtPort          = fmt.Sprintf("%s%d", types.K8sMgmtIntfNamePrefix, netID)
		gatewayInterface = "eth0"
		gatewayBridge    = "breth0"
		stopCh           chan struct{}
		wg               *sync.WaitGroup
		kubeMock         kubemocks.Interface
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		kubeMock = kubemocks.Interface{}
		fexec = ovntest.NewFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ovntest.AddLink(gatewayInterface)
			link := ovntest.AddLink(gatewayBridge)
			ovntest.AddLink(mgtPort)
			addr, _ := netlink.ParseAddr("169.254.169.2/29")
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr("10.0.0.5/24")
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		routeManager := routemanager.NewController()
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				routeManager.Run(stopCh, 2*time.Minute)
				return nil
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
		vrf = vrfmanager.NewController(routeManager)
		wg2 := &sync.WaitGroup{}
		defer func() {
			wg2.Wait()
		}()
		wg2.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg2.Done()
			err := testNS.Do(func(ns.NetNS) error {
				return vrf.Run(stopCh, wg)
			})
			Expect(err).NotTo(HaveOccurred())
		}()
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
		util.ResetRunner()
	})

	ovntest.OnSupportedPlatformsIt("ensure UDNGateway and VRFManager and IPRulesManager are invoked for Primary UDNs when feature gate is ON", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Gateway.NextHop = "10.0.0.11"
		config.Gateway.Interface = gatewayInterface
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
		config.IPv6Mode = true
		config.IPv4Mode = true

		By("creating necessary mocks")
		factoryMock := factoryMocks.NodeWatchFactory{}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%d\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet)},
			},
		}
		nodeList := []*corev1.Node{node}
		factoryMock.On("GetNode", nodeName).Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister := v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)

		By("creating NAD for primary UDN")
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(strconv.Itoa(netID), nad)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		By("creating secondary network controller for user defined primary network")
		cnnci := CommonNodeNetworkControllerInfo{name: nodeName, watchFactory: &factoryMock}
		controller, err := NewSecondaryNodeNetworkController(&cnnci, NetInfo, vrf, ipRulesManager, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(Not(BeNil()))
		Expect(controller.gateway.ruleManager).To(Not(BeNil()))
		controller.gateway.kubeInterface = &kubeMock

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, NetInfo.MTU())
			getVRFCreationFakeOVSCommands(fexec)
			getRPFilterLooseModeFakeCommands(fexec)
			getDeletionFakeOVSCommands(fexec, mgtPort)

			By("starting secondary network controller for user defined primary network")
			err = controller.Start(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("check management interface and VRF device is created for the network")
			vrfDeviceName := util.GetNetworkVRFName(NetInfo)
			vrfLink, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(vrfLink.Type()).To(Equal("vrf"))
			vrfDev, ok := vrfLink.(*netlink.Vrf)
			Expect(ok).To(BeTrue())
			mplink, err := util.GetNetLinkOps().LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			Expect(vrfDev.Table).To(Equal(uint32(vrfTableId)))

			By("delete VRF device explicitly and ensure VRF Manager reconciles it")
			err = util.GetNetLinkOps().LinkDelete(vrfLink)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
				return err
			}).WithTimeout(120 * time.Second).Should(Succeed())

			By("check iprules are created for the network")
			rulesFound, err := netlink.RuleList(netlink.FAMILY_ALL)
			Expect(err).NotTo(HaveOccurred())
			var udnRules []netlink.Rule
			for _, rule := range rulesFound {
				if rule.Priority == UDNMasqueradeIPRulePriority {
					udnRules = append(udnRules, rule)
				}
			}
			Expect(udnRules).To(HaveLen(3))

			By("delete the network and ensure its associated VRF device is also deleted")
			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
			err = controller.Cleanup()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
				return err
			}).WithTimeout(120 * time.Second).ShouldNot(Succeed())

			By("check masquerade iprules are deleted for the network")
			rulesFound, err = netlink.RuleList(netlink.FAMILY_ALL)
			Expect(err).NotTo(HaveOccurred())
			udnRules = []netlink.Rule{} // reset
			for _, rule := range rulesFound {
				if rule.Priority == UDNMasqueradeIPRulePriority {
					udnRules = append(udnRules, rule)
				}
			}
			Expect(udnRules).To(BeEmpty())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})
