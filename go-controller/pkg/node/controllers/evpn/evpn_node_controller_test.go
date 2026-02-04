package evpn

import (
	"context"
	"net"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedroutefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	routeadvertisementsfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	udnfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("EVPN node controller", func() {
	const (
		nodeName = "node1"
		vtepName = "vtep1"
	)

	var (
		wf     *factory.WatchFactory
		stopCh chan struct{}
		ctrl   *Controller

		kubeClient *fake.Clientset
		vtepClient *vtepfake.Clientset
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}

		kubeClient = fake.NewClientset(node)
		vtepClient = vtepfake.NewSimpleClientset()
		ovnNodeClient := &util.OVNNodeClientset{
			KubeClient:                kubeClient,
			EgressServiceClient:       egressservicefake.NewClientset(),
			EgressIPClient:            egressipfake.NewClientset(),
			AdminPolicyRouteClient:    adminpolicybasedroutefake.NewClientset(),
			NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient:  udnfake.NewClientset(),
			RouteAdvertisementsClient: routeadvertisementsfake.NewClientset(),
			VTEPClient:                vtepClient,
		}

		var err error
		wf, err = factory.NewNodeWatchFactory(ovnNodeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())

		stopCh = make(chan struct{})
		Expect(util.WaitForInformerCacheSyncWithTimeout(
			"evpn-controller",
			stopCh,
			wf.NodeCoreInformer().Informer().HasSynced,
			wf.VTEPInformer().Informer().HasSynced,
		)).To(BeTrue())

		ctrl, err = NewController(nodeName, wf, &kube.Kube{KClient: kubeClient})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if stopCh != nil {
			close(stopCh)
		}
		if wf != nil {
			wf.Shutdown()
		}
	})

	It("applies and cleans up device configs for a managed VTEP", func() {
		By("creating a managed VTEP with dual-stack CIDRs")
		_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{
				Name: vtepName,
			},
			Spec: vtepv1.VTEPSpec{
				CIDRs: vtepv1.DualStackCIDRs{
					"100.64.0.0/24",
					"fd00::/64",
				},
				Mode: vtepv1.VTEPModeManaged,
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("annotating the node with VTEP IPs")
		node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		node.Annotations = map[string]string{
			util.OVNNodeVTEPIPs: `{"vtep1":["100.64.0.1","fd00::1"]}`,
		}
		_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			_, err := wf.VTEPInformer().Lister().Get(vtepName)
			return err
		}).Should(Succeed())
		Eventually(func() bool {
			node, err := wf.GetNode(nodeName)
			if err != nil {
				return false
			}
			return node.Annotations[util.OVNNodeVTEPIPs] == `{"vtep1":["100.64.0.1","fd00::1"]}`
		}).Should(BeTrue())

		err = ctrl.reconcile(vtepName)
		Expect(err).NotTo(HaveOccurred())

		bridgeName := GetEVPNBridgeName(vtepName)
		vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
		vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)
		dummyName := GetEVPNDummyName(vtepName)

		By("verifying bridge, VXLANs, and dummy device configs are stored")
		bridgeCfg := ctrl.ndm.GetConfig(bridgeName)
		Expect(bridgeCfg).NotTo(BeNil())
		_, ok := bridgeCfg.Link.(*netlink.Bridge)
		Expect(ok).To(BeTrue())

		vxlan4Cfg := ctrl.ndm.GetConfig(vxlan4Name)
		Expect(vxlan4Cfg).NotTo(BeNil())
		Expect(vxlan4Cfg.Master).To(Equal(bridgeName))
		vxlan4Link, ok := vxlan4Cfg.Link.(*netlink.Vxlan)
		Expect(ok).To(BeTrue())
		Expect(vxlan4Link.SrcAddr.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())

		vxlan6Cfg := ctrl.ndm.GetConfig(vxlan6Name)
		Expect(vxlan6Cfg).NotTo(BeNil())
		Expect(vxlan6Cfg.Master).To(Equal(bridgeName))
		vxlan6Link, ok := vxlan6Cfg.Link.(*netlink.Vxlan)
		Expect(ok).To(BeTrue())
		Expect(vxlan6Link.SrcAddr.Equal(net.ParseIP("fd00::1"))).To(BeTrue())

		dummyCfg := ctrl.ndm.GetConfig(dummyName)
		Expect(dummyCfg).NotTo(BeNil())
		_, ok = dummyCfg.Link.(*netlink.Dummy)
		Expect(ok).To(BeTrue())
		Expect(dummyCfg.Addresses).To(HaveLen(2))
		addresses := make([]string, 0, len(dummyCfg.Addresses))
		for _, addr := range dummyCfg.Addresses {
			Expect(addr.IPNet).NotTo(BeNil())
			addresses = append(addresses, addr.IPNet.String())
		}
		Expect(addresses).To(ConsistOf("100.64.0.1/32", "fd00::1/128"))

		By("removing VTEP IPs from the node annotation")
		node, err = kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		delete(node.Annotations, util.OVNNodeVTEPIPs)
		_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			node, err := wf.GetNode(nodeName)
			if err != nil {
				return false
			}
			_, ok := node.Annotations[util.OVNNodeVTEPIPs]
			return !ok
		}).Should(BeTrue())

		err = ctrl.reconcile(vtepName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying device configs are cleaned up")
		Expect(ctrl.ndm.GetConfig(bridgeName)).To(BeNil())
		Expect(ctrl.ndm.GetConfig(vxlan4Name)).To(BeNil())
		Expect(ctrl.ndm.GetConfig(vxlan6Name)).To(BeNil())
		Expect(ctrl.ndm.GetConfig(dummyName)).To(BeNil())
	})
})
