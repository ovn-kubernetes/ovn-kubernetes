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

func resolvedUplinkGatewayState(uplinkName, nodeName, bridgeName string) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "eth0",
			OVSBridge:         &uplinkv1alpha1.OVSBridgeStatus{Name: bridgeName},
			MACAddress:        "0a:58:0a:80:00:01",
			IPAddresses:       []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"},
			DefaultGateways:   []uplinkv1alpha1.IPAddress{"192.0.2.1"},
			Conditions: []metav1.Condition{{
				Type:   uplinkv1alpha1.UplinkStateConditionResolved,
				Status: metav1.ConditionTrue,
				Reason: uplinkv1alpha1.UplinkStateReasonResolved,
			}},
		},
	}
}

type fakeUplinkGatewayNetworkLifecycle struct {
	reconciled   []*resolvedUplinkGateway
	withdrawn    int
	reconcileErr error
	withdrawErr  error
}

func (f *fakeUplinkGatewayNetworkLifecycle) reconcileUplinkGateway(
	resolved *resolvedUplinkGateway,
) error {
	f.reconciled = append(f.reconciled, resolved)
	return f.reconcileErr
}

func (f *fakeUplinkGatewayNetworkLifecycle) withdrawUplinkGateway() error {
	f.withdrawn++
	return f.withdrawErr
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
	if err := controller.InvalidateGatewayState(uplinkName); err != nil {
		t.Fatalf("failed to invalidate gateway state: %v", err)
	}

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
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected invalidated GatewayReady to remain pending, got %#v", gatewayReady)
	}

	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	phase := networkState.phase
	controller.mutex.Unlock()
	if phase != uplinkGatewayNetworkPending {
		t.Fatalf("expected cached network readiness to be pending, got %q", phase)
	}

}

func TestUplinkGatewayControllerReconcilesResolvedStateLifecycle(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	state := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")

	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to seed resolved gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile initial network: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate network lifecycle: %v", err)
	}

	if err := controller.InvalidateGatewayState(uplinkName); err != nil {
		t.Fatalf("failed to invalidate gateway state: %v", err)
	}
	if lifecycle.withdrawn != 1 {
		t.Fatalf("expected one gateway withdrawal, got %d", lifecycle.withdrawn)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse {
		t.Fatalf("expected readiness to remain false after withdrawal, got %#v", gatewayReady)
	}

	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to reconcile recreated gateway state: %v", err)
	}
	if len(lifecycle.reconciled) != 1 || lifecycle.reconciled[0].bridgeName != "br-uplink" {
		t.Fatalf("expected fresh gateway programming on br-uplink, got %#v", lifecycle.reconciled)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected readiness after fresh programming, got %#v", gatewayReady)
	}

	// An out-of-band recreation with the same resolved configuration only
	// needs status recovery; it must not disturb working gateway programming.
	if err := controller.ReconcileGatewayState(state.DeepCopy()); err != nil {
		t.Fatalf("failed to reconcile unchanged gateway state: %v", err)
	}
	if len(lifecycle.reconciled) != 1 {
		t.Fatalf("expected unchanged state not to reprogram the gateway, got %d reconciles",
			len(lifecycle.reconciled))
	}

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to reconcile changed gateway state: %v", err)
	}
	if len(lifecycle.reconciled) != 2 || lifecycle.reconciled[1].bridgeName != "br-replacement" {
		t.Fatalf("expected changed state to reprogram br-replacement, got %#v", lifecycle.reconciled)
	}
}

func TestUplinkGatewayControllerClosesActivationRace(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed initial gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}

	// Model discovery changing after initial programming completed but before
	// AddNetwork registered its lifecycle callback.
	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to record changed gateway state: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate against changed gateway state: %v", err)
	}
	if len(lifecycle.reconciled) != 1 || lifecycle.reconciled[0].bridgeName != "br-replacement" {
		t.Fatalf("expected activation to rebuild with br-replacement, got %#v", lifecycle.reconciled)
	}
}

func TestUplinkGatewayControllerCleansProgrammingWhenActivationLosesState(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed initial gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}

	// Model Uplink deletion after initial programming but before AddNetwork
	// registers the reusable gateway lifecycle.
	controller.DeleteGatewayState(uplinkName)
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err == nil {
		t.Fatal("expected activation to fail after gateway state deletion")
	}
	if lifecycle.withdrawn != 1 {
		t.Fatalf("expected programmed gateway to be withdrawn, got %d calls", lifecycle.withdrawn)
	}
	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retired {
		t.Fatal("expected failed activation to release the retired network lifecycle")
	}
}

func TestUplinkGatewayControllerRetriesFailedStateReconfiguration(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")

	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed initial gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate network lifecycle: %v", err)
	}

	expectedErr := errors.New("bridge replacement failed")
	lifecycle.reconcileErr = expectedErr
	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); !errors.Is(err, expectedErr) {
		t.Fatalf("expected reconfiguration failure, got %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
		t.Fatalf("expected failed gateway readiness, got %#v", gatewayReady)
	}

	lifecycle.reconcileErr = nil
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to retry gateway reconfiguration: %v", err)
	}
	if len(lifecycle.reconciled) != 2 {
		t.Fatalf("expected failed reconfiguration to be retried, got %d calls", len(lifecycle.reconciled))
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected readiness after successful retry, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerDeletesNetworkWithoutUplinkState(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	controller := NewUplinkGatewayController(
		nodeName,
		uplinkfake.NewSimpleClientset(),
		uplinklisters.NewUplinkStateLister(indexer),
	)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	controller.mutex.Lock()
	uplinkState := controller.newUplinkGatewayStateLocked(uplinkName)
	uplinkState.networks[network.GetNetworkName()] = &uplinkGatewayNetworkState{
		phase: uplinkGatewayNetworkReady,
	}
	controller.uplinks[uplinkName] = uplinkState
	controller.uplinkByNetworkName[network.GetNetworkName()] = uplinkName
	controller.mutex.Unlock()

	cleaned := false
	if err := controller.DeleteNetwork(network, func() error {
		cleaned = true
		return nil
	}); err != nil {
		t.Fatalf("failed to delete network after UplinkState removal: %v", err)
	}
	if !cleaned {
		t.Fatal("expected gateway cleanup despite missing UplinkState")
	}
	controller.mutex.Lock()
	_, found := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if found {
		t.Fatal("expected deleted network to be removed from the gateway cache")
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
	cleaned := false
	if err := controller.DeleteNetwork(network, func() error {
		cleaned = true
		return nil
	}); err != nil {
		t.Fatalf("failed late network cleanup: %v", err)
	}
	if !cleaned {
		t.Fatal("expected late network cleanup after Uplink cache deletion")
	}
	controller.mutex.Lock()
	_, uplinkFound = controller.uplinks[uplinkName]
	controller.mutex.Unlock()
	if uplinkFound {
		t.Fatal("expected late network cleanup not to recreate the deleted Uplink cache")
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

func TestUplinkGatewayControllerSerializesCleanupAfterCacheRemoval(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	entered := make(chan struct{})
	release := make(chan struct{})
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- controller.ReconcileNetwork(network, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	controller.DeleteGatewayState(uplinkName)
	reconciledAfterRetirement := false
	if err := controller.ReconcileNetwork(network, func() error {
		reconciledAfterRetirement = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "awaiting cleanup") {
		t.Fatalf("expected retired lifecycle reconciliation to be rejected, got %v", err)
	}
	if reconciledAfterRetirement {
		t.Fatal("expected retired lifecycle not to run another reconciliation")
	}

	cleanupEntered := make(chan struct{})
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- controller.DeleteNetwork(network, func() error {
			close(cleanupEntered)
			return nil
		})
	}()
	select {
	case <-cleanupEntered:
		t.Fatal("late cleanup ran concurrently with the in-flight reconciliation")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("failed in-flight reconciliation: %v", err)
	}
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("late cleanup did not run after reconciliation completed")
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("failed late cleanup: %v", err)
	}

	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName]
	_, operationMutex := controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if retired || operationMutex {
		t.Fatalf("expected retired serialization state to be released, retired=%t mutex=%t",
			retired, operationMutex)
	}
}

func TestUplinkGatewayControllerCompletesCleanupRacingCacheRemoval(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}

	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- controller.DeleteNetwork(network, func() error {
			close(cleanupEntered)
			<-cleanupRelease
			return nil
		})
	}()
	<-cleanupEntered
	controller.DeleteGatewayState(uplinkName)
	close(cleanupRelease)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("failed cleanup racing cache removal: %v", err)
	}

	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName]
	_, operationMutex := controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if retired || operationMutex {
		t.Fatalf("expected completed cleanup to release retired state, retired=%t mutex=%t",
			retired, operationMutex)
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

	invalidated := make(chan error, 1)
	go func() {
		invalidated <- controller.InvalidateGatewayState(uplinkName)
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("failed to finish invalidated reconciliation: %v", err)
	}
	if err := <-invalidated; err != nil {
		t.Fatalf("failed to invalidate gateway state: %v", err)
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

func TestUplinkGatewayControllerDPUHostDoesNotPublishGatewayReady(t *testing.T) {
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
	if len(client.Actions()) != 0 {
		t.Fatalf("expected DPU host not to publish GatewayReady, got actions %v", client.Actions())
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
