// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"reflect"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

func TestPodTrafficSelectionCache(t *testing.T) {
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner"}, Spec: eipv1.EgressIPSpec{
		TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}},
		PodSelector:     metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
	}}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "class", Labels: map[string]string{"traffic": "one"}},
		Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
	owners := []*eipv1.EgressIP{owner}
	classes := []*trafficv1.TrafficClass{class}
	var cache PodTrafficSelectionCache
	initial, err := cache.Resolve(owners, classes)
	if err != nil {
		t.Fatalf("resolve initial traffic selection: %v", err)
	}
	initialDestination := requireTrafficDestination(t, initial, owner.Name)
	again, err := cache.Resolve(owners, classes)
	if err != nil {
		t.Fatalf("resolve unchanged traffic selection: %v", err)
	}
	if requireTrafficDestination(t, again, owner.Name) != initialDestination {
		t.Fatal("unchanged informer objects did not reuse resolved destinations")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "client"}}}
	if len(again.SelectPod(pod, nil)) != 1 {
		t.Fatal("matching pod was not selected")
	}
	pod.Labels["app"] = "other"
	if len(again.SelectPod(pod, nil)) != 0 {
		t.Fatal("cached selection ignored new pod labels")
	}

	updatedOwner := owner.DeepCopy()
	updatedOwner.Spec.PodSelector.MatchLabels["app"] = "other"
	updatedNamespace := owner.DeepCopy()
	updatedNamespace.Spec.NamespaceSelector.MatchLabels = map[string]string{"env": "production"}
	updatedMatch := class.DeepCopy()
	updatedMatch.Spec.Matches[0].DestinationCIDR = "2001:db8::/64"
	updatedLabels := class.DeepCopy()
	updatedLabels.Labels["traffic"] = "other"
	invalidMatch := class.DeepCopy()
	invalidMatch.Spec.Matches[0].DestinationCIDR = "invalid"
	t.Run("failed refresh preserves snapshot", func(t *testing.T) {
		failed, err := cache.Resolve(owners, []*trafficv1.TrafficClass{invalidMatch})
		if err == nil || failed != nil {
			t.Fatalf("invalid refresh returned selection %+v, error %v", failed, err)
		}
		recovered, err := cache.Resolve(owners, classes)
		if err != nil {
			t.Fatalf("resolve original inputs after failed refresh: %v", err)
		}
		if requireTrafficDestination(t, recovered, owner.Name) != initialDestination {
			t.Fatal("failed refresh discarded the last successful cache entry")
		}
	})
	for _, tc := range []struct {
		name    string
		owners  []*eipv1.EgressIP
		classes []*trafficv1.TrafficClass
	}{
		{"owner selector update", []*eipv1.EgressIP{updatedOwner}, classes},
		{"namespace selector update", []*eipv1.EgressIP{updatedNamespace}, classes},
		{"class match update", owners, []*trafficv1.TrafficClass{updatedMatch}},
		{"class label update", owners, []*trafficv1.TrafficClass{updatedLabels}},
		{"owner deleted", nil, classes},
		{"class deleted", owners, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, gotErr := cache.Resolve(tc.owners, tc.classes)
			want, wantErr := ResolvePodTraffic(tc.owners, tc.classes)
			if (gotErr == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("cached resolution = %+v, %v; fresh resolution = %+v, %v", got, gotErr, want, wantErr)
			}
			// A previous pod may still be programming this snapshot when inputs change.
			if got := requireTrafficDestination(t, initial, owner.Name).CIDR.String(); got != "192.0.2.0/24" {
				t.Fatalf("updating the cache mutated an earlier snapshot: %s", got)
			}
		})
	}

	peer := owner.DeepCopy()
	peer.Name = "peer"
	otherClass := class.DeepCopy()
	otherClass.Name = "other-class"
	withPeers, err := cache.Resolve([]*eipv1.EgressIP{owner, peer}, []*trafficv1.TrafficClass{class, otherClass})
	if err != nil {
		t.Fatalf("resolve traffic selection with peers: %v", err)
	}
	peerDestination := requireTrafficDestination(t, withPeers, owner.Name)
	reordered, err := cache.Resolve([]*eipv1.EgressIP{peer, owner}, []*trafficv1.TrafficClass{otherClass, class})
	if err != nil {
		t.Fatalf("resolve reordered traffic selection: %v", err)
	}
	if requireTrafficDestination(t, reordered, owner.Name) != peerDestination {
		t.Fatal("informer list order invalidated the cache")
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			eips := []*eipv1.EgressIP{owner, peer}
			tcs := []*trafficv1.TrafficClass{class, otherClass}
			if i%2 == 0 {
				tcs[0] = updatedMatch
			}
			selection, err := cache.Resolve(eips, tcs)
			want, wantErr := ResolvePodTraffic(eips, tcs)
			if err != nil || wantErr != nil || !reflect.DeepEqual(selection, want) {
				t.Errorf("concurrent resolution = %+v, %v", selection, err)
			}
		})
	}
	wg.Wait()
}

func requireTrafficDestination(t *testing.T, selection PodTrafficSelection, owner string) *Destination {
	t.Helper()
	destinations := selection.DestinationsFor(owner)
	if len(destinations) == 0 {
		t.Fatalf("owner %q has no resolved destinations in selection %+v", owner, selection)
	}
	return &destinations[0]
}
