// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
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
	controller, client, _ := newUplinkGatewayControllerWithIndexerForTest(t, uplinkName, nodeName)
	return controller, client
}

func newUplinkGatewayControllerWithIndexerForTest(
	t *testing.T,
	uplinkName, nodeName string,
) (*UplinkGatewayController, *uplinkfake.Clientset, cache.Indexer) {
	t.Helper()
	state := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	controller := NewUplinkGatewayController(nodeName, client, uplinklisters.NewUplinkStateLister(indexer))
	return controller, client, indexer
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
			IPAddresses: []uplinkv1alpha1.IPAddressCIDR{
				"192.0.2.10/24",
				"2001:db8::10/64",
			},
			DefaultGateways: []uplinkv1alpha1.IPAddress{
				"192.0.2.1",
				"2001:db8::1",
			},
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
	reconcile    func(*resolvedUplinkGateway) error
	withdraw     func() error
}

func (f *fakeUplinkGatewayNetworkLifecycle) reconcileUplinkGateway(
	resolved *resolvedUplinkGateway,
) error {
	f.reconciled = append(f.reconciled, resolved)
	if f.reconcile != nil {
		return f.reconcile(resolved)
	}
	return f.reconcileErr
}

func (f *fakeUplinkGatewayNetworkLifecycle) withdrawUplinkGateway() error {
	f.withdrawn++
	if f.withdraw != nil {
		return f.withdraw()
	}
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

func TestUplinkGatewayControllerRepublishesAfterCacheNotFound(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, indexer := newUplinkGatewayControllerWithIndexerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	resolved := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(resolved); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed network readiness: %v", err)
	}

	if err := indexer.Delete(newUplinkStateFixture(uplinkName, nodeName)); err != nil {
		t.Fatalf("failed to remove UplinkState from informer cache: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("expected cache NotFound to leave reconciliation retryable, got %v", err)
	}

	recreated := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), recreated.DeepCopy(), metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to recreate UplinkState in the API: %v", err)
	}
	if err := indexer.Add(recreated); err != nil {
		t.Fatalf("failed to recreate UplinkState in informer cache: %v", err)
	}
	if err := controller.ReconcileGatewayState(resolved.DeepCopy()); err != nil {
		t.Fatalf("failed to republish readiness for unchanged configuration: %v", err)
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured {
		t.Fatalf("expected recreated UplinkState readiness to be restored, got %#v", gatewayReady)
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

	ipv6Changed := changed.DeepCopy()
	ipv6Changed.Status.IPAddresses[1] = "2001:db8::20/64"
	ipv6Changed.Status.DefaultGateways[1] = "2001:db8::2"
	if err := controller.ReconcileGatewayState(ipv6Changed); err != nil {
		t.Fatalf("failed to reconcile changed IPv6 gateway state: %v", err)
	}
	if len(lifecycle.reconciled) != 3 ||
		lifecycle.reconciled[2].ipAddresses[1].String() != "2001:db8::20/64" ||
		lifecycle.reconciled[2].defaultGateways[1].String() != "2001:db8::2" {
		t.Fatalf("expected changed IPv6 state to reprogram the gateway, got %#v", lifecycle.reconciled)
	}
}

func TestResolvedUplinkGatewayConfigurationDoesNotCollideOnDelimiters(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	firstState := resolvedUplinkGatewayState(uplinkName, nodeName, "c")
	firstState.Status.HostInterfaceName = "a|b"
	secondState := resolvedUplinkGatewayState(uplinkName, nodeName, "b|c")
	secondState.Status.HostInterfaceName = "a"

	first, err := controller.resolvedGateway(firstState)
	if err != nil {
		t.Fatalf("failed to resolve first gateway configuration: %v", err)
	}
	second, err := controller.resolvedGateway(secondState)
	if err != nil {
		t.Fatalf("failed to resolve second gateway configuration: %v", err)
	}
	if resolvedUplinkGatewayConfiguration(first) == resolvedUplinkGatewayConfiguration(second) {
		t.Fatal("expected distinct interface and bridge names containing delimiters not to collide")
	}
}

func TestResolvedUplinkGatewayConfigurationIgnoresDPUBridgeOnHost(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost

	first := &resolvedUplinkGateway{
		hostInterfaceName: "eth0",
		bridgeName:        "br-dpu-a",
	}
	second := &resolvedUplinkGateway{
		hostInterfaceName: "eth0",
		bridgeName:        "br-dpu-b",
	}
	if resolvedUplinkGatewayConfiguration(first) != resolvedUplinkGatewayConfiguration(second) {
		t.Fatal("expected a DPU-owned bridge change not to alter DPU-host gateway configuration")
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

func TestUplinkGatewayControllerPreservesProgrammingOnActivationPublishFailure(t *testing.T) {
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

	// Model discovery changing after initial programming completed but before
	// AddNetwork registered its lifecycle callback.
	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to record changed gateway state: %v", err)
	}

	publishErr := errors.New("failed to publish gateway readiness")
	failNextApply := true
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !failNextApply {
			return false, nil, nil
		}
		failNextApply = false
		return true, nil, publishErr
	})

	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); !errors.Is(err, publishErr) {
		t.Fatalf("expected activation to return the publish failure, got %v", err)
	}
	if len(lifecycle.reconciled) != 1 || lifecycle.reconciled[0].bridgeName != "br-replacement" {
		t.Fatalf("expected activation to rebuild with br-replacement, got %#v", lifecycle.reconciled)
	}
	if lifecycle.withdrawn != 0 {
		t.Fatalf("expected publish failure not to withdraw working programming, got %d withdrawals", lifecycle.withdrawn)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	startupAbandoned := networkState.startupAbandoned
	retainedLifecycle := networkState.lifecycle
	controller.mutex.Unlock()
	if startupAbandoned || retainedLifecycle != lifecycle {
		t.Fatalf("expected failed Start to retain its cleanup owner, abandoned=%t lifecycle=%#v",
			startupAbandoned, retainedLifecycle)
	}

	if err := controller.RepublishGatewayCondition(uplinkName); err != nil {
		t.Fatalf("failed to retry gateway readiness publication: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected readiness after publish retry, got %#v", gatewayReady)
	}
	next := resolvedUplinkGatewayState(uplinkName, nodeName, "br-next")
	if err := controller.ReconcileGatewayState(next); err != nil {
		t.Fatalf("failed to reconcile retained lifecycle: %v", err)
	}
	if len(lifecycle.reconciled) != 2 || lifecycle.reconciled[1].bridgeName != "br-next" {
		t.Fatalf("expected retained lifecycle to reconcile br-next, got %#v", lifecycle.reconciled)
	}

	controller.DeleteGatewayState(uplinkName)
	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if !retired {
		t.Fatal("expected retained cleanup owner to leave a retired network tombstone")
	}
	if err := controller.DeleteNetwork(network, lifecycle.withdrawUplinkGateway); err != nil {
		t.Fatalf("failed to clean retained lifecycle after state deletion: %v", err)
	}
	controller.mutex.Lock()
	_, retired = controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retired {
		t.Fatal("expected cleanup to release the retired network tombstone")
	}
}

func TestUplinkGatewayControllerIgnoresActivationPublishNotFound(t *testing.T) {
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

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to record changed gateway state: %v", err)
	}

	notFound := apierrors.NewNotFound(
		uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinkstates").GroupResource(),
		uplinkutil.StateName(uplinkName, nodeName),
	)
	failNextApply := true
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !failNextApply {
			return false, nil, nil
		}
		failNextApply = false
		return true, nil, notFound
	})

	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("expected deleted UplinkState publication to be retried by discovery, got %v", err)
	}
	if lifecycle.withdrawn != 0 {
		t.Fatalf("expected NotFound not to withdraw working programming, got %d withdrawals", lifecycle.withdrawn)
	}
}

func TestUplinkGatewayControllerIgnoresFinalReconcilePublishNotFound(t *testing.T) {
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

	notFound := apierrors.NewNotFound(
		uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinkstates").GroupResource(),
		uplinkutil.StateName(uplinkName, nodeName),
	)
	patches := 0
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, notFound
		}
		return false, nil, nil
	})

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("expected deleted UplinkState publication to be retried by discovery, got %v", err)
	}
	if len(lifecycle.reconciled) != 1 || lifecycle.reconciled[0].bridgeName != "br-replacement" {
		t.Fatalf("expected changed state to remain programmed, got %#v", lifecycle.reconciled)
	}
	if lifecycle.withdrawn != 0 {
		t.Fatalf("expected NotFound not to withdraw working programming, got %d withdrawals", lifecycle.withdrawn)
	}
}

func TestUplinkGatewayControllerStartsWithFreshResolvedConfiguration(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-fresh"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	var programmedBridge string
	if err := controller.startNetwork(
		network,
		&fakeUplinkGatewayNetworkLifecycle{},
		func(resolved *resolvedUplinkGateway) error {
			if resolved == nil {
				t.Fatal("expected initial programming to receive resolved Uplink configuration")
			}
			programmedBridge = resolved.bridgeName
			return nil
		},
	); err != nil {
		t.Fatalf("failed to start gateway network: %v", err)
	}
	if programmedBridge != "br-fresh" {
		t.Fatalf("expected initial programming on br-fresh, got %q", programmedBridge)
	}
}

func TestUplinkGatewayControllerPreservesFailedInitialProgramming(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	programmingCause := errors.New("gateway interface is already assigned to another VRF")
	programmingErr := newUplinkGatewayError(
		uplinkv1alpha1.UplinkStateReasonConfigurationConflict,
		programmingCause,
	)
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	err := controller.StartNetwork(network, lifecycle, func() error { return programmingErr })
	if !errors.Is(err, programmingCause) {
		t.Fatalf("expected initial programming failure, got %v", err)
	}
	if lifecycle.withdrawn != 1 {
		t.Fatalf("expected partial initial programming to be cleaned, got %d withdrawals", lifecycle.withdrawn)
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonConfigurationConflict {
		t.Fatalf("expected failed startup to preserve its classified failure, got %#v", gatewayReady)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState == nil || networkState.phase != uplinkGatewayNetworkFailed ||
		networkState.reason != uplinkv1alpha1.UplinkStateReasonConfigurationConflict ||
		networkState.message != programmingCause.Error() || !networkState.startupAbandoned {
		t.Fatalf("expected failed startup to remain visible and cached for retry, got %#v", networkState)
	}

	retryLifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.StartNetwork(network, retryLifecycle, func() error { return nil }); err != nil {
		t.Fatalf("failed to retry initial programming: %v", err)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected retry to restore readiness, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerInvalidationRetriesFailedStartupCleanup(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	programmingErr := errors.New("partial initial programming failed")
	cleanupErr := errors.New("partial gateway cleanup failed")
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{withdrawErr: cleanupErr}
	if err := controller.StartNetwork(network, lifecycle, func() error { return programmingErr }); !errors.Is(err, programmingErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("expected programming and cleanup failures, got %v", err)
	}

	if err := controller.InvalidateGatewayState(uplinkName); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected invalidation cleanup failure, got %v", err)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState.cleanupLifecycle != lifecycle || networkState.startupAbandoned {
		t.Fatalf("expected failed invalidation cleanup to retain its owner, got %#v", networkState)
	}

	lifecycle.withdrawErr = nil
	if err := controller.InvalidateGatewayState(uplinkName); err != nil {
		t.Fatalf("failed to invalidate gateway state: %v", err)
	}
	if lifecycle.withdrawn != 3 {
		t.Fatalf("expected invalidation to retry failed startup cleanup, got %d withdrawals", lifecycle.withdrawn)
	}
	controller.mutex.Lock()
	networkState = controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState.cleanupLifecycle != nil || !networkState.startupAbandoned {
		t.Fatalf("expected successful invalidation cleanup to release its owner, got %#v", networkState)
	}
}

func TestUplinkGatewayControllerConfigurationChangeRetriesFailedStartupCleanup(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	programmingErr := errors.New("partial initial programming failed")
	cleanupErr := errors.New("partial gateway cleanup failed")
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{withdrawErr: cleanupErr}
	if err := controller.StartNetwork(network, lifecycle, func() error { return programmingErr }); !errors.Is(err, programmingErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("expected programming and cleanup failures, got %v", err)
	}

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected configuration cleanup failure, got %v", err)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	invalidated := controller.uplinks[uplinkName].invalidated
	controller.mutex.Unlock()
	if networkState.cleanupLifecycle != lifecycle || !invalidated {
		t.Fatalf("expected failed configuration cleanup to remain retryable, state=%#v invalidated=%t",
			networkState, invalidated)
	}

	lifecycle.withdrawErr = nil
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to reconcile changed gateway state: %v", err)
	}
	if lifecycle.withdrawn != 3 {
		t.Fatalf("expected configuration change to retry failed startup cleanup, got %d withdrawals", lifecycle.withdrawn)
	}

	retried := false
	replacement := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.StartNetwork(network, replacement, func() error {
		retried = true
		return nil
	}); err != nil {
		t.Fatalf("failed to retry gateway startup after old cleanup: %v", err)
	}
	if !retried {
		t.Fatal("expected gateway startup to run after old cleanup completed")
	}
}

func TestUplinkGatewayControllerRetainsProgrammingAfterStartPublicationFailure(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	publishErr := errors.New("failed to publish configured gateway")
	patches := 0
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, publishErr
		}
		return false, nil, nil
	})
	programmed := false
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	err := controller.StartNetwork(network, lifecycle, func() error {
		programmed = true
		return nil
	})
	if !errors.Is(err, publishErr) {
		t.Fatalf("expected readiness publication failure, got %v", err)
	}
	if !programmed || lifecycle.withdrawn != 0 {
		t.Fatalf("expected successful programming to remain, programmed=%t withdrawals=%d",
			programmed, lifecycle.withdrawn)
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected publication-only startup failure to remain pending, got %#v", gatewayReady)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState == nil || networkState.phase != uplinkGatewayNetworkReady ||
		networkState.startupAbandoned || networkState.lifecycle != lifecycle {
		t.Fatalf("expected publication-only startup failure to retain its cleanup owner, got %#v", networkState)
	}

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to reconcile retained lifecycle after configuration change: %v", err)
	}
	if len(lifecycle.reconciled) != 1 || lifecycle.reconciled[0].bridgeName != "br-replacement" {
		t.Fatalf("expected retained lifecycle to program br-replacement, got %#v", lifecycle.reconciled)
	}
}

func TestUplinkGatewayControllerPreservesLifecycleWhenRetryPublishFails(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	firstPublishErr := errors.New("failed to publish configured gateway")
	patches := 0
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, firstPublishErr
		}
		return false, nil, nil
	})
	retained := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.StartNetwork(network, retained, func() error { return nil }); !errors.Is(err, firstPublishErr) {
		t.Fatalf("expected initial publication failure, got %v", err)
	}

	retryPublishErr := errors.New("failed to publish retry pending status")
	failNextApply := true
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !failNextApply {
			return false, nil, nil
		}
		failNextApply = false
		return true, nil, retryPublishErr
	})
	retriedProgramming := false
	if err := controller.StartNetwork(network, &fakeUplinkGatewayNetworkLifecycle{}, func() error {
		retriedProgramming = true
		return nil
	}); !errors.Is(err, retryPublishErr) {
		t.Fatalf("expected retry publication failure, got %v", err)
	}
	if retriedProgramming {
		t.Fatal("expected failed pending publication to stop before programming")
	}

	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState.lifecycle != retained || networkState.startupAbandoned {
		t.Fatalf("expected retry failure to preserve the previous lifecycle, got %#v", networkState)
	}

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to reconcile changed gateway state: %v", err)
	}
	if len(retained.reconciled) != 1 || retained.reconciled[0].bridgeName != "br-replacement" {
		t.Fatalf("expected retained lifecycle to consume the changed state, got %#v", retained.reconciled)
	}

	controller.DeleteGatewayState(uplinkName)
	replacementCleanup := false
	if err := controller.DeleteNetwork(network, func() error {
		replacementCleanup = true
		return nil
	}); err != nil {
		t.Fatalf("failed to clean the retired lifecycle: %v", err)
	}
	if replacementCleanup || retained.withdrawn != 1 {
		t.Fatalf("expected retired cleanup to use the retained lifecycle, replacement=%t retained=%d",
			replacementCleanup, retained.withdrawn)
	}
}

func TestUplinkGatewayControllerCleansPriorLifecycleBeforeReplacement(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	publishErr := errors.New("failed to publish configured gateway")
	patches := 0
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, publishErr
		}
		return false, nil, nil
	})
	cleanupErr := errors.New("prior lifecycle cleanup failed")
	prior := &fakeUplinkGatewayNetworkLifecycle{withdrawErr: cleanupErr}
	if err := controller.StartNetwork(
		network,
		prior,
		func() error { return nil },
	); !errors.Is(err, publishErr) {
		t.Fatalf("expected status-only start failure, got %v", err)
	}

	replacementProgrammed := false
	replacement := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.StartNetwork(
		network,
		replacement,
		func() error {
			replacementProgrammed = true
			return nil
		},
	); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected prior cleanup failure, got %v", err)
	}
	if replacementProgrammed || replacement.withdrawn != 0 {
		t.Fatalf("expected replacement to wait for prior cleanup, programmed=%t withdrawn=%d",
			replacementProgrammed, replacement.withdrawn)
	}
	controller.mutex.Lock()
	networkState := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState.lifecycle != prior || networkState.startupAbandoned {
		t.Fatalf("expected failed cleanup to retain the prior lifecycle, got %#v", networkState)
	}

	prior.withdrawErr = nil
	if err := controller.StartNetwork(
		network,
		replacement,
		func() error {
			replacementProgrammed = true
			return nil
		},
	); err != nil {
		t.Fatalf("failed replacement startup: %v", err)
	}
	if prior.withdrawn != 2 || !replacementProgrammed {
		t.Fatalf("expected prior cleanup before replacement, cleanup=%d programmed=%t",
			prior.withdrawn, replacementProgrammed)
	}
	controller.mutex.Lock()
	networkState = controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if networkState.lifecycle != replacement || networkState.startupAbandoned {
		t.Fatalf("expected replacement to own the active lifecycle, got %#v", networkState)
	}
}

func TestUplinkGatewayControllerCleansFailedInitialProgramming(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	programmingErr := errors.New("partial initial programming failed")
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- controller.StartNetwork(network, lifecycle, func() error {
			close(entered)
			<-release
			return programmingErr
		})
	}()
	<-entered
	// Model discovery deleting the UplinkState before the failed network
	// controller can be registered with the network manager.
	controller.DeleteGatewayState(uplinkName)
	close(release)
	err := <-done
	if !errors.Is(err, programmingErr) {
		t.Fatalf("expected initial programming failure, got %v", err)
	}
	if lifecycle.withdrawn != 1 {
		t.Fatalf("expected partial initial programming to be cleaned, got %d withdrawals", lifecycle.withdrawn)
	}

	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	_, retainedOperationMutex := controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if retired || retainedOperationMutex {
		t.Fatalf("expected failed initial programming to release retired state, retired=%t mutex=%t",
			retired, retainedOperationMutex)
	}

	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to restore gateway state: %v", err)
	}
	retried := false
	if err := controller.StartNetwork(network, &fakeUplinkGatewayNetworkLifecycle{}, func() error {
		retried = true
		return nil
	}); err != nil {
		t.Fatalf("failed to retry initial programming: %v", err)
	}
	if !retried {
		t.Fatal("expected initial programming to retry after cleanup")
	}
}

func TestUplinkGatewayControllerRetainsRetiredStateAfterFailedStartupCleanup(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	programmingErr := errors.New("partial initial programming failed")
	cleanupErr := errors.New("partial gateway cleanup failed")
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{withdrawErr: cleanupErr}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- controller.StartNetwork(network, lifecycle, func() error {
			close(entered)
			<-release
			return programmingErr
		})
	}()
	<-entered
	controller.DeleteGatewayState(uplinkName)
	close(release)
	err := <-done
	if !errors.Is(err, programmingErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("expected programming and cleanup failures, got %v", err)
	}

	controller.mutex.Lock()
	retiredLifecycle, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	_, retainedOperationMutex := controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if !retired || retiredLifecycle != lifecycle || !retainedOperationMutex {
		t.Fatalf("expected failed cleanup to retain its owner and retirement state, retired=%t owner=%#v mutex=%t",
			retired, retiredLifecycle, retainedOperationMutex)
	}

	lifecycle.withdrawErr = nil
	replacementCleanup := false
	if err := controller.DeleteNetwork(network, func() error {
		replacementCleanup = true
		return nil
	}); err != nil {
		t.Fatalf("failed to retry retired cleanup: %v", err)
	}
	if replacementCleanup || lifecycle.withdrawn != 2 {
		t.Fatalf("expected cleanup retry to use the retained failed-start owner, replacement=%t retained=%d",
			replacementCleanup, lifecycle.withdrawn)
	}
	controller.mutex.Lock()
	_, retired = controller.retiredNetworks[uplinkName]
	_, retainedOperationMutex = controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if retired || retainedOperationMutex {
		t.Fatalf("expected successful retry to release retirement state, retired=%t mutex=%t",
			retired, retainedOperationMutex)
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

func TestUplinkGatewayControllerRetriesFailedActivationCleanupBeforeReplacement(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	state := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}

	controller.DeleteGatewayState(uplinkName)
	cleanupErr := errors.New("activation cleanup failed")
	owner := &fakeUplinkGatewayNetworkLifecycle{withdrawErr: cleanupErr}
	if err := controller.ActivateNetwork(network, owner); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected activation cleanup failure, got %v", err)
	}
	controller.mutex.Lock()
	retiredOwner := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retiredOwner != owner {
		t.Fatalf("expected failed activation cleanup owner to be retained, got %#v", retiredOwner)
	}

	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to recreate gateway state: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected retired cleanup to keep replacement readiness pending, got %#v", gatewayReady)
	}
	if err := controller.PrepareNetwork(network); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected retired cleanup failure, got %v", err)
	}
	controller.mutex.Lock()
	retiredOwner = controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retiredOwner != owner {
		t.Fatalf("expected retired cleanup failure to preserve its owner, got %#v", retiredOwner)
	}

	owner.withdrawErr = nil
	if err := controller.PrepareNetwork(network); err != nil {
		t.Fatalf("failed to clean retired programming before replacement: %v", err)
	}
	if owner.withdrawn != 3 {
		t.Fatalf("expected replacement preparation to retry retired cleanup, got %d withdrawals", owner.withdrawn)
	}
	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retired {
		t.Fatal("expected successful cleanup to release the retired lifecycle")
	}

	retried := false
	if err := controller.StartNetwork(network, &fakeUplinkGatewayNetworkLifecycle{}, func() error {
		retried = true
		return nil
	}); err != nil {
		t.Fatalf("failed replacement gateway startup: %v", err)
	}
	if !retried {
		t.Fatal("expected replacement gateway programming to run")
	}
}

func TestUplinkGatewayControllerPublishesReadyAfterRetiredTerminalCleanup(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	state := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate gateway lifecycle: %v", err)
	}

	controller.DeleteGatewayState(uplinkName)
	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to recreate gateway state: %v", err)
	}
	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse {
		t.Fatalf("expected retired lifecycle to keep gateway readiness false, got %#v", gatewayReady)
	}

	fallbackCleanup := false
	if err := controller.DeleteNetwork(network, func() error {
		fallbackCleanup = true
		return nil
	}); err != nil {
		t.Fatalf("failed terminal cleanup of retired lifecycle: %v", err)
	}
	if fallbackCleanup || lifecycle.withdrawn != 1 {
		t.Fatalf("expected terminal cleanup to use the retired owner, fallback=%t withdrawals=%d",
			fallbackCleanup, lifecycle.withdrawn)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected readiness after retired terminal cleanup, got %#v", gatewayReady)
	}
}

func TestUplinkGatewayControllerRetriesAfterFailedActivationAndStateDeletion(t *testing.T) {
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

	changed := resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement")
	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to record changed gateway state: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{
		reconcileErr: errors.New("replacement programming failed"),
	}
	if err := controller.ActivateNetwork(network, lifecycle); err == nil {
		t.Fatal("expected activation to fail")
	}
	if lifecycle.withdrawn != 1 {
		t.Fatalf("expected failed activation to clean up programming, got %d withdrawals", lifecycle.withdrawn)
	}

	controller.DeleteGatewayState(uplinkName)
	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	controller.mutex.Unlock()
	if retired {
		t.Fatal("expected abandoned startup not to leave a retired network tombstone")
	}

	if err := controller.ReconcileGatewayState(changed); err != nil {
		t.Fatalf("failed to restore gateway state for retry: %v", err)
	}
	retried := false
	if err := controller.ReconcileNetwork(network, func() error {
		retried = true
		return nil
	}); err != nil {
		t.Fatalf("failed to retry network reconciliation: %v", err)
	}
	if !retried {
		t.Fatal("expected network startup to retry after the failed activation")
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

func TestUplinkGatewayControllerTerminalDeleteWinsConfigurationRace(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileGatewayState(
		resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink"),
	); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}

	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{
		withdraw: func() error {
			close(cleanupEntered)
			<-cleanupRelease
			return nil
		},
	}
	if err := controller.StartNetwork(network, lifecycle, func() error { return nil }); err != nil {
		t.Fatalf("failed to start network lifecycle: %v", err)
	}

	callerCleanup := false
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- controller.DeleteNetwork(network, func() error {
			callerCleanup = true
			return nil
		})
	}()
	<-cleanupEntered

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- controller.ReconcileGatewayState(
			resolvedUplinkGatewayState(uplinkName, nodeName, "br-replacement"),
		)
	}()
	if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			controller.mutex.Lock()
			defer controller.mutex.Unlock()
			return controller.uplinks[uplinkName].invalidated, nil
		}); err != nil {
		t.Fatalf("configuration reconciliation did not overlap terminal cleanup: %v", err)
	}

	close(cleanupRelease)
	if err := <-deleteDone; err != nil {
		t.Fatalf("failed terminal network cleanup: %v", err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatalf("failed concurrent configuration reconciliation: %v", err)
	}
	if callerCleanup {
		t.Fatal("expected terminal deletion to use the registered lifecycle cleanup owner")
	}
	if len(lifecycle.reconciled) != 0 {
		t.Fatalf("expected deleted lifecycle not to be reprogrammed, got %#v", lifecycle.reconciled)
	}
	controller.mutex.Lock()
	_, found := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	_, mapped := controller.uplinkByNetworkName[network.GetNetworkName()]
	controller.mutex.Unlock()
	if found || mapped {
		t.Fatalf("expected terminal cleanup to remove the lifecycle, found=%t mapped=%t", found, mapped)
	}
}

func TestUplinkGatewayControllerRetiresAbandonedStartupDuringDelete(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed gateway network state: %v", err)
	}
	controller.mutex.Lock()
	uplinkState := controller.uplinks[uplinkName]
	uplinkState.networks[network.GetNetworkName()].startupAbandoned = true
	controller.mutex.Unlock()

	cleanupErr := errors.New("gateway cleanup failed")
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- controller.DeleteNetwork(network, func() error {
			close(cleanupEntered)
			<-cleanupRelease
			return cleanupErr
		})
	}()
	<-cleanupEntered
	controller.abandonNetworkStartup(network, uplinkState, nil)
	controller.DeleteGatewayState(uplinkName)

	controller.mutex.Lock()
	_, retired := controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	_, retainedOperationMutex := controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if !retired || !retainedOperationMutex {
		t.Fatalf("expected in-flight cleanup to retain retirement state, retired=%t mutex=%t",
			retired, retainedOperationMutex)
	}

	close(cleanupRelease)
	if err := <-deleteDone; !errors.Is(err, cleanupErr) {
		t.Fatalf("expected cleanup failure, got %v", err)
	}
	controller.mutex.Lock()
	_, retired = controller.retiredNetworks[uplinkName][network.GetNetworkName()]
	_, retainedOperationMutex = controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if !retired || !retainedOperationMutex {
		t.Fatalf("expected failed cleanup to retain retirement state, retired=%t mutex=%t",
			retired, retainedOperationMutex)
	}

	if err := controller.DeleteNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to retry retired cleanup: %v", err)
	}
	controller.mutex.Lock()
	_, retired = controller.retiredNetworks[uplinkName]
	_, retainedOperationMutex = controller.operationMutexes[uplinkName]
	controller.mutex.Unlock()
	if retired || retainedOperationMutex {
		t.Fatalf("expected cleanup retry to release retirement state, retired=%t mutex=%t",
			retired, retainedOperationMutex)
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
	if err := wait.PollUntilContextTimeout(
		context.Background(), time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			controller.mutex.Lock()
			defer controller.mutex.Unlock()
			return controller.uplinks[uplinkName].invalidated, nil
		},
	); err != nil {
		t.Fatalf("gateway state was not invalidated before reconciliation resumed: %v", err)
	}
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

func TestUplinkGatewayControllerRejectsStaleResolvedStateUnderOperationLock(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate gateway lifecycle: %v", err)
	}

	controller.mutex.Lock()
	uplinkState := controller.uplinks[uplinkName]
	controller.mutex.Unlock()
	uplinkState.operationMutex.Lock()
	operationLocked := true
	defer func() {
		if operationLocked {
			uplinkState.operationMutex.Unlock()
		}
	}()

	var inputsCurrent atomic.Bool
	inputsCurrent.Store(true)
	inputChecked := make(chan struct{})
	var inputCheckOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- controller.ReconcileGatewayStateIfCurrent(
			resolvedUplinkGatewayState(uplinkName, nodeName, "br-stale"),
			func() bool {
				current := inputsCurrent.Load()
				inputCheckOnce.Do(func() { close(inputChecked) })
				return current
			},
		)
	}()
	select {
	case <-inputChecked:
	case <-time.After(time.Second):
		t.Fatal("resolved state did not validate its discovery inputs")
	}

	inputsCurrent.Store(false)
	uplinkState.operationMutex.Unlock()
	operationLocked = false
	if err := <-done; err != nil {
		t.Fatalf("stale resolved state returned an error: %v", err)
	}
	if len(lifecycle.reconciled) != 0 {
		t.Fatalf("stale resolved state restored gateway programming: %#v", lifecycle.reconciled)
	}
	controller.mutex.Lock()
	configuration := uplinkState.configuration
	controller.mutex.Unlock()
	if configuration.bridgeName != "br-uplink" {
		t.Fatalf("stale resolved state changed the active configuration: %#v", configuration)
	}
}

func TestUplinkGatewayControllerRejectsStaleInvalidationUnderOperationLock(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	initial := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")
	if err := controller.ReconcileGatewayState(initial); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate gateway lifecycle: %v", err)
	}

	controller.mutex.Lock()
	uplinkState := controller.uplinks[uplinkName]
	controller.mutex.Unlock()
	uplinkState.operationMutex.Lock()
	operationLocked := true
	defer func() {
		if operationLocked {
			uplinkState.operationMutex.Unlock()
		}
	}()

	var inputsCurrent atomic.Bool
	inputsCurrent.Store(true)
	inputChecked := make(chan struct{})
	var inputCheckOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- controller.InvalidateGatewayStateIfCurrent(
			uplinkName,
			func() bool {
				current := inputsCurrent.Load()
				inputCheckOnce.Do(func() { close(inputChecked) })
				return current
			},
		)
	}()
	select {
	case <-inputChecked:
	case <-time.After(time.Second):
		t.Fatal("invalidation did not validate its discovery inputs")
	}

	inputsCurrent.Store(false)
	uplinkState.operationMutex.Unlock()
	operationLocked = false
	if err := <-done; err != nil {
		t.Fatalf("stale invalidation returned an error: %v", err)
	}
	if lifecycle.withdrawn != 0 {
		t.Fatalf("stale invalidation withdrew current gateway programming %d time(s)", lifecycle.withdrawn)
	}
}

func TestUplinkGatewayControllerSkipsReconcileAfterInvalidation(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	state := resolvedUplinkGatewayState(uplinkName, nodeName, "br-uplink")

	if err := controller.ReconcileGatewayState(state); err != nil {
		t.Fatalf("failed to seed gateway state: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed initial gateway programming: %v", err)
	}
	lifecycle := &fakeUplinkGatewayNetworkLifecycle{}
	if err := controller.ActivateNetwork(network, lifecycle); err != nil {
		t.Fatalf("failed to activate gateway lifecycle: %v", err)
	}
	if err := controller.InvalidateGatewayState(uplinkName); err != nil {
		t.Fatalf("failed to invalidate gateway lifecycle: %v", err)
	}

	reconciled := false
	if err := controller.ReconcileNetwork(network, func() error {
		reconciled = true
		return nil
	}); err != nil {
		t.Fatalf("failed to suppress reconciliation after invalidation: %v", err)
	}
	if reconciled {
		t.Fatal("expected invalidated gateway lifecycle not to run gateway reconciliation")
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

func TestUplinkGatewayControllerDPUHostInvalidatedConditionType(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _ := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)

	controller.mutex.Lock()
	uplinkState := controller.newUplinkGatewayStateLocked(uplinkName)
	uplinkState.invalidated = true
	uplinkState.networks[network.GetNetworkName()] = &uplinkGatewayNetworkState{
		phase: uplinkGatewayNetworkReady,
	}
	controller.uplinks[uplinkName] = uplinkState
	controller.mutex.Unlock()

	condition, found := controller.gatewayCondition(uplinkName, uplinkState)
	if !found {
		t.Fatal("expected an invalidated DPU-host gateway condition")
	}
	if condition.Type != uplinkv1alpha1.UplinkStateConditionHostGatewayReady {
		t.Fatalf("expected HostGatewayReady, got %q", condition.Type)
	}
}

func TestUplinkGatewayControllerCleanupContinuesAfterPublishFailure(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed network readiness: %v", err)
	}

	publishErr := errors.New("failed to publish pending cleanup status")
	failNextApply := true
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !failNextApply {
			return false, nil, nil
		}
		failNextApply = false
		return true, nil, publishErr
	})
	cleanupErr := errors.New("gateway cleanup failed")
	cleanupCalled := false
	err := controller.DeleteNetwork(network, func() error {
		cleanupCalled = true
		return cleanupErr
	})
	if !cleanupCalled {
		t.Fatal("expected cleanup to continue after status publication failed")
	}
	if !errors.Is(err, publishErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("expected both publication and cleanup errors, got %v", err)
	}
}

func TestUplinkGatewayControllerRetriesFinalDeletePublication(t *testing.T) {
	prepareUplinkGatewayControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client := newUplinkGatewayControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed network readiness: %v", err)
	}

	publishErr := errors.New("failed to publish completed cleanup status")
	patches := 0
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, publishErr
		}
		return false, nil, nil
	})
	cleanupCalls := 0
	cleanup := func() error {
		cleanupCalls++
		return nil
	}
	if err := controller.DeleteNetwork(network, cleanup); !errors.Is(err, publishErr) {
		t.Fatalf("expected final readiness publication failure, got %v", err)
	}
	controller.mutex.Lock()
	_, found := controller.uplinks[uplinkName].networks[network.GetNetworkName()]
	controller.mutex.Unlock()
	if found {
		t.Fatal("expected successful cleanup to remove the network despite publication failure")
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse {
		t.Fatalf("expected pending readiness before publication retry, got %#v", gatewayReady)
	}
	if err := controller.DeleteNetwork(network, cleanup); err != nil {
		t.Fatalf("failed to retry final readiness publication: %v", err)
	}
	if cleanupCalls != 2 {
		t.Fatalf("expected idempotent cleanup on publication retry, got %d calls", cleanupCalls)
	}
	gatewayReady, _ = getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured {
		t.Fatalf("expected final readiness after publication retry, got %#v", gatewayReady)
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
