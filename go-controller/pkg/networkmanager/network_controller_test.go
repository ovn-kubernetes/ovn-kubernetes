package networkmanager

import (
	"context"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSetAdvertisements(t *testing.T) {
	testZoneName := "testZone"
	testNodeName := "testNode"
	testNodeOnZoneName := "testNodeOnZone"
	testNADName := "test/NAD"
	testRAName := "testRA"
	testVRFName := "testVRF"

	defaultNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	primaryNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "primary",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: "layer3",
		Role:     "primary",
		MTU:      1400,
	}

	podNetworkRA := ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:    testVRFName,
			NodeSelector: metav1.LabelSelector{},
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
		},
		Status: ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	nonPodNetworkRA := ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:    testVRFName,
			NodeSelector: metav1.LabelSelector{},
		},
		Status: ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	podNetworkRANotAccepted := podNetworkRA
	podNetworkRANotAccepted.Status = ratypes.RouteAdvertisementsStatus{}
	podNetworkRARejected := *podNetworkRA.DeepCopy()
	podNetworkRARejected.Status.Conditions[0].Status = metav1.ConditionFalse
	podNetworkRAOutdated := podNetworkRA
	podNetworkRAOutdated.Generation = 1

	testNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
	}
	testNodeOnZone := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeOnZoneName,
			Annotations: map[string]string{
				util.OvnNodeZoneName: testZoneName,
			},
		},
	}
	otherNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "otherNode",
		},
	}

	tests := []struct {
		name            string
		network         *ovncnitypes.NetConf
		ra              *ratypes.RouteAdvertisements
		node            corev1.Node
		expectNoNetwork bool
		expected        map[string][]string
	}{
		{
			name:    "reconciles VRF advertisements for selected node of default node network controller",
			network: defaultNetwork,
			ra:      &podNetworkRA,
			node:    testNode,
			expected: map[string][]string{
				testNodeName: {testVRFName},
			},
		},
		{
			name:    "reconciles VRF advertisements for selected node in same zone as default OVN network controller",
			network: primaryNetwork,
			ra:      &podNetworkRA,
			node:    testNodeOnZone,
			expected: map[string][]string{
				testNodeOnZoneName: {testVRFName},
			},
		},
		{
			name:    "ignores advertisements that are not for the pod network",
			network: defaultNetwork,
			ra:      &nonPodNetworkRA,
			node:    testNode,
		},
		{
			name:    "ignores advertisements that are not for applicable node",
			network: defaultNetwork,
			ra:      &podNetworkRA,
			node:    otherNode,
		},
		{
			name:    "ignores advertisements that are not accepted",
			network: defaultNetwork,
			ra:      &podNetworkRANotAccepted,
			node:    testNode,
		},
		{
			name:            "fails for advertisements that are rejected",
			network:         primaryNetwork,
			ra:              &podNetworkRARejected,
			node:            testNode,
			expectNoNetwork: true,
		},
		{
			name:            "fails for advertisements that are old",
			network:         primaryNetwork,
			ra:              &podNetworkRAOutdated,
			node:            testNode,
			expectNoNetwork: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkController("", testZoneName, testNodeName, tcm, wf)

			namespace, name, err := cache.SplitMetaNamespaceKey(testNADName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadAnnotations := map[string]string{
				types.OvnRouteAdvertisementsKey: "[\"" + tt.ra.Name + "\"]",
			}
			nad, err := buildNADWithAnnotations(name, namespace, tt.network, nadAnnotations)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &tt.node, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()
			g.Expect(nm.Start()).To(gomega.Succeed())
			defer nm.Stop()

			netInfo, err := util.NewNetInfo(tt.network)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.AddNADs(testNADName)

			nm.getNADKeysForNetwork = func(networkName string) []string {
				if networkName == mutableNetInfo.GetNetworkName() {
					return []string{testNADName}
				}
				return nil
			}

			nm.EnsureNetwork(mutableNetInfo)

			meetsExpectations := func(g gomega.Gomega) {
				tcm.Lock()
				defer tcm.Unlock()
				var reconcilable ReconcilableNetworkController
				switch tt.network.Name {
				case types.DefaultNetworkName:
					reconcilable = tcm.GetDefaultNetworkController()
				default:
					reconcilable = tcm.controllers[testNetworkKey(netInfo)]
				}

				if tt.expectNoNetwork {
					g.Expect(reconcilable).To(gomega.BeNil())
					return
				}
				g.Expect(reconcilable).ToNot(gomega.BeNil())

				if tt.expected == nil {
					tt.expected = map[string][]string{}
				}
				g.Expect(reconcilable.GetPodNetworkAdvertisedVRFs()).To(gomega.Equal(tt.expected))
			}

			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			g.Consistently(meetsExpectations).Should(gomega.Succeed())
		})
	}
}

func TestNetworkControllerReconcilePendingNetworkRefChange(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tests := []struct {
		name           string
		nodeHasNetwork bool
	}{
		{
			name:           "active",
			nodeHasNetwork: true,
		},
		{
			name:           "inactive",
			nodeHasNetwork: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkController("", "", "", tcm, nil)
			nm.nodeHasNetwork = func(_, _ string) bool { return tt.nodeHasNetwork }

			networkName := netInfo.GetNetworkName()
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetNADs(netConf.NADName)
			nm.setNetwork(networkName, mutableNetInfo)

			var gotNode string
			var gotActive bool
			var callCount int
			testController := &testNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				tcm:                 tcm,
				handleRefChange: func(node string, active bool) {
					gotNode = node
					gotActive = active
					callCount++
				},
			}
			nm.networkControllers[networkName] = &networkControllerState{
				controller: testController,
			}

			nm.NotifyNetworkRefChange(networkName, "node1")
			err := nm.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(callCount).To(gomega.Equal(1))
			g.Expect(gotNode).To(gomega.Equal("node1"))
			g.Expect(gotActive).To(gomega.Equal(tt.nodeHasNetwork))

			nm.NotifyNetworkRefChange(networkName, "node1")
			err = nm.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(callCount).To(gomega.Equal(1))
		})
	}
}

func TestNetworkControllerClearsPendingNetworkRefOnDelete(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("", "", "", tcm, nil)
	nm.nodeHasNetwork = func(_, _ string) bool { return true }

	networkName := netInfo.GetNetworkName()
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	nm.setNetwork(networkName, mutableNetInfo)

	var callCount int
	testController := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
		handleRefChange: func(string, bool) {
			callCount++
		},
	}
	nm.networkControllers[networkName] = &networkControllerState{
		controller: testController,
	}

	nm.NotifyNetworkRefChange(networkName, "node1")
	err = nm.deleteNetwork(networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(callCount).To(gomega.Equal(0))

	var followupCalls int
	followupController := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
		handleRefChange: func(string, bool) {
			followupCalls++
		},
	}
	nm.networkControllers[networkName] = &networkControllerState{
		controller: followupController,
	}
	nm.setNetwork(networkName, mutableNetInfo)
	err = nm.syncNetwork(networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(followupCalls).To(gomega.Equal(0))
}
