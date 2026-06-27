// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/validate/content"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	StateAnnotationUplink = "k8s.ovn.org/uplink"
	StateAnnotationNode   = "k8s.ovn.org/node"
)

func StateName(uplinkName, nodeName string) string {
	name := fmt.Sprintf("%s.%s", uplinkName, nodeName)
	if len(name) <= content.DNS1123SubdomainMaxLength {
		return name
	}
	return fmt.Sprintf("uplinkstate-%s", util.HashForOVN(name))
}

func StateIdentity(state *uplinkv1alpha1.UplinkState) (uplinkName, nodeName string) {
	if state == nil {
		return "", ""
	}
	uplinkName = state.Status.UplinkName
	if uplinkName == "" {
		uplinkName = state.Annotations[StateAnnotationUplink]
	}
	nodeName = state.Status.NodeName
	if nodeName == "" {
		nodeName = state.Annotations[StateAnnotationNode]
	}
	return uplinkName, nodeName
}

func StateMatches(state *uplinkv1alpha1.UplinkState, uplinkName, nodeName string) bool {
	stateUplink, stateNode := StateIdentity(state)
	return stateUplink == uplinkName && stateNode == nodeName
}

func StateBelongsTo(state *uplinkv1alpha1.UplinkState, uplinkName string) bool {
	stateUplink, _ := StateIdentity(state)
	return stateUplink == uplinkName
}
