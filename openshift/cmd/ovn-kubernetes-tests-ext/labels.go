// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sort"

	"github.com/openshift-eng/openshift-tests-extension/pkg/util/sets"
)

// map contains prepend labels to be added on the spec for an existing spec label
var labelToPrependLabelsMap = map[string][]string{
	// Make EVPN tests to be added with gated label.
	featureLabelEVPN: {
		"FeatureGate:EVPN",
	},
	featureLabelMACSecurity: {
		"FeatureGate:MACSecurity",
	},
}

// getTestExtensionLabels returns labels that should be applied to all tests in this extension
func getTestExtensionLabels() []string {
	return []string{"sig-network", "ovn-kubernetes-ote"}
}

// generatePrependedLabelsStr generates labels that are prepended to a test name
func generatePrependedLabelsStr(labels sets.Set[string]) string {
	labelList := labels.UnsortedList()
	sort.Strings(labelList)

	var labelsStr = ""
	for _, label := range labelList {
		labelsStr += "[" + label + "]"
	}
	return labelsStr
}

// getPrependLabels returns additional prepend labels for given labels.
func getPrependLabels(labels sets.Set[string]) []string {
	var prependLabels []string
	for _, label := range labels.UnsortedList() {
		prependLabels = append(prependLabels, labelToPrependLabelsMap[label]...)
	}
	return prependLabels
}
