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
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"

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
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	ndmmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EVPN node controller", func() {
	const (
		nodeName = "node1"
		vtepName = "vtep1"
	)

	Describe("reconcile", func() {
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
				nadVTEPInfo:  make(map[string]string),
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
				nadVTEPInfo:  make(map[string]string),
				stopChan:     make(chan struct{}),
			}

			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", GetEVPNBridgeName(vtepName)).Return(nil)

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
				nadVTEPInfo:  make(map[string]string),
				stopChan:     make(chan struct{}),
			}

			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", GetEVPNBridgeName(vtepName)).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
		})
	})

	Describe("onNodeUpdate", func() {
		var (
			kubeClient *fake.Clientset
			vtepClient *vtepfake.Clientset
			wf         *factory.WatchFactory
			ndm        *ndmmocks.Interface
			fakeNM     *networkmanager.FakeNetworkManager
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
			ctrl, err = NewController(nodeName, wf, &kube.Kube{KClient: kubeClient}, ndm, fakeNM)
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

		It("reconciles VTEP with VID/VNI mappings when a network is added or removed", func() {
			const nadKey = "test-ns/test-nad"

			By("allowing any NDM calls throughout the test")
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil)

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
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}
			fakeNM.TriggerHandlers(nadKey, netInfo, false)

			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			bridgeName := GetEVPNBridgeName(vtepName)

			By("verifying reconcile created devices with correct VID/VNI mappings on the VXLAN")
			Eventually(func() []netlinkdevicemanager.VIDVNIMapping {
				for _, call := range ndm.Calls {
					if call.Method == "EnsureLink" {
						cfg := call.Arguments.Get(0).(netlinkdevicemanager.DeviceConfig)
						if cfg.Link.Attrs().Name == vxlan4Name {
							return cfg.VIDVNIMappings
						}
					}
				}
				return nil
			}).Should(ConsistOf(
				netlinkdevicemanager.VIDVNIMapping{VID: 100, VNI: 10100},
				netlinkdevicemanager.VIDVNIMapping{VID: 200, VNI: 10200},
			))

			By("verifying bridge was also created")
			var ensured []string
			for _, call := range ndm.Calls {
				if call.Method == "EnsureLink" {
					cfg := call.Arguments.Get(0).(netlinkdevicemanager.DeviceConfig)
					ensured = append(ensured, cfg.Link.Attrs().Name)
				}
			}
			Expect(ensured).To(ContainElement(bridgeName))

			By("simulating a network removal: clear NADNetworks and trigger handlers")
			ndm.Calls = nil
			fakeNM.NADNetworks = nil
			fakeNM.PrimaryNetworks = nil
			fakeNM.TriggerHandlers(nadKey, nil, true)

			By("verifying reconcile was triggered again and VXLAN has no mappings")
			Eventually(func() bool {
				for _, call := range ndm.Calls {
					if call.Method == "EnsureLink" {
						cfg := call.Arguments.Get(0).(netlinkdevicemanager.DeviceConfig)
						if cfg.Link.Attrs().Name == vxlan4Name {
							return len(cfg.VIDVNIMappings) == 0
						}
					}
				}
				return false
			}).Should(BeTrue())
		})
	})
})
