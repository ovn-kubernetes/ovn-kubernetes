// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newUplinkStateFixture(
	uplinkName, nodeName string, conditions ...metav1.Condition,
) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{Conditions: conditions},
	}
}

func resolvedTrueCondition() metav1.Condition {
	return metav1.Condition{
		Type:   uplinkv1alpha1.UplinkStateConditionResolved,
		Status: metav1.ConditionTrue,
		Reason: uplinkv1alpha1.UplinkStateReasonResolved,
	}
}

func newUplinkGatewayControllerForTest(
	t *testing.T,
	uplinkName, nodeName string,
) (*UplinkGatewayController, *uplinkfake.Clientset) {
	t.Helper()
	state := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	controller := NewUplinkGatewayController(nodeName, client, uplinklisters.NewUplinkStateLister(indexer))
	return controller, client
}

func uplinkGatewayNetInfo(t *testing.T, networkName, uplinkName string) util.NetInfo {
	t.Helper()
	nad := generateUplinkNAD(
		networkName,
		networkName+"-nad",
		"test",
		types.Layer3Topology,
		"100.128.0.0/16/24",
		types.NetworkRolePrimary,
		uplinkName,
	)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}
	return netInfo
}

func prepareUplinkGatewayControllerTest(t *testing.T) {
	t.Helper()
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true
}

func getUplinkGatewayCondition(
	t *testing.T,
	client *uplinkfake.Clientset,
	uplinkName, nodeName string,
) (*metav1.Condition, *metav1.Condition) {
	t.Helper()
	state, err := client.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName(uplinkName, nodeName),
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("failed to get UplinkState: %v", err)
	}
	return meta.FindStatusCondition(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionGatewayReady),
		meta.FindStatusCondition(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionResolved)
}

func TestUplinkGatewayControllerRepublishesWipedCondition(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	// Nothing published yet: republish must not invent a condition.
	if err := controller.RepublishGatewayCondition(uplinkName); err != nil {
		t.Fatalf("failed to republish before first publish: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady != nil {
		t.Fatalf("expected no GatewayReady condition before first publish, got %#v", gatewayReady)
	}

	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected published GatewayReady condition, got %#v", gatewayReady)
	}

	// Simulate an out-of-band deletion and recreation: the recreated object
	// carries only the discovery condition. The lister already reflects that
	// shape (it was never updated with the published condition).
	recreated := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), recreated, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to wipe GatewayReady condition: %v", err)
	}

	if err := controller.RepublishGatewayCondition(uplinkName); err != nil {
		t.Fatalf("failed to republish after wipe: %v", err)
	}
	gatewayReady, resolved := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured {
		t.Fatalf("expected restored GatewayReady condition, got %#v", gatewayReady)
	}
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("expected Resolved to remain true, got %#v", resolved)
	}
}

func TestUplinkGatewayControllerInvalidatesIntentionalStateDeletion(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}
	controller.InvalidateGatewayState(uplinkName)

	// Model the UplinkState created after the node is selected again. Unlike an
	// out-of-band deletion, an intentional deletion invalidated the old
	// GatewayReady condition, so it must not be restored.
	recreated := newUplinkStateFixture(uplinkName, nodeName)
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), recreated, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to recreate UplinkState: %v", err)
	}

	if err := controller.RepublishGatewayCondition(uplinkName); err != nil {
		t.Fatalf("failed to check invalidated gateway condition: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady != nil {
		t.Fatalf("expected invalidated GatewayReady not to be restored, got %#v", gatewayReady)
	}

	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	phase := networkState.phase
	controller.mutex.Unlock()
	if phase != uplinkGatewayNetworkPending {
		t.Fatalf("expected cached network readiness to be pending, got %q", phase)
	}

	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network in the new lifecycle: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected GatewayReady after fresh reconciliation, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerDeletesRemovedUplinkCache(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}
	controller.DeleteGatewayState(uplinkName)

	controller.mutex.Lock()
	_, uplinkFound := controller.uplinks[uplinkName]
	_, networkFound := controller.uplinkByNetworkName[network.GetNetworkName()]
	controller.mutex.Unlock()
	if uplinkFound || networkFound {
		t.Fatalf("expected deleted Uplink cache to be removed, uplinkFound=%t networkFound=%t",
			uplinkFound, networkFound)
	}

	// A same-name Uplink created later is a new lifecycle and must not inherit
	// readiness from the deleted resource.
	recreated := newUplinkStateFixture(uplinkName, nodeName)
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), recreated, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to recreate UplinkState: %v", err)
	}
	if err := controller.RepublishGatewayCondition(uplinkName); err != nil {
		t.Fatalf("failed to check deleted gateway cache: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady != nil {
		t.Fatalf("expected no GatewayReady from deleted Uplink cache, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerInvalidationDiscardsInFlightCompletion(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- controller.ReconcileNetwork(network, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	controller.InvalidateGatewayState(uplinkName)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("failed to finish invalidated reconciliation: %v", err)
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected old completion to leave GatewayReady pending, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerAggregatesActiveNetworks(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	red := uplinkGatewayNetInfo(t, "red", uplinkName)
	blue := uplinkGatewayNetInfo(t, "blue", uplinkName)

	if err := controller.SyncNetworks(red, blue); err != nil {
		t.Fatalf("failed to sync networks: %v", err)
	}
	gatewayReady, resolved := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("unexpected pending GatewayReady condition: %#v", gatewayReady)
	}
	if !strings.Contains(gatewayReady.Message, "2 of 2 active CUDN(s)") {
		t.Fatalf("unexpected pending condition message: %q", gatewayReady.Message)
	}
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("expected Resolved to remain true, got %#v", resolved)
	}

	if err := controller.ReconcileNetwork(red, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile red: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected blue to keep aggregate readiness pending, got %#v", gatewayReady)
	}

	if err := controller.ReconcileNetwork(blue, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile blue: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured {
		t.Fatalf("unexpected ready GatewayReady condition: %#v", gatewayReady)
	}

	green := uplinkGatewayNetInfo(t, "green", uplinkName)
	if err := controller.PrepareNetwork(green); err != nil {
		t.Fatalf("failed to prepare green: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected a new active network to reset readiness, got %#v", gatewayReady)
	}

	if err := controller.SyncNetworks(); err != nil {
		t.Fatalf("failed to clear active networks: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured ||
		!strings.Contains(gatewayReady.Message, "No active CUDNs") {
		t.Fatalf("expected stale aggregate readiness to be cleared, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerReportsFailures(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	expectedErr := errors.New("failed to configure bridge mapping")
	err := controller.ReconcileNetwork(network, func() error {
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed, expectedErr)
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected bridge mapping error, got %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed {
		t.Fatalf("unexpected bridge mapping failure condition: %#v", gatewayReady)
	}

	expectedErr = errors.New("failed to program flows")
	err = controller.ReconcileNetwork(network, func() error { return expectedErr })
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected programming error, got %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
		t.Fatalf("unexpected gateway programming failure condition: %#v", gatewayReady)
	}

	expectedErr = errors.New("failed to program flows after patch port readiness")
	err = controller.ReconcileNetwork(network, func() error {
		programmingErr := newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, expectedErr)
		waitErr := fmt.Errorf("error waiting for node readiness: %w", programmingErr)
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed, waitErr)
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected wrapped programming error, got %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
		t.Fatalf("unexpected wrapped gateway programming failure condition: %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerSerializesSharedUplinkProgramming(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	red := uplinkGatewayNetInfo(t, "red", uplinkName)
	blue := uplinkGatewayNetInfo(t, "blue", uplinkName)
	if err := controller.SyncNetworks(red, blue); err != nil {
		t.Fatalf("failed to sync networks: %v", err)
	}

	entered := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	reconcile := func(network util.NetInfo) {
		defer wg.Done()
		if err := controller.ReconcileNetwork(network, func() error {
			entered <- network.GetNetworkName()
			<-release
			return nil
		}); err != nil {
			t.Errorf("failed to reconcile %s: %v", network.GetNetworkName(), err)
		}
	}

	wg.Add(1)
	go reconcile(red)
	<-entered
	wg.Add(1)
	go reconcile(blue)
	select {
	case networkName := <-entered:
		t.Fatalf("network %s entered programming while the shared Uplink was locked", networkName)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
}

func TestUplinkGatewayControllerDPUHostPublishesHostGatewayReady(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	reconciled := false

	if err := controller.ReconcileNetwork(network, func() error {
		reconciled = true
		return nil
	}); err != nil {
		t.Fatalf("failed to reconcile DPU-host gateway: %v", err)
	}
	if !reconciled {
		t.Fatal("expected DPU-host gateway reconciliation to run")
	}
	// The DPU-host reports its side of the gateway programming through its
	// own HostGatewayReady condition, leaving GatewayReady to the DPU.
	actions := client.Actions()
	if len(actions) == 0 {
		t.Fatal("expected DPU host to publish HostGatewayReady")
	}
	for _, action := range actions {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok {
			t.Fatalf("expected only patch actions, got %v", action)
		}
		payload := string(patch.GetPatch())
		if !strings.Contains(payload, uplinkv1alpha1.UplinkStateConditionHostGatewayReady) {
			t.Fatalf("expected DPU host to publish HostGatewayReady, got %s", payload)
		}
		if strings.Contains(payload, `"`+uplinkv1alpha1.UplinkStateConditionGatewayReady+`"`) {
			t.Fatalf("expected DPU host not to publish GatewayReady, got %s", payload)
		}
	}
}

func TestUplinkGatewayControllerRejectsMismatchedStateIdentity(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: "other-uplink",
			NodeName:   nodeName,
		},
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	controller := NewUplinkGatewayController(nodeName, client, uplinklisters.NewUplinkStateLister(indexer))

	err := controller.PrepareNetwork(uplinkGatewayNetInfo(t, "red", uplinkName))
	if err == nil || !strings.Contains(err.Error(), "reports uplinkName \"other-uplink\"") {
		t.Fatalf("expected identity validation error, got %v", err)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("expected no client actions, got %v", client.Actions())
	}
}
