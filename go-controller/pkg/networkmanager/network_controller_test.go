// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSetAdvertisements(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	t.Run("cluster manager leaves readiness to the nodes", func(t *testing.T) {
		g := gomega.NewWithT(t)
		wf, err := factory.NewClusterManagerWatchFactory(util.GetOVNClientset().GetClusterManagerClientset())
		g.Expect(err).NotTo(gomega.HaveOccurred(), "create cluster-manager watch factory")
		defer wf.Shutdown()
		nc := newNetworkController("cluster", "", &testControllerManager{}, wf, false)
		g.Expect(nc.legacyRAController).To(gomega.BeNil(), "cluster manager must not run the legacy RA watcher")
		g.Expect(nc.frrController).To(gomega.BeNil(), "FRR changes must not trigger cluster-wide network readiness checks")
		g.Expect(nc.nodeController).To(gomega.BeNil(), "node changes must not trigger cluster-wide network readiness checks")
	})

	testNodeName := "testNode"
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

	noOverlayNetwork := *primaryNetwork
	noOverlayNetwork.Transport = types.NetworkTransportNoOverlay
	evpnNetwork := *primaryNetwork
	evpnNetwork.Transport = types.NetworkTransportEVPN

	namedVRF := &util.RouteAdvertisementsIntent{TargetVRF: testVRFName}
	accepted := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: testRAName, UID: "original-ra", Generation: 1},
		Spec:       ratypes.RouteAdvertisementsSpec{TargetVRF: testVRFName, Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork}},
		Status: ratypes.RouteAdvertisementsStatus{Conditions: []metav1.Condition{
			{Type: ratypes.RouteAdvertisementsAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1},
		}},
	}
	egressOnly := accepted.DeepCopy()
	egressOnly.Spec.Advertisements = []ratypes.AdvertisementType{ratypes.EgressIP}
	migrated := accepted.DeepCopy()
	migrated.Status.Conditions = append(migrated.Status.Conditions, metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse})

	t.Run("keep the applied VRF while the first NAD intent is deferred", func(t *testing.T) {
		g := gomega.NewWithT(t)
		index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		nc := &networkController{legacyRALister: ralisters.NewRouteAdvertisementsLister(index)}
		ref := sets.New(testRAName)
		read := func(ra *ratypes.RouteAdvertisements, previous advertisementIntents) advertisementIntents {
			t.Helper()
			g.Expect(index.Update(ra.DeepCopy())).To(gomega.Succeed(), "observe the RA generation and conditions")
			intents, err := nc.getLegacyRAIntents(ref, previous)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "resolve the name-only NAD reference")
			return intents
		}
		applied := read(accepted, nil)
		g.Expect(applied).To(gomega.HaveKeyWithValue(testRAName, *util.NewRouteAdvertisementsIntent(accepted)), "start from the old controller's accepted VRF")

		ra := migrated.DeepCopy()
		ra.Generation++
		ra.Spec.TargetVRF = "red"
		ra.Status.Conditions[0].ObservedGeneration = ra.Generation
		degraded := &ra.Status.Conditions[1]
		degraded.Status, degraded.ObservedGeneration = metav1.ConditionTrue, ra.Generation
		g.Expect(read(ra, applied)).To(gomega.Equal(applied), "a deferred NAD patch must keep the old VRF")
		g.Expect(read(ra, nil)).To(gomega.BeEmpty(), "a cold start cannot infer the old VRF from the RA")
		replacement := ra.DeepCopy()
		replacement.UID = "replacement"
		g.Expect(read(replacement, applied)).To(gomega.BeEmpty(), "a replacement RA cannot inherit the VRF")
		degraded.Status = metav1.ConditionFalse
		g.Expect(read(ra, applied)).To(gomega.Equal(applied), "RA status can arrive before the NAD with the new VRF")

		ra.Generation++
		ra.Spec.Advertisements = []ratypes.AdvertisementType{ratypes.EgressIP}
		g.Expect(read(ra, applied)).To(gomega.Equal(applied), "success for the previous generation cannot confirm pod withdrawal")
		degraded.Status, degraded.ObservedGeneration = metav1.ConditionTrue, ra.Generation
		g.Expect(read(ra, applied)).To(gomega.Equal(applied), "a failed EgressIP-only reconcile cannot confirm pod withdrawal")
		ra.Status.Conditions[0].ObservedGeneration = ra.Generation
		degraded.Status = metav1.ConditionFalse
		g.Expect(read(ra, applied)).To(gomega.BeEmpty(), "drop the pod VRF after withdrawal even though the NAD still names the RA")
	})

	testNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testNodeName,
			Annotations: map[string]string{types.NodeSubnetsAnnotation: `{"default":"10.0.0.0/24","primary":"10.1.0.0/24"}`},
		},
	}
	otherNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "otherNode",
			Annotations: map[string]string{types.NodeSubnetsAnnotation: `{"default":"10.0.0.0/24","primary":"10.1.0.0/24"}`},
		},
	}

	tests := []struct {
		name           string
		network        *ovncnitypes.NetConf
		intent         *util.RouteAdvertisementsIntent
		legacyRef      *ratypes.RouteAdvertisements
		node           corev1.Node
		readyConfig    bool
		checkFRREvents bool
		expected       map[string][]string
	}{
		{name: "advertise the default network", network: defaultNetwork, intent: namedVRF, node: testNode,
			readyConfig: true, expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "advertise a primary network", network: primaryNetwork, intent: namedVRF, node: testNode,
			readyConfig: true, expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "use the default VRF on no-overlay", network: &noOverlayNetwork, intent: &util.RouteAdvertisementsIntent{},
			node: testNode, expected: map[string][]string{testNodeName: {types.DefaultNetworkName}}},
		{name: "use a named VRF on no-overlay", network: &noOverlayNetwork, intent: namedVRF, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "use a named VRF on EVPN", network: &evpnNetwork, intent: namedVRF, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "recover overlay from an old NAD reference on restart", network: primaryNetwork, legacyRef: accepted, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "recover the no-overlay VRF from an old NAD reference", network: &noOverlayNetwork, legacyRef: accepted, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}}},
		{name: "ignore an old EgressIP-only reference", network: primaryNetwork, legacyRef: egressOnly, node: testNode},
		{name: "a migrated RA cannot use its name alone to enable overlay", network: primaryNetwork, legacyRef: migrated, node: testNode},
		{name: "applied intent supersedes old acceptance and requires a local export", network: primaryNetwork, legacyRef: accepted,
			intent: namedVRF, node: testNode, checkFRREvents: true},
		{name: "start without NAD intent", network: primaryNetwork, node: testNode, readyConfig: true},
		{name: "skip a remote node", network: defaultNetwork, intent: namedVRF, node: otherNode, readyConfig: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, testNodeName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			t.Cleanup(wf.Shutdown)

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkController("", testNodeName, tcm, wf, false)

			namespace, name, err := cache.SplitMetaNamespaceKey(testNADName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadAnnotations := map[string]string{types.OvnNetworkNameAnnotation: tt.network.Name}
			if tt.legacyRef != nil {
				nadAnnotations[types.OvnRouteAdvertisementsKey] = fmt.Sprintf("[%q]", testRAName)
				_, err := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(t.Context(), tt.legacyRef.DeepCopy(), metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "seed the RA referenced by the old NAD annotation")
			}
			if tt.intent != nil {
				value, err := json.Marshal(map[string]*util.RouteAdvertisementsIntent{testRAName: tt.intent})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				nadAnnotations[types.OvnRouteAdvertisementsIntentKey] = string(value)
			}
			nad, err := buildNADWithAnnotations(name, namespace, tt.network, nadAnnotations)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &testNode, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			netInfo, err := util.NewNetInfo(tt.network)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			cfg := networkReadyConfig(t, testRAName, *namedVRF, netInfo, &tt.node, "generated")
			if tt.readyConfig {
				_, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Create(context.Background(), cfg, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}

			err = wf.Start()
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
			g.Expect(nm.Start()).To(gomega.Succeed())
			defer nm.Stop()

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

				g.Expect(reconcilable).ToNot(gomega.BeNil())

				if tt.expected == nil {
					tt.expected = map[string][]string{}
				}
				g.Expect(reconcilable.GetPodNetworkAdvertisedVRFs()).To(gomega.Equal(tt.expected), "pod advertisement VRFs for network %s", tt.network.Name)
			}

			g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "apply initial advertisement state")
			g.Consistently(meetsExpectations, 100*time.Millisecond).Should(gomega.Succeed(), "initial advertisement state must remain stable")
			if tt.legacyRef == accepted && tt.intent == nil {
				// The old manager has not yet accepted the change to next-vrf.
				ra := accepted.DeepCopy()
				ra.Generation++
				ra.Spec.TargetVRF = "next-vrf"
				raClient := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements()
				_, err := raClient.Update(t.Context(), ra, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "advance the RA spec before the old controller accepts it")
				g.Consistently(meetsExpectations, 100*time.Millisecond).Should(gomega.Succeed(), "keep the last accepted VRF while the new generation is pending")
				nextVRFs := []string{"next-vrf"}
				if tt.network.Transport == types.NetworkTransportNoOverlay {
					// A second RA migrates while the first RA's new VRF is pending.
					updated := nad.DeepCopy()
					updated.Annotations[types.OvnRouteAdvertisementsKey] = fmt.Sprintf(`[%q,"other"]`, testRAName)
					updated.Annotations[types.OvnRouteAdvertisementsIntentKey] = `{"other":{"uid":"other-uid","generation":1,"targetVRF":"blue"}}`
					g.Expect(wf.NADInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "publish the other RA's intent")
					nm.networkReconciler.Reconcile(tt.network.Name)
					nextVRFs = []string{"blue", "next-vrf"}
					tt.expected[testNodeName] = []string{"blue", testVRFName}
					g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "apply blue while preserving the pending RA's VRF")
				}
				ra.Status.Conditions[0].ObservedGeneration = ra.Generation
				_, err = raClient.UpdateStatus(t.Context(), ra, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "publish acceptance for the new generation")
				tt.expected[testNodeName] = nextVRFs
				g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "follow the accepted VRF during upgrade")
				if tt.network.Transport == types.NetworkTransportNoOverlay {
					g.Expect(raClient.Delete(t.Context(), ra.Name, metav1.DeleteOptions{})).To(gomega.Succeed(), "delete the legacy RA")
					replacement := ra.DeepCopy()
					replacement.UID = "replacement-ra"
					replacement.Generation = 1
					replacement.Status.Conditions[0] = metav1.Condition{Type: ratypes.RouteAdvertisementsAccepted, Status: metav1.ConditionFalse, ObservedGeneration: 1}
					_, err = raClient.Create(t.Context(), replacement, metav1.CreateOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred(), "recreate the legacy RA under the same name")
					tt.expected[testNodeName] = []string{"blue"}
					g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "the replacement cannot inherit the old RA's VRF")
				}
			}
			if !tt.checkFRREvents {
				return
			}
			// FRR events alone must update the advertised state, without polling
			// or an update to NAD intent.
			cfg, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Create(context.Background(), cfg, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create a ready FRR configuration")
			tt.expected = map[string][]string{testNodeName: {testVRFName}}
			g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "FRR creation must enable advertisements")
			prefixes := cfg.Spec.BGP.Routers[0].Prefixes
			cfg.Spec.BGP.Routers[0].Prefixes = nil
			cfg, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Update(context.Background(), cfg, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "remove the advertised routes")
			tt.expected = map[string][]string{}
			g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "removing the routes must disable advertisements")
			cfg.Spec.BGP.Routers[0].Prefixes = prefixes
			_, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Update(context.Background(), cfg, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "restore the advertised routes")
			tt.expected = map[string][]string{testNodeName: {testVRFName}}
			g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "restoring the routes must enable advertisements")
			g.Expect(fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Delete(context.Background(), cfg.Name, metav1.DeleteOptions{})).To(gomega.Succeed(), "delete the ready FRR configuration")
			tt.expected = map[string][]string{}
			g.Eventually(meetsExpectations, 5*time.Second).Should(gomega.Succeed(), "FRR deletion must disable advertisements")
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
			nm := newNetworkController("", "", tcm, nil, false)
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
			err := nm.syncNetwork(context.Background(), networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(callCount).To(gomega.Equal(1))
			g.Expect(gotNode).To(gomega.Equal("node1"))
			g.Expect(gotActive).To(gomega.Equal(tt.nodeHasNetwork))

			nm.NotifyNetworkRefChange(networkName, "node1")
			err = nm.syncNetwork(context.Background(), networkName)
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
	nm := newNetworkController("", "", tcm, nil, false)
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
	err = nm.deleteNetwork(context.Background(), networkName)
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
	err = nm.syncNetwork(context.Background(), networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(followupCalls).To(gomega.Equal(0))
}

func TestNetworkControllerCleansStoppedNetworkAfterStartFailureAndDeletion(t *testing.T) {
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
		raiseErrorWhenStartingController: fmt.Errorf("start failed"),
	}
	nm := newNetworkController("", "", tcm, nil, false)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)

	err = nm.syncNetwork(context.Background(), networkName)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("failed to start network"))
	failedState := nm.getNetworkState(networkName)
	g.Expect(failedState.controller).ToNot(gomega.BeNil())
	g.Expect(failedState.startFailed).To(gomega.BeTrue())

	nm.setNetwork(networkName, nil)
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.Succeed())
	g.Expect(nm.getNetworkState(networkName).controller).To(gomega.BeNil())

	tcm.Lock()
	defer tcm.Unlock()
	expectedNetworkKey := testNetworkKey(netInfo)
	g.Expect(tcm.started).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.stopped).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.cleaned).To(gomega.Equal([]string{expectedNetworkKey}))
}

func TestNetworkControllerCleansFailedStartBeforeRetry(t *testing.T) {
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

	startErr := fmt.Errorf("start failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil, false)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)

	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.Succeed())

	state := nm.getNetworkState(networkName)
	g.Expect(state.controller).ToNot(gomega.BeNil())
	g.Expect(state.startFailed).To(gomega.BeFalse())
	tcm.Lock()
	defer tcm.Unlock()
	expectedNetworkKey := testNetworkKey(netInfo)
	g.Expect(tcm.started).To(gomega.Equal([]string{expectedNetworkKey, expectedNetworkKey}))
	g.Expect(tcm.stopped).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.cleaned).To(gomega.Equal([]string{expectedNetworkKey}))
}

func TestNetworkControllerCleansFailedStartBeforeIncompatibleReplacement(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false
	config.OVNKubernetesFeature.EnableUplink = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
		Uplink:   "uplink-a",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	startErr := fmt.Errorf("start failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil, false)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	replacementConf := *netConf
	replacementConf.Uplink = "uplink-b"
	replacementInfo, err := util.NewNetInfo(&replacementConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(util.AreNetworksCompatible(netInfo, replacementInfo)).To(gomega.BeFalse())
	replacement := util.NewMutableNetInfo(replacementInfo)
	replacement.SetNADs(replacementConf.NADName)
	nm.setNetwork(networkName, replacement)

	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.Succeed())

	tcm.Lock()
	defer tcm.Unlock()
	g.Expect(tcm.started).To(gomega.HaveLen(2))
	g.Expect(tcm.stopped).To(gomega.HaveLen(1))
	g.Expect(tcm.cleaned).To(gomega.HaveLen(1))
}

func TestNetworkControllerFinishesTerminalCleanupBeforeFailedStartRecreation(t *testing.T) {
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
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)

	startErr := fmt.Errorf("start failed")
	cleanupErr := fmt.Errorf("cleanup failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil, false)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	nm.setNetwork(networkName, nil)
	tcm.Lock()
	tcm.raiseErrorWhenCleaningController = cleanupErr
	tcm.Unlock()
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.MatchError(gomega.ContainSubstring("cleanup failed")))

	nm.setNetwork(networkName, mutableNetInfo)
	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.raiseErrorWhenCleaningController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(context.Background(), networkName)).To(gomega.Succeed())

	state := nm.getNetworkState(networkName)
	g.Expect(state.controller).ToNot(gomega.BeNil())
	g.Expect(state.startFailed).To(gomega.BeFalse())
	tcm.Lock()
	defer tcm.Unlock()
	g.Expect(tcm.started).To(gomega.HaveLen(2))
	g.Expect(tcm.stopped).To(gomega.HaveLen(1))
	g.Expect(tcm.cleaned).To(gomega.HaveLen(2))
}

func TestNetworkControllerReconcilesAdvertisementUpdateDuringStart(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "starting-udn", Type: "ovn-k8s-cni-overlay"},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.128.0.0/14",
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	starting := make(chan struct{})
	finishStart := make(chan struct{})
	var finishOnce sync.Once
	unblockStart := func() { finishOnce.Do(func() { close(finishStart) }) }
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		beforeStart: func() {
			close(starting)
			<-finishStart
		},
	}
	nm := newNetworkController("", "node1", tcm, nil, true)
	t.Cleanup(func() {
		unblockStart()
		nm.Stop()
	})
	want := util.NewMutableNetInfo(netInfo)
	nm.EnsureNetwork(want)
	g.Expect(nm.Start()).To(gomega.Succeed())
	g.Eventually(starting, 5*time.Second).Should(gomega.BeClosed(), "the network controller must enter Start")
	g.Expect(nm.getNetworkState(netInfo.GetNetworkName()).controller).To(gomega.BeNil(), "a controller still in Start must not be registered as running")

	// Supply the new advertisement state while Start is blocked, then invoke
	// the same scheduling path used by node and FRR informer callbacks. The
	// starting controller has already copied the old, unadvertised state.
	updated := util.NewMutableNetInfo(netInfo)
	expected := map[string][]string{"node1": {types.DefaultNetworkName}}
	updated.SetPodNetworkAdvertisedVRFs(expected)
	nm.setNetwork(updated.GetNetworkName(), updated)
	g.Expect(nm.syncNetworks()).To(gomega.Succeed())
	unblockStart()

	g.Eventually(func(g gomega.Gomega) {
		ctrl, _, _ := nm.getReconcilableNetworkState(netInfo.GetNetworkName())
		g.Expect(ctrl).NotTo(gomega.BeNil(), "the network controller must be registered after Start completes")
		g.Expect(ctrl.GetPodNetworkAdvertisedVRFs()).To(gomega.Equal(expected), "the controller must receive the advertisement update queued during Start")
	}, 5*time.Second).Should(gomega.Succeed(), "advertisement updates during Start must converge without another event")
}

func TestNetworkControllerAsyncSyncAllSkipsInlineStart(t *testing.T) {
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
		startDelay: 300 * time.Millisecond,
	}
	nm := newNetworkController("", "", tcm, nil, true)
	t.Cleanup(nm.Stop)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	// Mirror production: NAD sync ensures networks (and queues reconciler work)
	// before networkController.Start / syncAll.
	nm.EnsureNetwork(mutableNetInfo)

	start := time.Now()
	g.Expect(nm.Start()).To(gomega.Succeed(), "async syncAll must return before UDN Start finishes")
	g.Expect(time.Since(start)).To(gomega.BeNumerically("<", 200*time.Millisecond),
		"async syncAll must not wait for NetworkController.Start")

	tcm.Lock()
	startedImmediately := len(tcm.started)
	tcm.Unlock()
	g.Expect(startedImmediately).To(gomega.Equal(0),
		"UDN Start must not run during syncAll when asyncNetworkStart is set")

	expectedNetworkKey := testNetworkKey(netInfo)
	g.Eventually(func() []string {
		tcm.Lock()
		defer tcm.Unlock()
		return append([]string(nil), tcm.started...)
	}).WithTimeout(2*time.Second).Should(gomega.ContainElement(expectedNetworkKey),
		"reconciler workers should start the UDN queued by EnsureNetwork")

	ctrl, _, startFailed := nm.getReconcilableNetworkState(networkName)
	g.Expect(ctrl).ToNot(gomega.BeNil())
	g.Expect(startFailed).To(gomega.BeFalse())
}

// TestNetworkControllerSyncNetworksQueuesKnownNetworks verifies that
// syncNetworks queues every network known to the controller, even one
// that did not go through Start and so has no controller state yet.
func TestNetworkControllerSyncNetworksQueuesKnownNetworks(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "failed to prepare test config")
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "failed to prepare test config")
	})

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
	g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to create NetInfo")
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()

	var mu sync.Mutex
	var reconciled []string
	fakeReconciler := controller.NewReconciler("test-network-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(_ context.Context, key string) error {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, key)
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(fakeReconciler)).To(gomega.Succeed(), "failed to start test reconciler")
	t.Cleanup(func() { controller.Stop(fakeReconciler) })

	nm := newNetworkController("", "", &testControllerManager{controllers: map[string]NetworkController{}}, nil, false)
	nm.networkReconciler = fakeReconciler
	// Place the network directly in c.networks without going through
	// EnsureNetwork/Start, so no networkControllerState exists for it yet.
	nm.setNetwork(networkName, mutableNetInfo)

	g.Expect(nm.syncNetworks()).To(gomega.Succeed(), "syncNetworks failed")

	g.Eventually(func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reconciled...)
	}).WithTimeout(2*time.Second).Should(gomega.ContainElements(types.DefaultNetworkName, networkName),
		"expected default network and %s to be reconciled", networkName)
}

// TestNetworkController_ConcurrentReconciliation validates that the networkReconciler
// can safely handle concurrent network additions and deletions without data races.
func TestNetworkController_ConcurrentReconciliation(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("test", "", tcm, wf, false)

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	g.Expect(nm.Start()).To(gomega.Succeed())
	defer nm.Stop()

	const numNetworks = 20
	const numIterations = 3

	for iteration := 0; iteration < numIterations; iteration++ {
		t.Logf("Iteration %d/%d: Testing concurrent add/delete of %d networks", iteration+1, numIterations, numNetworks)

		// Phase 1: Concurrent network additions
		var addWg sync.WaitGroup
		for i := 0; i < numNetworks; i++ {
			addWg.Add(1)
			go func(idx int) {
				defer addWg.Done()

				// Create a test network
				networkName := fmt.Sprintf("test-net-%d-%d", iteration, idx)
				netConf := &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName,
						Type: "ovn-k8s-cni-overlay",
					},
					Topology: "layer2",
					Role:     "secondary",
					MTU:      1400,
				}

				netInfo, err := util.NewNetInfo(netConf)
				if err != nil {
					t.Errorf("Failed to create NetInfo for %s: %v", networkName, err)
					return
				}

				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				nm.EnsureNetwork(mutableNetInfo)
			}(i)
		}
		addWg.Wait()

		// getAllNetworks() returns only secondary networks, not the default network
		g.Eventually(nm.getAllNetworks, 5*time.Second, 100*time.Millisecond).
			Should(gomega.HaveLen(numNetworks))

		// Phase 2: Concurrent network deletions
		var delWg sync.WaitGroup
		for i := 0; i < numNetworks; i++ {
			delWg.Add(1)
			go func(idx int) {
				defer delWg.Done()
				networkName := fmt.Sprintf("test-net-%d-%d", iteration, idx)
				nm.DeleteNetwork(networkName)
			}(i)
		}
		delWg.Wait()

		g.Eventually(nm.getAllNetworks).WithTimeout(5*time.Second).
			WithPolling(100*time.Millisecond).Should(gomega.BeEmpty(),
			"all test networks should be deleted")
	}
}

// TestNetworkController_ConcurrentReconciliationMixed validates concurrent operations
// with mixed add, update, and delete operations happening simultaneously.
func TestNetworkController_ConcurrentReconciliationMixed(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("test", "", tcm, wf, false)

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	g.Expect(nm.Start()).To(gomega.Succeed())
	defer nm.Stop()

	const numOperations = 30
	const numUniqueNetworks = 10
	var wg sync.WaitGroup

	for i := 0; i < numOperations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			networkName := fmt.Sprintf("mixed-net-%d", idx%numUniqueNetworks)
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: networkName,
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: "layer2",
				Role:     "secondary",
				MTU:      1400,
			}

			netInfo, err := util.NewNetInfo(netConf)
			if err != nil {
				t.Errorf("Failed to create NetInfo: %v", err)
				return
			}

			mutableNetInfo := util.NewMutableNetInfo(netInfo)

			// Randomly add or delete
			if idx%3 == 0 {
				nm.DeleteNetwork(networkName)
			} else {
				nm.EnsureNetwork(mutableNetInfo)
			}
		}(i)
	}
	wg.Wait()

	// Ensure all networks exist to reach a deterministic final state
	for i := 0; i < numUniqueNetworks; i++ {
		networkName := fmt.Sprintf("mixed-net-%d", i)
		netConf := &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: networkName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology: "layer2",
			Role:     "secondary",
			MTU:      1400,
		}

		netInfo, err := util.NewNetInfo(netConf)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		nm.EnsureNetwork(mutableNetInfo)
	}

	// Verify all networks exist (deterministic final state)
	g.Eventually(func(g gomega.Gomega) {
		networks := nm.getAllNetworks()
		// getAllNetworks() returns secondary networks only
		g.Expect(networks).To(gomega.HaveLen(numUniqueNetworks),
			"Expected %d networks after mixed operations, got %d", numUniqueNetworks, len(networks))

		// Verify each network can be retrieved
		for _, network := range networks {
			retrieved := nm.getNetwork(network.GetNetworkName())
			g.Expect(retrieved).ToNot(gomega.BeNil())
		}
	}).Should(gomega.Succeed())
}

// Both node agents must queue only the networks referenced by the updated RA,
// including the default network and CUDNs with NADs in several namespaces.
func TestNetworkControllerSyncRouteAdvertisements(t *testing.T) {
	factories := []struct {
		name string
		new  func() (*factory.WatchFactory, error)
	}{
		{
			name: "ovnkube-controller",
			new: func() (*factory.WatchFactory, error) {
				return factory.NewOVNKubeControllerWatchFactory(util.GetOVNClientset().GetOVNKubeControllerClientset(), "test-node")
			},
		},
		{
			name: "node",
			new: func() (*factory.WatchFactory, error) {
				return factory.NewNodeWatchFactory(util.GetOVNClientset().GetNodeClientset(), "test-node")
			},
		},
	}
	for _, factoryConfig := range factories {
		t.Run(factoryConfig.name, func(t *testing.T) {
			runSyncRouteAdvertisementsTest(t, factoryConfig.new)
		})
	}
}

// runSyncRouteAdvertisementsTest exercises syncRouteAdvertisements against the
// watch factory built by newFactory and asserts the exact reconcile requests,
// including duplicate suppression.
func runSyncRouteAdvertisementsTest(t *testing.T, newFactory func() (*factory.WatchFactory, error)) {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "failed to prepare test config")
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "failed to restore test config")
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true

	wf, err := newFactory()
	g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create watch factory")
	t.Cleanup(wf.Shutdown)

	nm := newNetworkController("test", "test-node", &testControllerManager{controllers: map[string]NetworkController{}}, wf, false)
	controller.Stop(nm.networkReconciler)
	t.Cleanup(nm.Stop)

	fakeReconciler := &controller.FakeController{}
	nm.networkReconciler = fakeReconciler

	for _, network := range []string{"net-a", "net-b", "net-c"} {
		netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: network, Type: "ovn-k8s-cni-overlay"},
			Topology: types.Layer2Topology,
			Role:     types.NetworkRoleSecondary,
			MTU:      1400,
		})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create network info for %q", network)
		nm.setNetwork(network, util.NewMutableNetInfo(netInfo))
	}

	newNAD := func(namespace, name, network string, advertisements ...string) *nadapi.NetworkAttachmentDefinition {
		annotations := map[string]string{}
		if network != "" {
			annotations[types.OvnNetworkNameAnnotation] = network
		}
		raAnnotation, err := json.Marshal(advertisements)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to marshal route advertisements")
		annotations[types.OvnRouteAdvertisementsKey] = string(raAnnotation)
		return &nadapi.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations},
		}
	}
	nads := []*nadapi.NetworkAttachmentDefinition{
		newNAD("ns", "nad-a1", "net-a", "ra-1"),
		newNAD("ns", "nad-a2", "net-a", "ra-1", "ra-2", "ra-3"),
		newNAD("ns", "nad-b", "net-b", "ra-1", "ra-2"),
		newNAD("ns", "nad-c", "net-c", "ra-3"),
		newNAD(config.Default.ClusterDefaultNetworkNAD.Namespace, config.Default.ClusterDefaultNetworkNAD.Name, "", "ra-1"),
		newNAD("ns", "nad-unannotated", "", "ra-1"),
		newNAD("ns", "nad-absent", "net-d", "ra-1"),
	}
	for _, nad := range nads {
		g.Expect(wf.NADInformer().Informer().GetIndexer().Add(nad)).To(gomega.Succeed(), "failed to add NAD %s/%s to indexer", nad.Namespace, nad.Name)
	}

	tests := []struct {
		raName   string
		expected []string
	}{
		{raName: "ra-1", expected: []string{"net-a", "net-b", types.DefaultNetworkName}},
		{raName: "ra-2", expected: []string{"net-a", "net-b"}},
		{raName: "ra-3", expected: []string{"net-a", "net-c"}},
		{raName: "ra-4", expected: nil},
	}
	for _, test := range tests {
		t.Run(test.raName, func(t *testing.T) {
			g := gomega.NewWithT(t)
			fakeReconciler := &controller.FakeController{}
			nm.networkReconciler = fakeReconciler

			g.Expect(nm.syncRouteAdvertisements(context.Background(), test.raName)).To(gomega.Succeed(), "failed to sync route advertisements")

			expected := make([]string, 0, len(test.expected))
			for _, network := range test.expected {
				expected = append(expected, "Reconcile:"+network)
			}
			g.Expect(fakeReconciler.Reconciles).To(gomega.ConsistOf(expected), "unexpected networks reconciled")
		})
	}
}

func networkReadyConfig(t *testing.T, raName string, intent util.RouteAdvertisementsIntent, network util.NetInfo, node *corev1.Node, key string) *frrtypes.FRRConfiguration {
	t.Helper()
	prefixes, err := util.GetRouteAdvertisementsPodPrefixes(network, node)
	if err != nil {
		t.Fatalf("get pod prefixes for network %s on node %s: %v", network.GetNetworkName(), node.Name, err)
	}
	router := frrtypes.Router{
		VRF: intent.ResolvedVRF(network), Prefixes: prefixes,
		Neighbors: []frrtypes.Neighbor{{Interface: "eth0", ToAdvertise: frrtypes.Advertise{
			Allowed: frrtypes.AllowedOutPrefixes{Mode: frrtypes.AllowRestricted, Prefixes: prefixes},
		}}},
	}
	return &frrtypes.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: "frr",
		Labels:      map[string]string{types.OvnRouteAdvertisementsKey: raName, types.OvnRouteAdvertisementsNodeKey: util.RouteAdvertisementsNodeLabel(node.Name)},
		Annotations: map[string]string{types.OvnRouteAdvertisementsKey: key}},
		Spec: frrtypes.FRRConfigurationSpec{BGP: frrtypes.BGPConfig{Routers: []frrtypes.Router{router}}}}
}

func TestNodeAdvertisementsReady(t *testing.T) {
	for _, nodeName := range []string{"node", strings.Repeat("a", 63) + ".example.org"} {
		t.Run(nodeName, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			wf, err := factory.NewNodeWatchFactory(util.GetOVNClientset().GetNodeClientset(), nodeName)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()
			indexer := wf.FRRConfigurationsInformer().Informer().GetIndexer()
			c := &networkController{frrIndexer: indexer}
			network := &util.DefaultNetInfo{}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{types.NodeSubnetsAnnotation: `{"default":"10.0.0.0/24"}`}}}
			const raName = "ra"
			intent := util.RouteAdvertisementsIntent{}
			check := func(expected bool) {
				t.Helper()
				ready, err := c.nodeAdvertisementsReady(raName, intent, network, node)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "check readiness for RA %s, network %s, node %s", raName, network.GetNetworkName(), node.Name)
				g.Expect(ready).To(gomega.Equal(expected), "readiness for RA %s, network %s, node %s", raName, network.GetNetworkName(), node.Name)
			}
			check(false)
			first := networkReadyConfig(t, raName, intent, network, node, "first")
			second := networkReadyConfig(t, raName, intent, network, node, "second")
			g.Expect(indexer.Add(first)).To(gomega.Succeed())
			check(true) // One configuration carrying the routes is enough.
			g.Expect(indexer.Add(second)).To(gomega.Succeed())
			check(true)
			g.Expect(indexer.Delete(first)).To(gomega.Succeed())
			check(true)
			// FRRConfiguration has no status fields, but a status write can
			// still change its resource version and managed fields.
			statusOnly := second.DeepCopy()
			statusOnly.ResourceVersion = "2"
			statusOnly.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "frr-k8s", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status"}}
			g.Expect(frrNeedsUpdate(second, statusOnly)).To(gomega.BeFalse())
			intent.TargetVRF = "changed"
			check(false)
			intent.TargetVRF = ""
			noLongerSelected := second.DeepCopy()
			noLongerSelected.Spec.NodeSelector.MatchLabels = map[string]string{corev1.LabelHostname: "previous-hostname"}
			g.Expect(indexer.Update(noLongerSelected)).To(gomega.Succeed())
			check(false) // Preserved routes cannot authorize a node the configuration no longer selects.
			g.Expect(indexer.Update(second)).To(gomega.Succeed())
			removed := second.DeepCopy()
			removed.Spec.BGP.Routers[0].Prefixes = nil
			g.Expect(indexer.Update(removed)).To(gomega.Succeed())
			check(false) // A configuration whose routes are gone no longer counts.
			g.Expect(indexer.Update(second)).To(gomega.Succeed())
			blue, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "cluster_udn_blue", Type: "ovn-k8s-cni-overlay"}, Topology: types.Layer3Topology, Role: "primary"})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			node.Annotations[types.NodeSubnetsAnnotation] = `{"default":"10.0.0.0/24","cluster_udn_blue":"10.1.0.0/24"}`
			otherReady, err := c.nodeAdvertisementsReady(raName, intent, blue, node)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(otherReady).To(gomega.BeFalse(), "an existing configuration cannot authorize a newly selected network")
			check(true)
			withBlue := second.DeepCopy()
			// Both networks are advertised into the same target router.
			withBlue.Spec.BGP.Routers[0].Prefixes = append(withBlue.Spec.BGP.Routers[0].Prefixes, "10.1.0.0/24")
			g.Expect(indexer.Update(withBlue)).To(gomega.Succeed())
			check(true)
			otherReady, err = c.nodeAdvertisementsReady(raName, intent, blue, node)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(otherReady).To(gomega.BeFalse(), "originating blue's prefix alone must keep its SNAT fallback")
			withBlue = withBlue.DeepCopy()
			withBlue.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes = withBlue.Spec.BGP.Routers[0].Prefixes
			g.Expect(indexer.Update(withBlue)).To(gomega.Succeed())
			otherReady, err = c.nodeAdvertisementsReady(raName, intent, blue, node)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(otherReady).To(gomega.BeTrue(), "blue is ready once a peer can export its route")
			g.Expect(indexer.Update(second)).To(gomega.Succeed())
			node.Annotations[types.NodeSubnetsAnnotation] = `{"default":["10.0.1.0/24","fd00::/64"]}`
			check(false)
			updated := second.DeepCopy()
			updated.Spec.BGP.Routers[0].Prefixes = []string{"10.0.1.0/24"}
			v4Peer := &updated.Spec.BGP.Routers[0].Neighbors[0]
			v4Peer.Interface, v4Peer.Address = "", "192.0.2.1"
			v4Peer.ToAdvertise.Allowed.Prefixes = []string{"10.0.1.0/24"}
			g.Expect(indexer.Update(updated)).To(gomega.Succeed())
			check(false)
			updated = updated.DeepCopy()
			updated.Spec.BGP.Routers[0].Prefixes = append(updated.Spec.BGP.Routers[0].Prefixes, "fd00::/64")
			g.Expect(indexer.Update(updated)).To(gomega.Succeed())
			check(false)
			v6Peer := frrtypes.Neighbor{
				Address:     "2001:db8::1",
				ToAdvertise: frrtypes.Advertise{Allowed: frrtypes.AllowedOutPrefixes{Mode: frrtypes.AllowRestricted, Prefixes: []string{"fd00::/64"}}},
			}
			v6 := first.DeepCopy()
			router := &v6.Spec.BGP.Routers[0]
			router.VRF, router.Prefixes, router.Neighbors = "blue", []string{"fd00::/64"}, []frrtypes.Neighbor{v6Peer}
			g.Expect(indexer.Add(v6)).To(gomega.Succeed(), "add the IPv6 template in a different VRF")
			check(false)
			v6 = v6.DeepCopy()
			v6.Spec.BGP.Routers[0].VRF = ""
			g.Expect(indexer.Update(v6)).To(gomega.Succeed(), "export IPv6 into the same VRF as IPv4")
			check(true)
			g.Expect(indexer.Delete(v6)).To(gomega.Succeed(), "withdraw the only IPv6 export")
			check(false)
			updated = updated.DeepCopy()
			updated.Spec.BGP.Routers[0].Neighbors = append(updated.Spec.BGP.Routers[0].Neighbors, v6Peer)
			g.Expect(indexer.Update(updated)).To(gomega.Succeed())
			check(true)
			g.Expect(indexer.Update(second)).To(gomega.Succeed())
			node.Annotations[types.NodeSubnetsAnnotation] = `{"default":"10.0.0.0/24"}`
			deleting := second.DeepCopy()
			now := metav1.Now()
			deleting.DeletionTimestamp = &now
			g.Expect(indexer.Update(deleting)).To(gomega.Succeed())
			check(false)
			g.Expect(frrNeedsUpdate(first, removed)).To(gomega.BeTrue(), "route cleanup triggers readiness reconciliation")
			g.Expect(frrNeedsUpdate(nil, first)).To(gomega.BeTrue())
		})
	}
}

// Each RA's intent recorded on the NAD is evaluated independently, without
// reading the RA: an entry holds until cluster manager changes it.
func TestSetAdvertisementsFollowsNADIntent(t *testing.T) {
	for _, transport := range []string{"", types.NetworkTransportNoOverlay, types.NetworkTransportEVPN} {
		t.Run(transport, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			wf, err := factory.NewOVNKubeControllerWatchFactory(util.GetOVNClientset().GetOVNKubeControllerClientset(), "node")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()
			c := newNetworkController("", "node", &testControllerManager{}, wf, false)
			netConf := &ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "primary", Type: "ovn-k8s-cni-overlay"}, Topology: types.Layer3Topology, Role: "primary", Transport: transport}
			network, err := util.NewNetInfo(netConf)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			netInfo := util.NewMutableNetInfo(network)
			nad, err := buildNADWithAnnotations("nad", "test", netConf, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			setIntents := func(namespace string, intents map[string]util.RouteAdvertisementsIntent) {
				t.Helper()
				value, err := json.Marshal(intents)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "encode intent for namespace %s", namespace)
				updated := nad.DeepCopy()
				updated.Namespace = namespace
				updated.Annotations = map[string]string{types.OvnRouteAdvertisementsIntentKey: string(value)}
				g.Expect(wf.NADInformer().Informer().GetIndexer().Update(updated)).To(gomega.Succeed(), "update the NAD in %s", namespace)
			}
			a := util.RouteAdvertisementsIntent{TargetVRF: "vrf-a", EgressIP: true}
			b := util.RouteAdvertisementsIntent{UID: "uid-b", Generation: 4, TargetVRF: "vrf-b", EgressIP: true}
			setIntents("test", map[string]util.RouteAdvertisementsIntent{"a": a, "b": b})
			c.getNADKeysForNetwork = func(string) []string { return []string{"test/nad"} }
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{types.NodeSubnetsAnnotation: `{"primary":"10.1.0.0/24"}`}}}
			g.Expect(wf.NodeCoreInformer().Informer().GetIndexer().Add(node)).To(gomega.Succeed())
			configFor := func(name, vrf string) *frrtypes.FRRConfiguration {
				return networkReadyConfig(t, name, util.RouteAdvertisementsIntent{TargetVRF: vrf}, netInfo, node, name+"/peers/node")
			}
			cfgA, cfgB := configFor("a", "vrf-a"), configFor("b", "vrf-b")
			g.Expect(c.frrIndexer.Add(cfgA)).To(gomega.Succeed())
			g.Expect(c.frrIndexer.Add(cfgB)).To(gomega.Succeed())
			check := func(vrfs ...string) {
				t.Helper()
				_, err := c.setAdvertisements(netInfo, nil)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "refresh advertisements for network %s", netInfo.GetNetworkName())
				g.Expect(netInfo.GetPodNetworkAdvertisedVRFs()[node.Name]).To(gomega.ConsistOf(vrfs), "pod advertisement VRFs on node %s", node.Name)
				g.Expect(netInfo.GetEgressIPAdvertisedVRFs()[node.Name]).To(gomega.ConsistOf(vrfs), "EgressIP advertisement VRFs on node %s", node.Name)
			}
			check("vrf-a", "vrf-b")
			// Only overlay depends on the generated configurations.
			g.Expect(c.frrIndexer.Delete(cfgB)).To(gomega.Succeed())
			if transport == "" {
				check("vrf-a")
			} else {
				check("vrf-a", "vrf-b")
			}
			// Only one namespace has received B's VRF change.
			newB := b
			newB.Generation++
			newB.TargetVRF = "vrf-b-new"
			setIntents("test", map[string]util.RouteAdvertisementsIntent{"a": a, "b": newB})
			setIntents("other", map[string]util.RouteAdvertisementsIntent{"b": b})
			checkBothOrders := func(vrfs ...string) {
				t.Helper()
				for _, order := range [][]string{{"test/nad", "other/nad"}, {"other/nad", "test/nad"}} {
					c.getNADKeysForNetwork = func(string) []string { return order }
					check(vrfs...)
				}
			}
			g.Expect(c.frrIndexer.Add(cfgB)).To(gomega.Succeed(), "cache still has B's export in the old VRF")
			if transport == "" {
				checkBothOrders("vrf-a")
			}
			g.Expect(c.frrIndexer.Update(configFor("b", "vrf-b-new"))).To(gomega.Succeed(), "observe B's export in the new VRF")
			checkBothOrders("vrf-a", "vrf-b-new")

			legacyB := util.RouteAdvertisementsIntent{TargetVRF: "vrf-b"}
			setIntents("other", map[string]util.RouteAdvertisementsIntent{"b": legacyB})
			checkBothOrders("vrf-a", "vrf-b-new")
			setIntents("test", map[string]util.RouteAdvertisementsIntent{"a": a, "b": {TargetVRF: "vrf-b-new", EgressIP: true}})
			checkBothOrders("vrf-a")

			// B was recreated under the same name. Its new generation 1 cannot
			// be compared with generation 4 from the deleted RA.
			newB.UID, newB.Generation = "recreated-b", 1
			setIntents("test", map[string]util.RouteAdvertisementsIntent{"a": a, "b": newB})
			setIntents("other", map[string]util.RouteAdvertisementsIntent{"b": b})
			checkBothOrders("vrf-a")
			setIntents("other", map[string]util.RouteAdvertisementsIntent{"b": newB})
			checkBothOrders("vrf-a", "vrf-b-new")
			g.Expect(c.frrIndexer.Delete(cfgA)).To(gomega.Succeed())
			if transport == "" {
				check("vrf-b-new")
			} else {
				check("vrf-a", "vrf-b-new")
			}
			setIntents("test", map[string]util.RouteAdvertisementsIntent{})
			check("vrf-b-new")
			setIntents("other", map[string]util.RouteAdvertisementsIntent{})
			check()
		})
	}
}
