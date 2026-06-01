// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"context"
	gotesting "testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
)

func TestUplinkApplyReactorPreservesUnmentionedStatusFields(t *gotesting.T) {
	client := uplinkfake.NewSimpleClientset(&uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: "uplink1.node1"},
		Status: uplinkv1alpha1.UplinkStateStatus{
			UplinkName:        "uplink1",
			NodeName:          "node1",
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "eth1",
			OVSBridge:         &uplinkv1alpha1.OVSBridgeStatus{Name: "ovsbr1"},
			MACAddress:        "0a:58:c0:00:02:01",
			IPAddresses:       []uplinkv1alpha1.IPAddressCIDR{"192.0.2.2/24"},
			DefaultGateways:   []uplinkv1alpha1.IPAddress{"192.0.2.1"},
		},
	})
	AddUplinkApplyReactor(client)

	_, err := client.K8sV1alpha1().UplinkStates().Apply(
		context.Background(),
		uplinkapply.UplinkState("uplink1.node1").WithStatus(
			uplinkapply.UplinkStateStatus().WithOVSBridge(
				uplinkapply.OVSBridgeStatus().WithName("ovsbr2"),
			),
		),
		metav1.ApplyOptions{FieldManager: "test", Force: true},
	)
	if err != nil {
		t.Fatalf("failed to apply OVSBridge-only status patch: %v", err)
	}

	_, err = client.K8sV1alpha1().UplinkStates().Apply(
		context.Background(),
		uplinkapply.UplinkState("uplink1.node1").WithStatus(
			uplinkapply.UplinkStateStatus().WithConditions(
				metaapply.Condition().
					WithType(string(uplinkv1alpha1.UplinkStateConditionReady)).
					WithStatus(metav1.ConditionFalse).
					WithReason(string(uplinkv1alpha1.UplinkStateReasonBridgeInvalid)).
					WithMessage("bridge is invalid"),
			),
		),
		metav1.ApplyOptions{FieldManager: "test", Force: true},
	)
	if err != nil {
		t.Fatalf("failed to apply conditions-only status patch: %v", err)
	}

	state, err := client.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		"uplink1.node1",
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("failed to get UplinkState: %v", err)
	}
	if state.Status.Type != uplinkv1alpha1.UplinkTypeOVSBridge {
		t.Fatalf("expected type to be preserved, got %q", state.Status.Type)
	}
	if state.Status.HostInterfaceName != "eth1" {
		t.Fatalf("expected host interface to be preserved, got %q",
			state.Status.HostInterfaceName)
	}
	if state.Status.OVSBridge == nil || state.Status.OVSBridge.Name != "ovsbr2" {
		t.Fatalf("expected OVS bridge to be updated, got %#v",
			state.Status.OVSBridge)
	}
	if state.Status.MACAddress != "0a:58:c0:00:02:01" {
		t.Fatalf("expected MAC address to be preserved, got %q",
			state.Status.MACAddress)
	}
	if len(state.Status.IPAddresses) != 1 ||
		state.Status.IPAddresses[0] != "192.0.2.2/24" {
		t.Fatalf("expected IP addresses to be preserved, got %#v",
			state.Status.IPAddresses)
	}
	if len(state.Status.DefaultGateways) != 1 ||
		state.Status.DefaultGateways[0] != "192.0.2.1" {
		t.Fatalf("expected default gateways to be preserved, got %#v",
			state.Status.DefaultGateways)
	}
	if len(state.Status.Conditions) != 1 ||
		state.Status.Conditions[0].Reason != string(uplinkv1alpha1.UplinkStateReasonBridgeInvalid) {
		t.Fatalf("expected condition to be updated, got %#v",
			state.Status.Conditions)
	}
}
