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

func newUplinkGatewayControllerForTest(
	t *testing.T,
	uplinkName, nodeName string,
) (*UplinkGatewayController, *uplinkfake.Clientset) {
	t.Helper()
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Conditions: []metav1.Condition{{
				Type:   uplinkv1alpha1.UplinkStateConditionResolved,
				Status: metav1.ConditionTrue,
				Reason: uplinkv1alpha1.UplinkStateReasonResolved,
			}},
		},
	}
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
