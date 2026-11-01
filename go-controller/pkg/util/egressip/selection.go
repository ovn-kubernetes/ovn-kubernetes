// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

// UnsupportedProtocol identifies an entry decoded by an older client as {}.
type UnsupportedProtocol struct {
	Class string
	// UID and generation detect class replacements and edits hidden by version skew.
	ClassUID        types.UID
	ClassGeneration int64
	Match           int
	Protocol        int
}

// TrafficResolution separates selector resolution from per-pod assignment.
type TrafficResolution struct {
	Destinations []Destination
	ClassCount   int
	Unsupported  []UnsupportedProtocol
}

// ResolveTraffic combines the supported matches of the selected classes.
func ResolveTraffic(eip *eipv1.EgressIP, classes []*trafficv1.TrafficClass) (TrafficResolution, error) {
	result := TrafficResolution{}
	selector, err := ParseTrafficSelector(eip.Spec.TrafficSelector)
	if err != nil {
		return result, err
	}
	var raw []trafficv1.TrafficMatch
	for _, class := range classes {
		if !selector.Matches(labels.Set(class.Labels)) {
			continue
		}
		result.ClassCount++
		raw = append(raw, class.Spec.Matches...)
		for i, match := range class.Spec.Matches {
			for j, protocol := range match.Protocols {
				if protocol.TCP == nil && protocol.UDP == nil && protocol.SCTP == nil {
					result.Unsupported = append(result.Unsupported, UnsupportedProtocol{Class: class.Name, ClassUID: class.UID, ClassGeneration: class.Generation, Match: i, Protocol: j})
				}
			}
		}
	}
	result.Destinations, err = ParseDestinations(raw)
	return result, err
}

// TrafficCandidate is one selective owner's resolved traffic for a pod.
type TrafficCandidate struct {
	EgressIP     *eipv1.EgressIP
	Destinations []Destination
}

// TrafficConflict identifies the accepted owner that rejects a candidate.
type TrafficConflict struct {
	Owner       string
	Destination Destination
	Other       Destination
}

// SelectTrafficOwners returns conflicts keyed by candidate owner, with nil for winners.
// Rejected candidates cannot block subsequent candidates.
func SelectTrafficOwners(candidates []TrafficCandidate) map[string]*TrafficConflict {
	candidates = slices.Clone(candidates)
	slices.SortFunc(candidates, func(a, b TrafficCandidate) int {
		if a.EgressIP.CreationTimestamp.Before(&b.EgressIP.CreationTimestamp) {
			return -1
		}
		if b.EgressIP.CreationTimestamp.Before(&a.EgressIP.CreationTimestamp) {
			return 1
		}
		return strings.Compare(a.EgressIP.Name, b.EgressIP.Name)
	})
	selected := make(map[string]*TrafficConflict, len(candidates))
	var winners []TrafficCandidate
	for _, candidate := range candidates {
		conflict := findTrafficConflict(candidate, winners)
		selected[candidate.EgressIP.Name] = conflict
		if conflict == nil {
			winners = append(winners, candidate)
		}
	}
	return selected
}

func findTrafficConflict(candidate TrafficCandidate, winners []TrafficCandidate) *TrafficConflict {
	for _, winner := range winners {
		for _, own := range candidate.Destinations {
			for _, other := range winner.Destinations {
				if own.Overlaps(other) {
					return &TrafficConflict{Owner: winner.EgressIP.Name, Destination: own, Other: other}
				}
			}
		}
	}
	return nil
}

// PodTrafficSelection holds one reconciliation's resolved owners and workload selectors.
// It is immutable and can be reused for every pod in that reconciliation.
type PodTrafficSelection map[string]podTrafficCandidate

type podTrafficCandidate struct {
	TrafficCandidate
	namespaceSelector labels.Selector
	podSelector       labels.Selector
}

// ResolvePodTraffic resolves each selective owner once. Invalid selectors exclude
// only that owner; traffic-condition reconciliation reports the invalid selection.
func ResolvePodTraffic(eips []*eipv1.EgressIP, classes []*trafficv1.TrafficClass) (PodTrafficSelection, error) {
	candidates := PodTrafficSelection{}
	for _, eip := range eips {
		if !HasTrafficSelector(eip.Spec) {
			continue
		}
		ns, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			continue
		}
		ps, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
		if err != nil {
			continue
		}
		resolution, err := ResolveTraffic(eip, classes)
		if err != nil {
			if _, selectorErr := ParseTrafficSelector(eip.Spec.TrafficSelector); selectorErr != nil {
				continue
			}
			return nil, fmt.Errorf("EgressIP %s traffic: %w", eip.Name, err)
		}
		if len(resolution.Destinations) != 0 {
			candidates[eip.Name] = podTrafficCandidate{
				TrafficCandidate:  TrafficCandidate{EgressIP: eip, Destinations: resolution.Destinations},
				namespaceSelector: ns, podSelector: ps,
			}
		}
	}
	return candidates, nil
}

// DestinationsFor returns no matches for absent or invalid owners.
func (selection PodTrafficSelection) DestinationsFor(name string) []Destination {
	return selection[name].Destinations
}

// SelectPod returns the eligible owners and conflicts independently of applied state.
func (selection PodTrafficSelection) SelectPod(pod *corev1.Pod, namespaceLabels map[string]string) map[string]*TrafficConflict {
	var candidates []TrafficCandidate
	for _, candidate := range selection {
		if candidate.namespaceSelector.Matches(labels.Set(namespaceLabels)) && candidate.podSelector.Matches(labels.Set(pod.Labels)) {
			candidates = append(candidates, candidate.TrafficCandidate)
		}
	}
	return SelectTrafficOwners(candidates)
}

// SelectPodTraffic resolves all selective peers for a pod, independent of local applied state.
func SelectPodTraffic(pod *corev1.Pod, namespaceLabels map[string]string, eips []*eipv1.EgressIP, classes []*trafficv1.TrafficClass) (map[string]*TrafficConflict, error) {
	selection, err := ResolvePodTraffic(eips, classes)
	if err != nil {
		return nil, err
	}
	return selection.SelectPod(pod, namespaceLabels), nil
}
