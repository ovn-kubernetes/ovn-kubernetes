// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
)

const podNetworkSelectionIndex = "podNetworkSelection"

// A malformed selection must still reach the allocator's error/retry handling.
// This key cannot be a qualified NAD name.
const invalidPodNetworkSelection = ""

type podNetworkSelectionQuery []string

func podNetworkSelectionIndexFunc(obj interface{}) ([]string, error) {
	if keys, ok := obj.(podNetworkSelectionQuery); ok {
		return append([]string{invalidPodNetworkSelection}, keys...), nil
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("expected Pod, got %T", obj)
	}
	annotation := pod.Annotations[nadapi.NetworkAttachmentAnnot]
	if annotation == "" {
		return nil, nil
	}
	var selections []*nadapi.NetworkSelectionElement
	var err error
	if strings.IndexAny(annotation, "[{\"") >= 0 {
		// The NAD parser dereferences JSON null elements while defaulting
		// namespaces. Invalid user input must not panic in the informer store.
		err = json.Unmarshal([]byte(annotation), &selections)
	} else {
		selections, err = nadutils.ParseNetworkAnnotation(annotation, pod.Namespace)
	}
	if err != nil {
		return []string{invalidPodNetworkSelection}, nil
	}
	keys := sets.New[string]()
	for _, selection := range selections {
		if selection == nil {
			return []string{invalidPodNetworkSelection}, nil
		}
		namespace := selection.Namespace
		if namespace == "" {
			namespace = pod.Namespace
		}
		keys.Insert(namespace + "/" + selection.Name)
	}
	return keys.UnsortedList(), nil
}

func podsForNADs(indexer cache.Indexer, nadKeys []string) ([]interface{}, error) {
	// Index takes one store read lock and deduplicates the union. Separate
	// ByIndex calls could miss a Pod moving between two NADs during startup.
	return indexer.Index(podNetworkSelectionIndex, podNetworkSelectionQuery(nadKeys))
}
