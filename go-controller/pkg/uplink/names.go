// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/validate/content"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func StateName(uplinkName, nodeName string) string {
	// UplinkState names are only stable object keys. Do not parse identity from
	// them: Uplink and Node names may contain dots, and long keys are hashed.
	name := fmt.Sprintf("%s.%s", uplinkName, nodeName)
	if len(name) <= content.DNS1123SubdomainMaxLength {
		return name
	}
	return fmt.Sprintf("uplinkstate-%s", util.HashForOVN(name))
}

func StateIdentity(state *uplinkv1alpha1.UplinkState) (uplinkName, nodeName string) {
	return state.Status.UplinkName, state.Status.NodeName
}

// GetState returns the UplinkState for an Uplink and node after validating that
// the identity stored in its status matches the requested identity.
func GetState(
	lister uplinklisters.UplinkStateLister, uplinkName, nodeName string,
) (*uplinkv1alpha1.UplinkState, error) {
	stateName := StateName(uplinkName, nodeName)
	state, err := lister.Get(stateName)
	if err != nil {
		return nil, err
	}
	if err := ValidateStateIdentity(state, stateName, uplinkName, nodeName); err != nil {
		return nil, err
	}
	return state, nil
}

func ValidateStateIdentity(state *uplinkv1alpha1.UplinkState, stateName, uplinkName, nodeName string) error {
	stateUplink, stateNode := StateIdentity(state)
	if stateUplink == uplinkName && stateNode == nodeName {
		return nil
	}
	return fmt.Errorf("uplink state %s reports uplinkName %q and nodeName %q, expected uplinkName %q and nodeName %q",
		stateName, stateUplink, stateNode, uplinkName, nodeName)
}
