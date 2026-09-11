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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinkinformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newUplinkStateFixture(
	uplinkName, nodeName string, conditions ...metav1.Condition,
) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: uplinkutil.StateName(uplinkName, nodeName),
			UID:  k8stypes.UID(uplinkName + "-" + nodeName + "-uid"),
		},
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

func newUplinkStateGatewayStatusControllerForTest(
	t *testing.T,
	uplinkName, nodeName string,
) (*UplinkStateGatewayStatusController, *uplinkfake.Clientset, cache.Indexer) {
	t.Helper()
	state := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(client, 0)
	informer := informerFactory.K8s().V1alpha1().UplinkStates()
	sharedInformer := informer.Informer()
	informerStop := make(chan struct{})
	informerFactory.Start(informerStop)
	if !cache.WaitForCacheSync(informerStop, sharedInformer.HasSynced) {
		close(informerStop)
		t.Fatal("failed to sync UplinkState informer")
	}
	controller := NewUplinkStateGatewayStatusController(
		nodeName, client, informer)
	if err := controller.Start(); err != nil {
		close(informerStop)
		t.Fatalf("failed to start gateway status controller: %v", err)
	}
	t.Cleanup(func() {
		controller.Stop()
		close(informerStop)
	})
	return controller, client, sharedInformer.GetIndexer()
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

func prepareUplinkStateGatewayStatusControllerTest(t *testing.T) {
	t.Helper()
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true
}

type countingUplinkStateLister struct {
	uplinklisters.UplinkStateLister
	gets atomic.Int32
}

func (l *countingUplinkStateLister) Get(name string) (*uplinkv1alpha1.UplinkState, error) {
	l.gets.Add(1)
	return l.UplinkStateLister.Get(name)
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
	return meta.FindStatusCondition(
			state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionGatewayReady),
		meta.FindStatusCondition(
			state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionResolved)
}

func waitForUplinkGatewayCondition(
	t *testing.T,
	client *uplinkfake.Clientset,
	uplinkName, nodeName string,
	matches func(*metav1.Condition) bool,
) (*metav1.Condition, *metav1.Condition) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		gatewayReady, resolved := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
		if matches(gatewayReady) {
			return gatewayReady, resolved
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for GatewayReady, last condition: %#v", gatewayReady)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForUplinkGatewayPatches(
	t *testing.T,
	client *uplinkfake.Clientset,
	minimum int,
) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		patches := 0
		for _, action := range client.Actions() {
			if _, ok := action.(clienttesting.PatchAction); ok {
				patches++
			}
		}
		if patches >= minimum {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d UplinkState patches; got %d", minimum, patches)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUplinkStateGatewayStatusControllerAggregatesActiveNetworks(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	red := uplinkGatewayNetInfo(t, "red", uplinkName)
	blue := uplinkGatewayNetInfo(t, "blue", uplinkName)

	if err := controller.SyncNetworks(red, blue); err != nil {
		t.Fatalf("failed to sync networks: %v", err)
	}
	gatewayReady, resolved := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionFalse &&
				strings.Contains(condition.Message, "2 of 2 active CUDN(s)")
		},
	)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("unexpected pending GatewayReady condition: %#v", gatewayReady)
	}
	if !strings.Contains(gatewayReady.Message, "2 of 2 active CUDN(s)") {
		t.Fatalf("unexpected pending message: %q", gatewayReady.Message)
	}
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("expected Resolved to remain true, got %#v", resolved)
	}

	if err := controller.ReconcileNetwork(red, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile red: %v", err)
	}
	gatewayReady, _ = waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionFalse
		},
	)
	if gatewayReady.Status != metav1.ConditionFalse {
		t.Fatalf("expected blue to keep readiness false, got %#v", gatewayReady)
	}

	if err := controller.ReconcileNetwork(blue, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile blue: %v", err)
	}
	gatewayReady, _ = waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)
	if gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected aggregate readiness true, got %#v", gatewayReady)
	}
}

func TestUplinkStateGatewayStatusControllerRemovesFailedNetworksAfterDeletion(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	red := uplinkGatewayNetInfo(t, "red", uplinkName)
	blue := uplinkGatewayNetInfo(t, "blue", uplinkName)

	for _, network := range []util.NetInfo{red, blue} {
		err := controller.ReconcileNetwork(network, func() error {
			return errors.New("failed to update isolation rules")
		})
		if err == nil {
			t.Fatalf("expected %s programming to fail", network.GetNetworkName())
		}
	}
	gatewayReady, _ := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && strings.Contains(
				condition.Message, "2 of 2 active CUDN(s)")
		},
	)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		!strings.Contains(gatewayReady.Message, "2 of 2 active CUDN(s)") {
		t.Fatalf("expected both failed networks in GatewayReady, got %#v", gatewayReady)
	}

	cleanupErr := errors.New("failed to clean partial gateway programming")
	if err := controller.DeleteNetwork(red, func() error { return cleanupErr }); !errors.Is(err, cleanupErr) {
		t.Fatalf("expected failed cleanup to remain retryable, got %v", err)
	}
	gatewayReady, _ = waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && strings.Contains(
				condition.Message, "2 of 2 active CUDN(s)")
		},
	)
	if gatewayReady == nil || !strings.Contains(
		gatewayReady.Message, "2 of 2 active CUDN(s)",
	) {
		t.Fatalf("failed cleanup prematurely removed the network: %#v", gatewayReady)
	}

	if err := controller.DeleteNetwork(red, func() error { return nil }); err != nil {
		t.Fatalf("failed to retry red deletion: %v", err)
	}
	gatewayReady, _ = waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && strings.Contains(
				condition.Message, "1 of 1 active CUDN(s)")
		},
	)
	if gatewayReady == nil || !strings.Contains(
		gatewayReady.Message, "1 of 1 active CUDN(s)",
	) || strings.Contains(gatewayReady.Message, "red=") {
		t.Fatalf("successful cleanup did not remove red: %#v", gatewayReady)
	}
	if err := controller.DeleteNetwork(blue, func() error { return nil }); err != nil {
		t.Fatalf("failed to delete blue: %v", err)
	}
	gatewayReady, _ = waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue &&
				condition.Message == "No active CUDNs require Uplink gateway programming"
		},
	)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured ||
		gatewayReady.Message != "No active CUDNs require Uplink gateway programming" {
		t.Fatalf("deleted failed networks remained in GatewayReady: %#v", gatewayReady)
	}
}

func TestUplinkStateGatewayStatusControllerRepublishesWipedCondition(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)

	recreated := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), recreated, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to wipe GatewayReady: %v", err)
	}
	gatewayReady, resolved := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected restored GatewayReady, got %#v", gatewayReady)
	}
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("expected Resolved to remain true, got %#v", resolved)
	}
}

func TestUplinkStateGatewayStatusControllerRepublishesDPUHostCondition(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(
		t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile network: %v", err)
	}
	waitForUplinkGatewayPatches(t, client, 1)

	peerCondition := metav1.Condition{
		Type:   uplinkv1alpha1.UplinkStateConditionGatewayReady,
		Status: metav1.ConditionTrue,
		Reason: uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
	}
	wiped := newUplinkStateFixture(
		uplinkName, nodeName, resolvedTrueCondition(), peerCondition)
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), wiped, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to wipe HostGatewayReady: %v", err)
	}

	err := wait.PollUntilContextTimeout(
		context.Background(), 10*time.Millisecond, 3*time.Second, true,
		func(context.Context) (bool, error) {
			state, err := client.K8sV1alpha1().UplinkStates().Get(
				context.Background(), uplinkutil.StateName(uplinkName, nodeName),
				metav1.GetOptions{},
			)
			if err != nil {
				return false, err
			}
			hostReady := meta.FindStatusCondition(
				state.Status.Conditions,
				uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
			)
			gatewayReady := meta.FindStatusCondition(
				state.Status.Conditions,
				uplinkv1alpha1.UplinkStateConditionGatewayReady,
			)
			return hostReady != nil && hostReady.Status == metav1.ConditionTrue &&
				gatewayReady != nil && gatewayReady.Status == metav1.ConditionTrue, nil
		},
	)
	if err != nil {
		t.Fatalf("failed to restore HostGatewayReady without disturbing GatewayReady: %v", err)
	}
}

func TestUplinkStateGatewayStatusControllerRepublishesNoActiveNetworkConditionAfterRecreation(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	if err := controller.SyncNetworks(); err != nil {
		t.Fatalf("failed to publish empty network readiness: %v", err)
	}
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)

	if err := client.K8sV1alpha1().UplinkStates().Delete(
		context.Background(), uplinkutil.StateName(uplinkName, nodeName),
		metav1.DeleteOptions{},
	); err != nil {
		t.Fatalf("failed to delete UplinkState: %v", err)
	}
	recreated := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	recreated.UID = "recreated-uid"
	if _, err := client.K8sV1alpha1().UplinkStates().Create(
		context.Background(), recreated, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("failed to recreate UplinkState: %v", err)
	}
	gatewayReady, _ := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue &&
				condition.Message == "No active CUDNs require Uplink gateway programming"
		},
	)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionTrue ||
		gatewayReady.Message != "No active CUDNs require Uplink gateway programming" {
		t.Fatalf("expected restored empty-network GatewayReady, got %#v", gatewayReady)
	}
}

func TestUplinkStateGatewayStatusControllerDeleteDoesNotBlockOnConditionPublisher(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed gateway readiness: %v", err)
	}
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)

	patchEntered := make(chan struct{}, 2)
	releasePatch := make(chan struct{})
	client.PrependReactor(
		"patch",
		"uplinkstates",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			patchEntered <- struct{}{}
			<-releasePatch
			return true, newUplinkStateFixture(uplinkName, nodeName), nil
		},
	)
	wiped := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	if _, err := client.K8sV1alpha1().UplinkStates().Update(
		context.Background(), wiped, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("failed to trigger GatewayReady republication: %v", err)
	}
	select {
	case <-patchEntered:
	case <-time.After(time.Second):
		t.Fatal("gateway condition publisher did not enter the API call")
	}

	deleteDone := make(chan struct{})
	go func() {
		controller.onUplinkStateDelete(wiped)
		close(deleteDone)
	}()
	select {
	case <-deleteDone:
	case <-time.After(100 * time.Millisecond):
		close(releasePatch)
		<-deleteDone
		t.Fatal("UplinkState delete blocked on the in-flight status request")
	}
	close(releasePatch)

	controller.enqueueGatewayCondition(uplinkName)
	time.Sleep(2 * uplinkGatewayStatusBatchDelay)
	select {
	case <-patchEntered:
		t.Fatal("deleted Uplink readiness published another condition")
	default:
	}
}

func TestUplinkStateGatewayStatusControllerDoesNotSerializeIndependentNetworks(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	red := uplinkGatewayNetInfo(t, "red", uplinkName)
	blue := uplinkGatewayNetInfo(t, "blue", uplinkName)
	if err := controller.SyncNetworks(red, blue); err != nil {
		t.Fatalf("failed to sync networks: %v", err)
	}

	entered := make(chan string, 2)
	release := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, network := range []util.NetInfo{red, blue} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- controller.ReconcileNetwork(network, func() error {
				entered <- network.GetNetworkName()
				<-release
				return nil
			})
		}()
	}

	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("independent UDN operation was serialized by the status aggregator")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("failed to reconcile network: %v", err)
		}
	}
}

func TestUplinkStateGatewayStatusControllerBatchesStatusForNetworksSharingUplink(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName   = "uplink1"
		nodeName     = "node-a"
		networkCount = 200
	)
	state := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(client, 0)
	informer := informerFactory.K8s().V1alpha1().UplinkStates()
	indexer := informer.Informer().GetIndexer()
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	controller := NewUplinkStateGatewayStatusController(
		nodeName, client, informer)
	t.Cleanup(controller.Stop)

	networks := make([]util.NetInfo, 0, networkCount)
	for i := range networkCount {
		networks = append(networks, uplinkGatewayNetInfo(
			t, fmt.Sprintf("network-%d", i), uplinkName))
	}
	if err := controller.SyncNetworks(networks...); err != nil {
		t.Fatalf("failed to sync networks: %v", err)
	}
	for _, network := range networks {
		if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
			t.Fatalf("failed to reconcile %s: %v", network.GetNetworkName(), err)
		}
	}

	if err := controller.Start(); err != nil {
		t.Fatalf("failed to start gateway status controller: %v", err)
	}
	gatewayReady, _ := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue &&
				strings.Contains(condition.Message, "200 active CUDN(s)")
		},
	)
	if gatewayReady == nil {
		t.Fatal("expected aggregate GatewayReady condition")
	}
	time.Sleep(2 * uplinkGatewayStatusBatchDelay)
	patches := 0
	for _, action := range client.Actions() {
		if _, ok := action.(clienttesting.PatchAction); ok {
			patches++
		}
	}
	if patches != 1 {
		t.Fatalf("expected one coalesced status patch for %d CUDNs, got %d",
			networkCount, patches)
	}
}

func TestUplinkStateGatewayStatusControllerDeletionBlocksInFlightCompletionPublication(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
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
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionFalse &&
				condition.Reason == uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
		},
	)
	controller.onUplinkStateDelete(
		newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition()))
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("failed to finish invalidated reconciliation: %v", err)
	}

	gatewayReady, _ := getUplinkGatewayCondition(t, client, uplinkName, nodeName)
	if gatewayReady == nil || gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending {
		t.Fatalf("expected old completion to leave readiness pending, got %#v", gatewayReady)
	}
}

func TestUplinkStateGatewayStatusControllerOldCompletionDoesNotUpdateRecreatedLifecycle(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(
		t, uplinkName, nodeName)
	initialUID := k8stypes.UID(uplinkName + "-" + nodeName + "-uid")
	if err := wait.PollUntilContextTimeout(
		context.Background(), 5*time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			controller.mutex.Lock()
			defer controller.mutex.Unlock()
			state := controller.uplinks[uplinkName]
			return state != nil && state.stateUID == initialUID, nil
		},
	); err != nil {
		t.Fatalf("gateway status controller did not observe initial UplinkState: %v", err)
	}

	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	entered := make(chan struct{})
	release := make(chan struct{})
	oldErr := errors.New("old lifecycle failed")
	done := make(chan error, 1)
	go func() {
		done <- controller.ReconcileNetwork(network, func() error {
			close(entered)
			<-release
			return oldErr
		})
	}()
	<-entered

	controller.mutex.Lock()
	oldState := controller.uplinks[uplinkName]
	controller.mutex.Unlock()
	if err := client.K8sV1alpha1().UplinkStates().Delete(
		context.Background(), uplinkutil.StateName(uplinkName, nodeName),
		metav1.DeleteOptions{},
	); err != nil {
		t.Fatalf("failed to delete UplinkState: %v", err)
	}
	if err := wait.PollUntilContextTimeout(
		context.Background(), 5*time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			controller.mutex.Lock()
			defer controller.mutex.Unlock()
			current := controller.uplinks[uplinkName]
			return current != nil && current != oldState && current.publicationInvalid, nil
		},
	); err != nil {
		t.Fatalf("gateway status controller did not retire deleted UplinkState: %v", err)
	}
	recreated := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	recreated.UID = "recreated-uid"
	if _, err := client.K8sV1alpha1().UplinkStates().Create(
		context.Background(), recreated, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("failed to recreate UplinkState: %v", err)
	}
	if err := wait.PollUntilContextTimeout(
		context.Background(), 5*time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			controller.mutex.Lock()
			defer controller.mutex.Unlock()
			current := controller.uplinks[uplinkName]
			return current != nil && current.stateUID == recreated.UID &&
				!current.publicationInvalid, nil
		},
	); err != nil {
		t.Fatalf("gateway status controller did not observe recreated UplinkState: %v", err)
	}
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile recreated lifecycle: %v", err)
	}

	close(release)
	if err := <-done; !errors.Is(err, oldErr) {
		t.Fatalf("expected old lifecycle error, got %v", err)
	}

	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	currentState := controller.uplinks[uplinkName]
	if currentState == oldState {
		t.Fatal("gateway readiness lifecycle was not replaced")
	}
	networkState := currentState.networks[network.GetNetworkName()]
	if networkState == nil || networkState.phase != uplinkGatewayNetworkReady {
		t.Fatalf("old completion changed recreated readiness: %#v", networkState)
	}
}

func TestUplinkStateGatewayStatusControllerDeletionDropsQueuedPublication(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	state := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(client, 0)
	informer := informerFactory.K8s().V1alpha1().UplinkStates()
	indexer := informer.Informer().GetIndexer()
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	controller := NewUplinkStateGatewayStatusController(
		nodeName, client, informer)
	t.Cleanup(controller.Stop)

	if err := controller.PrepareNetwork(
		uplinkGatewayNetInfo(t, "red", uplinkName),
	); err != nil {
		t.Fatalf("failed to prepare network: %v", err)
	}
	controller.onUplinkStateDelete(state)
	if err := controller.Start(); err != nil {
		t.Fatalf("failed to start gateway status controller: %v", err)
	}
	time.Sleep(2 * uplinkGatewayStatusBatchDelay)

	for _, action := range client.Actions() {
		if _, ok := action.(clienttesting.PatchAction); ok {
			t.Fatal("invalidated queued readiness was published")
		}
	}
}

func TestUplinkStateGatewayStatusControllerRunsProgrammingWhenPendingPublishFails(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	publishErr := errors.New("status API failed")
	var publishAttempts atomic.Int32
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if publishAttempts.Add(1) == 1 {
			return true, nil, publishErr
		}
		return false, nil, nil
	})
	programmed := false
	err := controller.ReconcileNetwork(network, func() error {
		programmed = true
		return nil
	})
	if !programmed {
		t.Fatal("status failure prevented UDN-owned programming")
	}
	if err != nil {
		t.Fatalf("status publication should be retried independently, got %v", err)
	}
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return publishAttempts.Load() >= 2 && condition != nil &&
				condition.Status == metav1.ConditionTrue
		},
	)
	if publishAttempts.Load() < 2 {
		t.Fatalf("expected failed status publication to be retried, got %d attempt(s)",
			publishAttempts.Load())
	}
}

func TestUplinkStateGatewayStatusControllerCleanupContinuesAfterPublishFailure(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to seed network readiness: %v", err)
	}
	waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil && condition.Status == metav1.ConditionTrue
		},
	)

	publishErr := errors.New("status API failed")
	var publishAttempts atomic.Int32
	client.PrependReactor("patch", "uplinkstates", func(clienttesting.Action) (bool, runtime.Object, error) {
		publishAttempts.Add(1)
		return true, nil, publishErr
	})
	cleanupErr := errors.New("cleanup failed")
	cleanupCalled := false
	err := controller.DeleteNetwork(network, func() error {
		cleanupCalled = true
		return cleanupErr
	})
	if !cleanupCalled {
		t.Fatal("status failure prevented UDN-owned cleanup")
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("expected cleanup error, got %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for publishAttempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if publishAttempts.Load() < 2 {
		t.Fatalf("expected failed status publication to be retried, got %d attempt(s)",
			publishAttempts.Load())
	}
}

func TestUplinkStateGatewayStatusControllerPrepareNetworkIgnoresMissingState(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, _, indexer := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	state, found, err := indexer.GetByKey(uplinkutil.StateName(uplinkName, nodeName))
	if err != nil {
		t.Fatalf("failed to read UplinkState: %v", err)
	}
	if !found {
		t.Fatal("expected UplinkState fixture")
	}
	if err := indexer.Delete(state); err != nil {
		t.Fatalf("failed to delete UplinkState: %v", err)
	}
	if err := controller.PrepareNetwork(
		uplinkGatewayNetInfo(t, "red", uplinkName),
	); err != nil {
		t.Fatalf("missing UplinkState should be expected during startup: %v", err)
	}
}

func TestUplinkStateGatewayStatusControllerReportsFailures(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	expectedErr := errors.New("failed to configure bridge mapping")
	err := controller.ReconcileNetwork(network, func() error {
		return newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed, expectedErr)
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected programming error, got %v", err)
	}
	gatewayReady, _ := waitForUplinkGatewayCondition(
		t, client, uplinkName, nodeName,
		func(condition *metav1.Condition) bool {
			return condition != nil &&
				condition.Reason == uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed
		},
	)
	if gatewayReady.Status != metav1.ConditionFalse ||
		gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed {
		t.Fatalf("unexpected failure condition: %#v", gatewayReady)
	}
}

func TestUplinkStateGatewayStatusControllerDPUHostPublishesHostGatewayReady(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	controller, client, _ := newUplinkStateGatewayStatusControllerForTest(t, uplinkName, nodeName)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	if err := controller.ReconcileNetwork(network, func() error { return nil }); err != nil {
		t.Fatalf("failed to reconcile DPU-host gateway: %v", err)
	}
	waitForUplinkGatewayPatches(t, client, 1)
	patches := 0
	for _, action := range client.Actions() {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok {
			continue
		}
		patches++
		payload := string(patch.GetPatch())
		if !strings.Contains(payload, uplinkv1alpha1.UplinkStateConditionHostGatewayReady) {
			t.Fatalf("expected HostGatewayReady, got %s", payload)
		}
		if strings.Contains(payload, `"`+uplinkv1alpha1.UplinkStateConditionGatewayReady+`"`) {
			t.Fatalf("unexpected GatewayReady ownership, got %s", payload)
		}
	}
	if patches == 0 {
		t.Fatal("expected HostGatewayReady status publication")
	}
}

func TestUplinkStateGatewayStatusControllerRejectsMismatchedStateIdentity(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	state := newUplinkStateFixture(uplinkName, nodeName)
	state.Spec.UplinkName = "other-uplink"
	client := uplinkfake.NewSimpleClientset(state.DeepCopy())
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(client, 0)
	informer := informerFactory.K8s().V1alpha1().UplinkStates()
	indexer := informer.Informer().GetIndexer()
	if err := indexer.Add(state); err != nil {
		t.Fatalf("failed to add UplinkState: %v", err)
	}
	controller := NewUplinkStateGatewayStatusController(
		nodeName, client, informer)
	t.Cleanup(controller.Stop)

	if err := controller.PrepareNetwork(uplinkGatewayNetInfo(t, "red", uplinkName)); err != nil {
		t.Fatalf("failed to prepare network: %v", err)
	}
	err := controller.publishGatewayCondition(uplinkName)
	if err == nil || !strings.Contains(err.Error(), "reports uplinkName") {
		t.Fatalf("expected identity validation error, got %v", err)
	}
}

func TestUplinkGatewayFingerprintIncludesHostFunction(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	vfID := int32(4)
	resolved := &resolvedUplinkGateway{
		hostInterfaceName: "eth0",
		bridgeName:        "breth0",
		hostFunction:      &uplinkv1alpha1.HostFunction{PFID: 2, VFID: &vfID},
	}
	fingerprint := resolvedUplinkGatewayFingerprint(resolved)
	if !fingerprint.hasHostFunction || fingerprint.hostPFID != 2 ||
		!fingerprint.hasHostVF || fingerprint.hostVFID != 4 {
		t.Fatalf("host function missing from fingerprint: %#v", fingerprint)
	}
}

func TestUDNUplinkStateControllerFiltersReadinessUpdates(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	gateway := &UserDefinedNetworkGateway{
		NetInfo: network,
		node:    &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}},
	}

	oldState := newUplinkStateFixture(uplinkName, nodeName, resolvedTrueCondition())
	if !gateway.uplinkStateNeedsUpdate(nil, oldState) {
		t.Fatal("initial UplinkState did not require reconciliation")
	}
	readinessOnly := oldState.DeepCopy()
	readinessOnly.Status.Conditions = append(
		readinessOnly.Status.Conditions,
		metav1.Condition{
			Type:   uplinkv1alpha1.UplinkStateConditionGatewayReady,
			Status: metav1.ConditionTrue,
		},
	)
	if gateway.uplinkStateNeedsUpdate(oldState, readinessOnly) {
		t.Fatal("gateway readiness update fed back into the UDN controller")
	}

	configurationChange := readinessOnly.DeepCopy()
	configurationChange.Status.IPAddresses = []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"}
	if !gateway.uplinkStateNeedsUpdate(readinessOnly, configurationChange) {
		t.Fatal("discovery-owned configuration update did not enqueue the UDN")
	}

	otherState := configurationChange.DeepCopy()
	otherState.Name = uplinkutil.StateName("other-uplink", nodeName)
	otherState.Spec.UplinkName = "other-uplink"
	if gateway.uplinkStateNeedsUpdate(nil, otherState) {
		t.Fatal("unrelated UplinkState enqueued the UDN")
	}
}

func TestUDNUplinkStateControllerRetriesUntilSuccess(t *testing.T) {
	prepareUplinkStateGatewayStatusControllerTest(t)
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
	)

	state := newUplinkStateFixture(uplinkName, nodeName)
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = "eth0"
	state.Status.MACAddress = "invalid"
	state.Status.IPAddresses = []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"}
	client := uplinkfake.NewSimpleClientset(state)
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(client, 0)
	informer := informerFactory.K8s().V1alpha1().UplinkStates()
	sharedInformer := informer.Informer()
	informerStop := make(chan struct{})
	informerFactory.Start(informerStop)

	statusController := NewUplinkStateGatewayStatusController(
		nodeName,
		client,
		informer,
	)
	countingLister := &countingUplinkStateLister{UplinkStateLister: informer.Lister()}
	network := uplinkGatewayNetInfo(t, "red", uplinkName)
	gateway := &UserDefinedNetworkGateway{
		NetInfo:                            network,
		node:                               &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}},
		uplinkStateLister:                  countingLister,
		uplinkStateGatewayStatusController: statusController,
	}
	gateway.uplinkStateController = gateway.newUplinkStateController(informer)
	t.Cleanup(func() {
		gateway.Stop()
		statusController.Stop()
		close(informerStop)
	})

	if err := controllerutil.Start(gateway.uplinkStateController); err != nil {
		t.Fatalf("failed to start UplinkState controller: %v", err)
	}

	// Four attempts was the old bounded retry limit. Wait for a fifth failed
	// attempt to prove the level-driven controller keeps the key active.
	err := wait.PollUntilContextTimeout(
		context.Background(),
		5*time.Millisecond,
		time.Second,
		true,
		func(context.Context) (bool, error) {
			return countingLister.gets.Load() >= 5, nil
		},
	)
	if err != nil {
		t.Fatalf("UplinkState controller stopped retrying: %v", err)
	}

	validState := state.DeepCopy()
	validState.Status.MACAddress = "02:42:ac:12:00:02"
	resolved, err := resolvedUplinkGatewayFromState(
		validState,
		uplinkName,
		nodeName,
		false,
	)
	if err != nil {
		t.Fatalf("failed to resolve valid UplinkState: %v", err)
	}
	gateway.operationMutex.Lock()
	gateway.uplinkGatewayCleanupRequired = true
	gateway.uplinkFingerprint = resolvedUplinkGatewayFingerprint(resolved)
	if err := sharedInformer.GetIndexer().Update(validState); err != nil {
		gateway.operationMutex.Unlock()
		t.Fatalf("failed to update cached UplinkState: %v", err)
	}
	gateway.operationMutex.Unlock()

	err = wait.PollUntilContextTimeout(
		context.Background(),
		5*time.Millisecond,
		time.Second,
		true,
		func(context.Context) (bool, error) {
			statusController.mutex.Lock()
			defer statusController.mutex.Unlock()
			uplinkState := statusController.uplinks[uplinkName]
			if uplinkState == nil || uplinkState.networks[network.GetNetworkName()] == nil {
				return false, nil
			}
			return uplinkState.networks[network.GetNetworkName()].phase == uplinkGatewayNetworkReady, nil
		},
	)
	if err != nil {
		t.Fatalf("UplinkState controller did not recover after the input became valid: %v", err)
	}
}
