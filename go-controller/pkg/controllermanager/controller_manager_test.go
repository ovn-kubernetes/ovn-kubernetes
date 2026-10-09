// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ignoreAlreadyRegisteredRegisterer allows ControllerManager.Start to be invoked
// more than once in a single test process. Production metrics registration uses
// prometheus.MustRegister and assumes a single leadership term per process.
type ignoreAlreadyRegisteredRegisterer struct {
	prometheus.Registerer
}

func (r ignoreAlreadyRegisteredRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

func testNode(nodeName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-subnets":    `{"default":["10.128.0.0/23"]}`,
				"k8s.ovn.org/node-chassis-id": "chassis-id",
				util.OvnNodeID:                "1",
				util.OvnNodeZoneName:          nodeName,
				util.Layer2TopologyVersion:    util.TransitRouterTopoVersion,
				"k8s.ovn.org/network-ids":     `{"default":"0"}`,
				"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"shared","chassis-id":"chassis-id","ip-addresses":["10.1.1.2/24"],` +
					`"mac-address":"00:00:00:55:66:77","next-hops":["10.1.1.1"],"node-port-enable":"true"}}`,
				"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":"100.64.0.3/16"}`,
				util.OVNNodeHostCIDRs:                         `["10.1.1.2/24"]`,
				util.OVNNodeEncapIPs:                          `["10.1.1.2"]`,
				util.OvnTransitSwitchPortAddr:                 `{"ipv4":"100.88.0.2/16"}`,
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.1.1.2"}},
		},
	}
}

var _ = Describe("ControllerManager Start order", func() {
	const nodeName = "worker1"

	var (
		nbClient       libovsdbclient.Client
		sbClient       libovsdbclient.Client
		nbsbCleanup    *libovsdbtest.Context
		wf             *factory.WatchFactory
		wg             *sync.WaitGroup
		prevRegisterer prometheus.Registerer
		prevGatherer   prometheus.Gatherer
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		config.OVNKubernetesFeature.EnableEgressIP = false
		config.OVNKubernetesFeature.EnableObservability = false
		config.OVNKubernetesFeature.EnableRouteAdvertisements = false
		config.OVNKubernetesFeature.EnableServiceTemplateSupport = false
		config.EnableMulticast = false

		// Allow configureMetrics to run for every Start() in this Describe.
		reg := prometheus.NewRegistry()
		prevRegisterer = prometheus.DefaultRegisterer
		prevGatherer = prometheus.DefaultGatherer
		prometheus.DefaultRegisterer = ignoreAlreadyRegisteredRegisterer{reg}
		prometheus.DefaultGatherer = reg

		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --columns=_uuid list Chassis_Template_Var",
			Err: fmt.Errorf("not supported"),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			Output: "uuid",
		})
		Expect(util.SetExec(fexec)).To(Succeed())

		var err error
		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.NBGlobal{UUID: "nbglobal-UUID", Options: map[string]string{}},
				&nbdb.LogicalRouter{Name: types.OVNClusterRouter},
				&nbdb.LogicalSwitch{Name: types.OVNJoinSwitch},
				&nbdb.LoadBalancerGroup{Name: types.ClusterLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterSwitchLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterRouterLBGroupName},
			},
			SBData: []libovsdbtest.TestData{
				&sbdb.SBGlobal{UUID: "sbglobal-UUID", Options: map[string]string{}},
			},
		}
		nbClient, sbClient, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		Expect(err).NotTo(HaveOccurred())

		wg = &sync.WaitGroup{}
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		if nbsbCleanup != nil {
			nbsbCleanup.Cleanup()
			nbsbCleanup = nil
		}
		util.ResetRunner()
		if prevRegisterer != nil {
			prometheus.DefaultRegisterer = prevRegisterer
			prometheus.DefaultGatherer = prevGatherer
		}
	})

	newCM := func() *ControllerManager {
		ovnClient := util.GetOVNClientset(testNode(nodeName))
		var err error
		wf, err = factory.NewOVNKubeControllerWatchFactory(ovnClient.GetOVNKubeControllerClientset(), nodeName)
		Expect(err).NotTo(HaveOccurred())

		cm, err := NewControllerManager(nodeName, ovnClient, wf, nbClient, sbClient, record.NewFakeRecorder(10), wg)
		Expect(err).NotTo(HaveOccurred())
		return cm
	}

	It("starts the default network controller before network manager", func() {
		cm := newCM()

		releaseNM := make(chan struct{})
		nmEntered := make(chan struct{})
		var nodeSwitchErr error
		nm := &networkmanager.FakeNetworkManager{
			StartFunc: func() error {
				// DNC.Start must have programmed the local node switch before NAD/UDN sync begins.
				_, nodeSwitchErr = libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: nodeName})
				close(nmEntered)
				<-releaseNM
				return nil
			},
		}
		cm.networkManager = nm
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseNM) }) }
		DeferCleanup(func() {
			release()
			cm.Stop()
		})

		errCh := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			errCh <- cm.Start(context.Background())
		}()

		Eventually(nmEntered, 60*time.Second).Should(BeClosed())
		Expect(nodeSwitchErr).NotTo(HaveOccurred(), "default network node logical switch must exist before network manager Start")
		Expect(cm.defaultNetworkController).NotTo(BeNil())
		Expect(nm.Started()).To(BeFalse())
		Consistently(errCh, 50*time.Millisecond).ShouldNot(Receive())

		release()
		Eventually(errCh, 30*time.Second).Should(Receive(BeNil()))
		Expect(nm.Started()).To(BeTrue())
	})

	It("does not start network manager when default network controller start fails", func() {
		cm := newCM()

		// Fail after initDefaultNetworkController succeeds so this asserts the
		// Start-before-NM order (an init-only failure would return earlier and
		// still pass if networkManager.Start were moved before DNC.Start).
		origStart := startDefaultNetworkController
		DeferCleanup(func() { startDefaultNetworkController = origStart })
		startDefaultNetworkController = func(context.Context, networkmanager.BaseNetworkController) error {
			return fmt.Errorf("injected start failure")
		}

		nmStarted := false
		nm := &networkmanager.FakeNetworkManager{
			StartFunc: func() error {
				nmStarted = true
				return nil
			},
		}
		cm.networkManager = nm
		DeferCleanup(cm.Stop)

		err := cm.Start(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("failed to start default network controller: injected start failure"))
		Expect(nmStarted).To(BeFalse())
		Expect(nm.Started()).To(BeFalse())
	})

	It("does not remove UDN EgressIP NBDB entries when DNC starts", func() {
		// Pre-existing UDN EgressIP OVN config must survive DefaultNetworkController.Start.
		// NAD reconcile and pods on those NADs have not run yet at that point, so DNC
		// must not treat the LRPs as stale. Only StartEgressIP (after networkManager.Start)
		// performs EgressIP stale cleanup via syncEgressIPs.
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = 1234

		const (
			udnNamespace = "udn-ns"
			nadName      = "nad1"
			networkName  = "network1"
			podName      = "egress-pod"
			egressIPName = "egressip"
			podIP        = "20.128.0.5"
			udnSubnet    = "20.128.0.0/16"
			eipMark      = 50000
		)
		nadKey := util.GetNADName(udnNamespace, nadName)
		egressLabel := map[string]string{"egress": "needed"}
		findUDNEgressIPLRP := func() ([]*nbdb.LogicalRouterPolicy, error) {
			return libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == types.EgressIPReroutePriority &&
					item.Match == fmt.Sprintf("ip4.src == %s", podIP) &&
					item.ExternalIDs[libovsdbops.NetworkKey.String()] == networkName &&
					item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == fmt.Sprintf("%s_%s/%s", egressIPName, udnNamespace, podName)
			})
		}

		netconf := ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: networkName,
				Type: "ovn-k8s-cni-overlay",
			},
			Role:     types.NetworkRolePrimary,
			Topology: types.Layer3Topology,
			NADName:  nadKey,
			Subnets:  "20.128.0.0/14",
		}
		netconfBytes, err := json.Marshal(netconf)
		Expect(err).NotTo(HaveOccurred())
		nad := &nadv1.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nadName,
				Namespace: udnNamespace,
				Annotations: map[string]string{
					types.OvnNetworkIDAnnotation: "2",
				},
			},
			Spec: nadv1.NetworkAttachmentDefinitionSpec{Config: string(netconfBytes)},
		}
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.AddNADs(nadKey)

		node := testNode(nodeName)
		node.Labels = map[string]string{"k8s.ovn.org/egress-assignable": ""}
		node.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(
			`{"default":["10.128.0.0/23"],"%s":"%s"}`, networkName, udnSubnet)
		node.Annotations["k8s.ovn.org/network-ids"] = fmt.Sprintf(
			`{"default":"0","%s":"2"}`, networkName)
		node.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"10.1.1.2/24","ipv6":""}`

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: udnNamespace,
				Labels: map[string]string{
					types.RequiredUDNNamespaceLabel: "",
					"egress":                        "needed",
				},
			},
		}
		pod := ovntest.NewPodWithLabels(udnNamespace, podName, nodeName, podIP, egressLabel)
		podIPNet := ovntest.MustParseIPNet(podIP + util.GetIPFullMaskString(podIP))
		hwAddr, err := net.ParseMAC("00:00:5e:00:53:01")
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
			IPs:  []*net.IPNet{podIPNet},
			MAC:  hwAddr,
			Role: types.NetworkRolePrimary,
		}, nadKey)
		Expect(err).NotTo(HaveOccurred())

		eip := &egressipv1.EgressIP{
			ObjectMeta: metav1.ObjectMeta{
				Name: egressIPName,
				Annotations: map[string]string{
					util.EgressIPMarkAnnotation: fmt.Sprintf("%d", eipMark),
				},
			},
			Spec: egressipv1.EgressIPSpec{
				EgressIPs: []string{"192.168.126.101"},
				PodSelector: metav1.LabelSelector{
					MatchLabels: egressLabel,
				},
				NamespaceSelector: metav1.LabelSelector{
					MatchLabels: egressLabel,
				},
			},
			Status: egressipv1.EgressIPStatus{
				Items: []egressipv1.EgressIPStatusItem{{
					Node:     nodeName,
					EgressIP: "192.168.126.101",
				}},
			},
		}

		// Rebuild NBDB with a pre-existing UDN EgressIP reroute LRP (restart state).
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		nbsbCleanup.Cleanup()
		udnRouterName := mutableNetInfo.GetNetworkScopedClusterRouterName()
		udnJoinIP := "100.65.0.2"
		udnJoinLRPUUID := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + networkName + "_" + nodeName + "-UUID"
		udnJoinLRPName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + networkName + "_" + nodeName
		_, udnNodeSubnet, err := net.ParseCIDR(udnSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgmtIP := util.GetNodeManagementIfAddr(udnNodeSubnet).IP.String()
		lrpUUID := "udn-eip-reroute-UUID"
		lrpExternalIDs := libovsdbops.NewDbObjectIDs(
			libovsdbops.LogicalRouterPolicyEgressIP,
			types.DefaultNetworkControllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", egressIPName, udnNamespace, podName),
				libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPReroutePriority),
				libovsdbops.IPFamilyKey:   "ip4",
				libovsdbops.NetworkKey:    networkName,
			},
		).GetExternalIDs()
		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.NBGlobal{UUID: "nbglobal-UUID", Options: map[string]string{}},
				&nbdb.LogicalRouter{Name: types.OVNClusterRouter},
				&nbdb.LogicalSwitch{Name: types.OVNJoinSwitch},
				&nbdb.LoadBalancerGroup{Name: types.ClusterLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterSwitchLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterRouterLBGroupName},
				&nbdb.LogicalRouterPolicy{
					UUID:        lrpUUID,
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", podIP),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    []string{udnJoinIP},
					ExternalIDs: lrpExternalIDs,
					Options:     map[string]string{"pkt_mark": fmt.Sprintf("%d", eipMark)},
				},
				&nbdb.LogicalRouter{
					UUID:        udnRouterName + "-UUID",
					Name:        udnRouterName,
					Policies:    []string{lrpUUID},
					ExternalIDs: map[string]string{types.NetworkExternalID: networkName, types.TopologyExternalID: types.Layer3Topology},
				},
				&nbdb.LogicalRouterPort{
					UUID:     udnJoinLRPUUID,
					Name:     udnJoinLRPName,
					Networks: []string{udnJoinIP + "/16"},
				},
				&nbdb.LogicalRouter{
					UUID:        mutableNetInfo.GetNetworkScopedGWRouterName(nodeName) + "-UUID",
					Name:        mutableNetInfo.GetNetworkScopedGWRouterName(nodeName),
					Ports:       []string{udnJoinLRPUUID},
					ExternalIDs: map[string]string{types.NetworkExternalID: networkName, types.TopologyExternalID: types.Layer3Topology},
				},
				&nbdb.LogicalSwitchPort{
					UUID:      "k8s-" + networkName + "_" + nodeName + "-UUID",
					Name:      "k8s-" + networkName + "_" + nodeName,
					Addresses: []string{"fe:1a:b2:3f:0e:fb " + mgmtIP},
				},
				&nbdb.LogicalSwitch{
					UUID:        mutableNetInfo.GetNetworkScopedSwitchName(nodeName) + "-UUID",
					Name:        mutableNetInfo.GetNetworkScopedSwitchName(nodeName),
					Ports:       []string{"k8s-" + networkName + "_" + nodeName + "-UUID"},
					ExternalIDs: util.GenerateExternalIDsForSwitchOrRouter(mutableNetInfo),
				},
			},
			SBData: []libovsdbtest.TestData{
				&sbdb.SBGlobal{UUID: "sbglobal-UUID", Options: map[string]string{}},
			},
		}
		nbClient, sbClient, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		Expect(err).NotTo(HaveOccurred())

		ovnClient := util.GetOVNClientset(node, ns, pod, nad, eip)
		wf, err = factory.NewOVNKubeControllerWatchFactory(ovnClient.GetOVNKubeControllerClientset(), nodeName)
		Expect(err).NotTo(HaveOccurred())

		cm, err := NewControllerManager(nodeName, ovnClient, wf, nbClient, sbClient, record.NewFakeRecorder(10), wg)
		Expect(err).NotTo(HaveOccurred())

		existingLRPs, err := findUDNEgressIPLRP()
		Expect(err).NotTo(HaveOccurred())
		Expect(existingLRPs).NotTo(BeEmpty())
		existingLRPUUIDs := make([]string, 0, len(existingLRPs))
		for _, lrp := range existingLRPs {
			existingLRPUUIDs = append(existingLRPUUIDs, lrp.UUID)
		}

		dncFinishedWithoutRemovingLRP := false
		origStart := startDefaultNetworkController
		DeferCleanup(func() { startDefaultNetworkController = origStart })
		startDefaultNetworkController = func(ctx context.Context, c networkmanager.BaseNetworkController) error {
			if err := origStart(ctx, c); err != nil {
				return err
			}
			// DNC.Start must leave UDN EgressIP config alone: NAD reconcile and
			// pods on that NAD have not been reconciled yet. Stale cleanup belongs
			// to StartEgressIP after networkManager.Start.
			lrps, getErr := findUDNEgressIPLRP()
			Expect(getErr).NotTo(HaveOccurred())
			lrpUUIDs := make([]string, 0, len(lrps))
			for _, lrp := range lrps {
				lrpUUIDs = append(lrpUUIDs, lrp.UUID)
			}
			Expect(lrpUUIDs).To(ConsistOf(existingLRPUUIDs),
				"DNC.Start must not remove or replace UDN EgressIP NBDB entries before StartEgressIP")
			dncFinishedWithoutRemovingLRP = true
			return nil
		}

		nm := &networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{
				udnNamespace: mutableNetInfo,
			},
			NADNetworks: map[string]util.NetInfo{
				nadKey: mutableNetInfo,
			},
			StartFunc: func() error {
				Expect(dncFinishedWithoutRemovingLRP).To(BeTrue(),
					"network manager must start only after DNC.Start")
				// NAD/UDN sync programs local UDN pods into the shared port cache
				// before StartEgressIP runs syncEgressIPs.
				cm.portCache.Add(pod, mutableNetInfo.GetNetworkScopedSwitchName(nodeName), nadKey, "lsp-uuid", hwAddr, []*net.IPNet{podIPNet})
				return nil
			},
		}
		cm.networkManager = nm
		DeferCleanup(cm.Stop)

		Expect(cm.Start(context.Background())).To(Succeed())
		Expect(dncFinishedWithoutRemovingLRP).To(BeTrue())
		Expect(nm.Started()).To(BeTrue())

		// After StartEgressIP, live entries for reconciled NAD pods remain.
		lrps, err := findUDNEgressIPLRP()
		Expect(err).NotTo(HaveOccurred())
		lrpUUIDs := make([]string, 0, len(lrps))
		for _, lrp := range lrps {
			lrpUUIDs = append(lrpUUIDs, lrp.UUID)
		}
		Expect(lrpUUIDs).To(ConsistOf(existingLRPUUIDs),
			"StartEgressIP must not remove live UDN EgressIP NBDB entries once NAD pods are reconciled")
	})
})
