package evpn

import (
	"context"
	"net"
	"sync"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedroutefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	routeadvertisementsfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	vtepinfmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1/mocks"
	vteplistmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	factorymocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	ndmmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager/mocks"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func newTestOVSClient() (libovsdbclient.Client, *libovsdbtest.Context) {
	bridgeUUID := "bridge-br-int-uuid"
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: ovsBridgeInt},
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return ovsClient, testCtx
}

var _ = Describe("EVPN node controller", func() {
	const (
		nodeName = "node1"
		vtepName = "vtep1"
	)

	Describe("reconcile", func() {
		var (
			ovsClient  libovsdbclient.Client
			ovsCleanup *libovsdbtest.Context
		)

		BeforeEach(func() {
			ovsClient, ovsCleanup = newTestOVSClient()
		})

		AfterEach(func() {
			if ovsCleanup != nil {
				ovsCleanup.Cleanup()
			}
		})

		It("creates bridge and VXLANs for an unmanaged dual-stack VTEP", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: vtepv1.DualStackCIDRs{"100.64.0.0/24", "fd00::/64"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: `["100.64.0.1/24","fd00::1/64"]`},
				},
			}

			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("GetNode", nodeName).Return(node, nil)
			wf.On("VTEPInformer").Return(informer)
			lister.On("Get", vtepName).Return(vtep, nil)

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				ndm:          ndm,
				networkMgr:   &networkmanager.FakeNetworkManager{},
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: make(map[string]sets.Set[string]),
				stopChan:     make(chan struct{}),
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == bridgeName
			})).Return(nil)
			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan4Name
			})).Return(nil).Run(func(args mock.Arguments) {
				cfg := args.Get(0).(netlinkdevicemanager.DeviceConfig)
				vxlan, isVxlan := cfg.Link.(*netlink.Vxlan)
				Expect(isVxlan).To(BeTrue())
				Expect(vxlan.SrcAddr.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
				Expect(cfg.Master).To(Equal(bridgeName))
			})
			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan6Name
			})).Return(nil).Run(func(args mock.Arguments) {
				cfg := args.Get(0).(netlinkdevicemanager.DeviceConfig)
				vxlan, isVxlan := cfg.Link.(*netlink.Vxlan)
				Expect(isVxlan).To(BeTrue())
				Expect(vxlan.SrcAddr.Equal(net.ParseIP("fd00::1"))).To(BeTrue())
				Expect(cfg.Master).To(Equal(bridgeName))
			})

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
		})

		It("cleans up devices for a managed VTEP", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: vtepv1.DualStackCIDRs{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeManaged,
				},
			}

			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			lister.On("Get", vtepName).Return(vtep, nil)

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				ndm:          ndm,
				networkMgr:   &networkmanager.FakeNetworkManager{},
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: make(map[string]sets.Set[string]),
				stopChan:     make(chan struct{}),
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", bridgeName).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
			ndm.AssertNotCalled(GinkgoT(), "EnsureLink", mock.Anything)
		})

		It("deletes all devices when VTEP is not found", func() {
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			lister.On("Get", vtepName).Return(nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "k8s.ovn.org", Resource: "vteps"}, vtepName))

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				ndm:          ndm,
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: make(map[string]sets.Set[string]),
				stopChan:     make(chan struct{}),
			}

			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", GetEVPNBridgeName(vtepName)).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
		})

		It("creates L3 and L2 SVIs with correct VLAN IDs and VRF master", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return(vtepName)
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("EVPNIPVRFVID").Return(200)
			netInfo.On("EVPNIPVRFVNI").Return(int32(10200))
			netInfo.On("GetNetworkName").Return("mynet")
			netInfo.On("GetNetworkID").Return(5)
			netInfo.On("GetNetworkScopedSwitchName", mock.Anything).Return("mynet_ovn_layer2_switch")

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{"test-ns": netInfo},
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			l3SVIName := GetEVPNL3SVIName(netInfo)
			l2SVIName := GetEVPNL2SVIName(netInfo)
			vrfName := util.GetNetworkVRFName(netInfo)

			ndm := &ndmmocks.Interface{}
			ndm.On("EnsureLink", mock.Anything).Return(nil)

			ctrl := &Controller{ndm: ndm, networkMgr: fakeNM, svisByBridge: make(map[string]sets.Set[string])}
			networks, err := ctrl.collectEVPNNetworks(vtepName)
			Expect(err).NotTo(HaveOccurred())

			Expect(ctrl.reconcileSVIs(bridgeName, networks)).To(Succeed())

			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				return ok && cfg.Link.Attrs().Name == l3SVIName && vlan.VlanId == 200 &&
					cfg.VLANParent == bridgeName && cfg.Master == vrfName
			}))
			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				return ok && cfg.Link.Attrs().Name == l2SVIName && vlan.VlanId == 100 &&
					cfg.VLANParent == bridgeName && cfg.Master == vrfName
			}))
		})

		It("removes stale SVIs that are no longer desired", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: vtepv1.DualStackCIDRs{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: `["100.64.0.1/24"]`},
				},
			}

			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("GetNode", nodeName).Return(node, nil)
			wf.On("VTEPInformer").Return(informer)
			lister.On("Get", vtepName).Return(vtep, nil)

			bridgeName := GetEVPNBridgeName(vtepName)
			staleSVI := "evsvi-old"

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				ndm:          ndm,
				networkMgr:   &networkmanager.FakeNetworkManager{},
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: map[string]sets.Set[string]{
					bridgeName: sets.New(staleSVI),
				},
				stopChan: make(chan struct{}),
			}

			ndm.On("EnsureLink", mock.Anything).Return(nil)
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "DeleteLink", staleSVI)
		})

		It("deletes SVIs parented to the bridge when VTEP is not found", func() {
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			lister.On("Get", vtepName).Return(nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "k8s.ovn.org", Resource: "vteps"}, vtepName))

			bridgeName := GetEVPNBridgeName(vtepName)
			sviName := "evsvi-net1"

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				ndm:          ndm,
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: map[string]sets.Set[string]{
					bridgeName: sets.New(sviName),
				},
				stopChan: make(chan struct{}),
			}

			ndm.On("DeleteLink", sviName).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", bridgeName).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "DeleteLink", sviName)
			ndm.AssertExpectations(GinkgoT())
		})

		It("cleans up stale OVS ports for a VTEP", func() {
			// Create a stale OVS port using the production helper
			stalePortName := "evovs-stale"
			Expect(libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, ovsBridgeInt, stalePortName,
				map[string]string{externalIDEVPNVTEP: vtepName}, nil)).To(Succeed())

			// Verify the port exists before cleanup
			_, err := getOVSPort(ovsClient, stalePortName)
			Expect(err).NotTo(HaveOccurred())

			ctrl := &Controller{ovsClient: ovsClient}

			By("calling reconcileOVSPorts with no desired ports")
			err = ctrl.reconcileOVSPorts(vtepName, GetEVPNBridgeName(vtepName), nil)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the stale OVS port was removed")
			_, err = getOVSPort(ovsClient, stalePortName)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("onNodeUpdate", func() {
		var (
			kubeClient *fake.Clientset
			vtepClient *vtepfake.Clientset
			wf         *factory.WatchFactory
			ndm        *ndmmocks.Interface
			fakeNM     *networkmanager.FakeNetworkManager
			ovsCleanup *libovsdbtest.Context
			ctrl       *Controller
		)

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEVPN = true
			config.Gateway.Mode = config.GatewayModeLocal

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeClient = fake.NewClientset(node)
			vtepClient = vtepfake.NewSimpleClientset()
			ndm = &ndmmocks.Interface{}

			var err error
			wf, err = factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient:                kubeClient,
				EgressServiceClient:       egressservicefake.NewClientset(),
				EgressIPClient:            egressipfake.NewClientset(),
				AdminPolicyRouteClient:    adminpolicybasedroutefake.NewClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewClientset(),
				RouteAdvertisementsClient: routeadvertisementsfake.NewClientset(),
				VTEPClient:                vtepClient,
			}, nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.Start()).To(Succeed())

			fakeNM = &networkmanager.FakeNetworkManager{}
			var ovsClient libovsdbclient.Client
			ovsClient, ovsCleanup = newTestOVSClient()
			ctrl, err = NewController(nodeName, wf, &kube.Kube{KClient: kubeClient}, ndm, fakeNM, ovsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ctrl.Start()).To(Succeed())
		})

		AfterEach(func() {
			if ctrl != nil {
				ctrl.Stop()
			}
			if wf != nil {
				wf.Shutdown()
			}
			if ovsCleanup != nil {
				ovsCleanup.Cleanup()
			}
		})

		It("reconciles VTEPs when node host-cidrs annotation changes", func() {
			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			// testify's mock.Calls is not safe to read concurrently, so we
			// collect EnsureLink configs via a Run callback under our own lock.
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			By("allowing any NDM calls throughout the test")
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and waiting for informer sync")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: vtepv1.DualStackCIDRs{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			By("annotating the node with host-cidrs to trigger onNodeUpdate → reconcile")
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			node.Annotations = map[string]string{
				util.OVNNodeHostCIDRs: `["100.64.0.1/24"]`,
			}
			_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the full chain: onNodeUpdate → reconcile → NDM creates bridge and VXLAN")
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var names []string
				for _, cfg := range ensuredCfgs {
					names = append(names, cfg.Link.Attrs().Name)
				}
				return names
			}).Should(ContainElements(bridgeName, vxlan4Name))

			By("verifying IPv6 VXLAN was deleted (no IPv6 in host-cidrs)")
			Expect(ndm.AssertCalled(GinkgoT(), "DeleteLink", vxlan6Name)).To(BeTrue())
		})

		It("creates SVIs with correct mappings when a network is added and removes them when removed", func() {
			const nadKey = "test-ns/test-nad"

			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			By("allowing any NDM calls throughout the test")
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and annotating the node with host-cidrs")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: vtepv1.DualStackCIDRs{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			node.Annotations = map[string]string{
				util.OVNNodeHostCIDRs: `["100.64.0.1/24"]`,
			}
			_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("simulating a network add with EVPN VID/VNI mappings")
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return(vtepName)
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("EVPNIPVRFVID").Return(200)
			netInfo.On("EVPNIPVRFVNI").Return(int32(10200))
			netInfo.On("GetNetworkName").Return("mynet")
			netInfo.On("GetNetworkID").Return(5)
			netInfo.On("GetNetworkScopedSwitchName", mock.Anything).Return("mynet_ovn_layer2_switch")
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}
			fakeNM.TriggerHandlers(nadKey, netInfo, false)

			l3SVIName := GetEVPNL3SVIName(netInfo)
			l2SVIName := GetEVPNL2SVIName(netInfo)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			bridgeName := GetEVPNBridgeName(vtepName)

			By("verifying VID/VNI mappings on the VXLAN device")
			Eventually(func() []netlinkdevicemanager.VIDVNIMapping {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name && len(cfg.VIDVNIMappings) > 0 {
						return cfg.VIDVNIMappings
					}
				}
				return nil
			}).Should(ConsistOf(
				netlinkdevicemanager.VIDVNIMapping{VID: 100, VNI: 10100},
				netlinkdevicemanager.VIDVNIMapping{VID: 200, VNI: 10200},
			))

			By("verifying L3 and L2 SVIs were created with correct VLAN IDs and bridge parent")
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var svis []string
				for _, cfg := range ensuredCfgs {
					if _, ok := cfg.Link.(*netlink.Vlan); ok {
						svis = append(svis, cfg.Link.Attrs().Name)
					}
				}
				return svis
			}).Should(ContainElements(l3SVIName, l2SVIName))

			ensuredMu.Lock()
			for _, cfg := range ensuredCfgs {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				if !ok {
					continue
				}
				switch cfg.Link.Attrs().Name {
				case l3SVIName:
					Expect(vlan.VlanId).To(Equal(200))
					Expect(cfg.VLANParent).To(Equal(bridgeName))
				case l2SVIName:
					Expect(vlan.VlanId).To(Equal(100))
					Expect(cfg.VLANParent).To(Equal(bridgeName))
				}
			}
			ensuredMu.Unlock()

			By("simulating network removal and verifying VXLAN has no mappings")
			ensuredMu.Lock()
			ensuredCfgs = nil
			ensuredMu.Unlock()
			fakeNM.NADNetworks = nil
			fakeNM.PrimaryNetworks = nil
			fakeNM.TriggerHandlers(nadKey, nil, true)

			Eventually(func() bool {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name {
						return len(cfg.VIDVNIMappings) == 0
					}
				}
				return false
			}).Should(BeTrue())

			By("verifying stale SVIs were deleted after network removal")
			Eventually(func() bool {
				return ndm.AssertCalled(GinkgoT(), "DeleteLink", l3SVIName) &&
					ndm.AssertCalled(GinkgoT(), "DeleteLink", l2SVIName)
			}).Should(BeTrue())
		})
	})
})

func getOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	port := &vswitchd.Port{Name: name}
	err := ovsClient.Get(ctx, port)
	return port, err
}
