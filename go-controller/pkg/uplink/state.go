// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

func StateReady(state *uplinkv1alpha1.UplinkState) bool {
	condition := meta.FindStatusCondition(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func ValidateOVSBridgeState(
	state *uplinkv1alpha1.UplinkState, uplinkName, nodeName string, requireReadyBridge bool,
) error {
	if state.Status.Type != uplinkv1alpha1.UplinkTypeOVSBridge {
		return fmt.Errorf("uplink state %s has no OVSBridge uplink for uplink %q on node %q",
			state.Name, uplinkName, nodeName)
	}
	if requireReadyBridge && !StateReady(state) {
		return fmt.Errorf("uplink state %s for uplink %q on node %q is not ready",
			state.Name, uplinkName, nodeName)
	}
	if requireReadyBridge && (state.Status.OVSBridge == nil || state.Status.OVSBridge.Name == "") {
		return fmt.Errorf("uplink state %s has no resolved OVS bridge for uplink %q",
			state.Name, uplinkName)
	}
	return nil
}
