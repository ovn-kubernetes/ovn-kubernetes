// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"sync"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

// PodTrafficSelectionCache reuses compiled selectors and resolved destinations
// across pod reconciliations. It retains only the latest informer snapshot.
// Its zero value is ready for use; callers must not copy it after first use.
type PodTrafficSelectionCache struct {
	mu        sync.Mutex
	owners    map[string]*eipv1.EgressIP
	classes   map[string]*trafficv1.TrafficClass
	selection PodTrafficSelection
}

// Resolve returns an immutable selection for the supplied informer objects.
// Inputs must remain immutable, as required by informer listers: an update
// replaces the object, so pointer identity detects edits and replacements.
func (c *PodTrafficSelectionCache) Resolve(eips []*eipv1.EgressIP, classes []*trafficv1.TrafficClass) (PodTrafficSelection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selection != nil && c.matches(eips, classes) {
		return c.selection, nil
	}
	selection, err := ResolvePodTraffic(eips, classes)
	if err != nil {
		return nil, err
	}
	c.owners = make(map[string]*eipv1.EgressIP, len(eips))
	for _, eip := range eips {
		c.owners[eip.Name] = eip
	}
	c.classes = make(map[string]*trafficv1.TrafficClass, len(classes))
	for _, class := range classes {
		c.classes[class.Name] = class
	}
	c.selection = selection
	return selection, nil
}

func (c *PodTrafficSelectionCache) matches(eips []*eipv1.EgressIP, classes []*trafficv1.TrafficClass) bool {
	if len(c.owners) != len(eips) || len(c.classes) != len(classes) {
		return false
	}
	for _, eip := range eips {
		if c.owners[eip.Name] != eip {
			return false
		}
	}
	for _, class := range classes {
		if c.classes[class.Name] != class {
			return false
		}
	}
	return true
}
