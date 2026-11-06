// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"slices"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/lru"
)

const (
	networkSelectionCacheEntries = 4096
	// Bound encoded input, not decoded heap usage. A per-entry admission limit
	// keeps this budget without custom byte accounting or eviction logic.
	networkSelectionCacheBytes = 8 * 1024 * 1024
)

type networkSelectionKey struct {
	namespace  string
	annotation string
}

// Cached values are immutable. Only syntax is cached: NAD and primary-network
// resolution must still run for each reconciliation. Content keys permit reuse
// across pod lifetimes without retaining Pods or needing deletion callbacks.
type networkSelectionCache struct {
	cache         *lru.Cache
	maxEntryBytes int
}

var parsedNetworkSelections = newNetworkSelectionCache(networkSelectionCacheEntries, networkSelectionCacheBytes)

// maxEntries must be positive. Oversized inputs parse normally without caching.
func newNetworkSelectionCache(maxEntries, maxBytes int) *networkSelectionCache {
	return &networkSelectionCache{
		cache:         lru.New(maxEntries),
		maxEntryBytes: maxBytes / maxEntries,
	}
}

// get returns read-only selections. Callers must copy any values they expose.
func (c *networkSelectionCache) get(namespace, annotation string) ([]*nadapi.NetworkSelectionElement, error) {
	if annotation == "" {
		return []*nadapi.NetworkSelectionElement{}, nil
	}
	if len(namespace)+len(annotation) > c.maxEntryBytes {
		return nadutils.ParseNetworkAnnotation(annotation, namespace)
	}
	key := networkSelectionKey{namespace: namespace, annotation: annotation}
	if selections, ok := c.cache.Get(key); ok {
		return selections.([]*nadapi.NetworkSelectionElement), nil
	}

	// Parse outside the cache lock so unrelated misses and hits can proceed.
	// Concurrent misses may parse the same input more than once; each result
	// is equivalent and remains read-only after publication.
	selections, err := nadutils.ParseNetworkAnnotation(annotation, namespace)
	if err != nil {
		return nil, err
	}
	c.cache.Add(key, selections)
	return selections, nil
}

func getReadOnlyNetworkSelections(pod *corev1.Pod) ([]*nadapi.NetworkSelectionElement, error) {
	selections, err := parsedNetworkSelections.get(pod.Namespace, pod.Annotations[nadapi.NetworkAttachmentAnnot])
	if err != nil {
		return nil, fmt.Errorf("failed to get all NetworkSelectionElements for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return selections, nil
}

func copyNetworkSelection(in *nadapi.NetworkSelectionElement) *nadapi.NetworkSelectionElement {
	if in == nil {
		return nil
	}
	out := *in
	out.IPRequest = slices.Clone(in.IPRequest)
	if in.PortMappingsRequest != nil {
		out.PortMappingsRequest = make([]*nadapi.PortMapEntry, len(in.PortMappingsRequest))
		for i, port := range in.PortMappingsRequest {
			if port != nil {
				copy := *port
				out.PortMappingsRequest[i] = &copy
			}
		}
	}
	if in.BandwidthRequest != nil {
		copy := *in.BandwidthRequest
		out.BandwidthRequest = &copy
	}
	if in.CNIArgs != nil {
		copy := runtime.DeepCopyJSON(*in.CNIArgs)
		out.CNIArgs = &copy
	}
	out.GatewayRequest = slices.Clone(in.GatewayRequest)
	for i, ip := range in.GatewayRequest {
		out.GatewayRequest[i] = slices.Clone(ip)
	}
	return &out
}
