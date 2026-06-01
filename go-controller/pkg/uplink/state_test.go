// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

func TestValidateOVSBridgeState(t *testing.T) {
	g := gomega.NewWithT(t)
	state := testUplinkState(metav1.ConditionTrue)

	g.Expect(ValidateOVSBridgeState(state, "blue", "node-a", true)).To(gomega.Succeed())

	state.Status.Conditions[0].Status = metav1.ConditionFalse
	g.Expect(ValidateOVSBridgeState(state, "blue", "node-a", true)).To(gomega.MatchError(
		gomega.ContainSubstring("is not ready")))

	g.Expect(ValidateOVSBridgeState(state, "blue", "node-a", false)).To(gomega.Succeed())
}

func testUplinkState(readyStatus metav1.ConditionStatus) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: "blue-node-a"},
		Status: uplinkv1alpha1.UplinkStateStatus{
			UplinkName:        "blue",
			NodeName:          "node-a",
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "breth0",
			OVSBridge: &uplinkv1alpha1.OVSBridgeStatus{
				Name: "breth0",
			},
			Conditions: []metav1.Condition{
				{
					Type:               uplinkv1alpha1.UplinkStateConditionReady,
					Status:             readyStatus,
					Reason:             uplinkv1alpha1.UplinkStateReasonReady,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}
}
