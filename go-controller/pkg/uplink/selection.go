// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

type nodeConfigSelector struct {
	config   *uplinkv1alpha1.UplinkNodeConfig
	selector labels.Selector
}

// SelectNodeConfig returns the Uplink nodeConfig selected for a node and
// reports when multiple nodeConfigs select it.
func SelectNodeConfig(
	uplink *uplinkv1alpha1.Uplink,
	node *corev1.Node,
) (*uplinkv1alpha1.UplinkNodeConfig, bool, error) {
	selectors, err := nodeConfigSelectors(uplink)
	if err != nil {
		return nil, false, err
	}
	config, overlapping := selectNodeConfig(selectors, node)
	return config, overlapping, nil
}

// SelectNodeConfigs resolves an Uplink's nodeConfigs across the provided
// nodes. Nodes selected by multiple nodeConfigs are returned as conflicts and
// omitted from the selected map.
func SelectNodeConfigs(
	uplink *uplinkv1alpha1.Uplink,
	nodes []*corev1.Node,
) (map[string]uplinkv1alpha1.UplinkNodeConfig, []string, error) {
	selectors, err := nodeConfigSelectors(uplink)
	if err != nil {
		return nil, nil, err
	}

	selected := make(map[string]uplinkv1alpha1.UplinkNodeConfig)
	conflicts := make([]string, 0)
	for _, node := range nodes {
		config, overlapping := selectNodeConfig(selectors, node)
		if overlapping {
			conflicts = append(conflicts, node.Name)
			continue
		}
		if config != nil {
			selected[node.Name] = *config
		}
	}
	sort.Strings(conflicts)
	return selected, conflicts, nil
}

func nodeConfigSelectors(uplink *uplinkv1alpha1.Uplink) ([]nodeConfigSelector, error) {
	selectors := make([]nodeConfigSelector, 0, len(uplink.Spec.NodeConfigs))
	for i := range uplink.Spec.NodeConfigs {
		config := &uplink.Spec.NodeConfigs[i]
		selector, err := metav1.LabelSelectorAsSelector(&config.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("nodeConfig %d has invalid nodeSelector: %w", i, err)
		}
		selectors = append(selectors, nodeConfigSelector{config: config, selector: selector})
	}
	return selectors, nil
}

func selectNodeConfig(selectors []nodeConfigSelector, node *corev1.Node) (*uplinkv1alpha1.UplinkNodeConfig, bool) {
	var selected *uplinkv1alpha1.UplinkNodeConfig
	for _, entry := range selectors {
		if !entry.selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		if selected != nil {
			return nil, true
		}
		selected = entry.config
	}
	return selected, false
}
