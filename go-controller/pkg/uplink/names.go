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
	return state.Spec.UplinkName, state.Spec.NodeName
}

// UplinkForState finds the Uplink owning the UplinkState named stateName on
// one of the given nodes. UplinkState names are opaque keys that cannot be
// parsed back into an identity, so the owner is found by forward-computing
// every candidate's state name. The boolean reports whether exactly one
// Uplink/node pair matched.
func UplinkForState(
	uplinks []*uplinkv1alpha1.Uplink, nodeNames []string, stateName string,
) (*uplinkv1alpha1.Uplink, bool) {
	var matched *uplinkv1alpha1.Uplink
	matches := 0
	for _, uplink := range uplinks {
		for _, nodeName := range nodeNames {
			if StateName(uplink.Name, nodeName) != stateName {
				continue
			}
			matched = uplink
			matches++
		}
	}
	if matches != 1 {
		return nil, false
	}
	return matched, true
}

// GetState returns the UplinkState for an Uplink and node after validating that
// the identity stored in its spec matches the requested identity.
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
