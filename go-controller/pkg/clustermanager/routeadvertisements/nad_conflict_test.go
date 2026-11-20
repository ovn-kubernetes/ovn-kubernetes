// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func nadWithRAs(ns, name, rv string, ras ...string) *nadtypes.NetworkAttachmentDefinition {
	nad := &nadtypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, ResourceVersion: rv,
			Annotations: map[string]string{},
		},
	}
	if len(ras) > 0 {
		j, _ := json.Marshal(ras)
		nad.Annotations[types.OvnRouteAdvertisementsKey] = string(j)
	}
	return nad
}

func nadListerFor(nads ...*nadtypes.NetworkAttachmentDefinition) nadlisters.NetworkAttachmentDefinitionLister {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, n := range nads {
		_ = idx.Add(n)
	}
	return nadlisters.NewNetworkAttachmentDefinitionLister(idx)
}

// fakeNADServer stands in for the apiserver: it holds the live copy of a
// single NAD and rejects merge patches whose resourceVersion precondition does
// not match it, the way the apiserver does.
type fakeNADServer struct {
	live    *nadtypes.NetworkAttachmentDefinition
	patches []string // resourceVersion carried by each patch received
}

func (s *fakeNADServer) client() *nadfake.Clientset {
	gr := schema.GroupResource{Group: "k8s.cni.cncf.io", Resource: "network-attachment-definitions"}
	client := nadfake.NewSimpleClientset()
	client.PrependReactor("patch", "network-attachment-definitions",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			var patch struct {
				Metadata struct {
					ResourceVersion string            `json:"resourceVersion"`
					Annotations     map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(a.(k8stesting.PatchAction).GetPatch(), &patch); err != nil {
				return true, nil, err
			}
			s.patches = append(s.patches, patch.Metadata.ResourceVersion)
			if patch.Metadata.ResourceVersion != "" && patch.Metadata.ResourceVersion != s.live.ResourceVersion {
				return true, nil, apierrors.NewConflict(gr, s.live.Name, nil)
			}
			updated := s.live.DeepCopy()
			maps.Copy(updated.Annotations, patch.Metadata.Annotations)
			s.live = updated
			return true, updated.DeepCopy(), nil
		})
	return client
}

func (s *fakeNADServer) liveRAs(t *testing.T) sets.Set[string] {
	t.Helper()
	var got []string
	if v := s.live.Annotations[types.OvnRouteAdvertisementsKey]; v != "" {
		if err := json.Unmarshal([]byte(v), &got); err != nil {
			t.Fatalf("annotation is not a JSON list: %v", err)
		}
	}
	return sets.New(got...)
}

// TestUpdateNADsPreconditionsOnCachedVersion covers the expected case: with an
// up to date cache the patch carries the cached resourceVersion and applies.
func TestUpdateNADsPreconditionsOnCachedVersion(t *testing.T) {
	cached := nadWithRAs("ns1", "nad1", "7", "ra-other")
	server := &fakeNADServer{live: cached.DeepCopy()}
	c := &Controller{nadClient: server.client(), nadLister: nadListerFor(cached)}

	hadUpdates, err := c.updateNADs(context.Background(), "ra-mine", []*nadtypes.NetworkAttachmentDefinition{cached})
	if err != nil {
		t.Fatalf("updateNADs: %v", err)
	}
	if !hadUpdates {
		t.Fatalf("expected updateNADs to report an update")
	}
	if len(server.patches) != 1 || server.patches[0] != "7" {
		t.Fatalf("expected one patch preconditioned on resourceVersion 7, got %v", server.patches)
	}
	if want := sets.New("ra-mine", "ra-other"); !server.liveRAs(t).Equal(want) {
		t.Fatalf("annotation is %v, want %v", sets.List(server.liveRAs(t)), sets.List(want))
	}
}

// TestUpdateNADsNoChangeDoesNotPatch covers the edge case where the cached
// annotation already reflects this RouteAdvertisements: no request is made.
func TestUpdateNADsNoChangeDoesNotPatch(t *testing.T) {
	cached := nadWithRAs("ns1", "nad1", "7", "ra-mine")
	server := &fakeNADServer{live: cached.DeepCopy()}
	c := &Controller{nadClient: server.client(), nadLister: nadListerFor(cached)}

	hadUpdates, err := c.updateNADs(context.Background(), "ra-mine", []*nadtypes.NetworkAttachmentDefinition{cached})
	if err != nil {
		t.Fatalf("updateNADs: %v", err)
	}
	if hadUpdates || len(server.patches) != 0 {
		t.Fatalf("expected no update and no patch, got hadUpdates=%v patches=%v", hadUpdates, server.patches)
	}
}

// TestUpdateNADsStaleCacheConflictsAndRequeueRecovers covers the lost-update
// hazard in the route-advertisements annotation. It holds the list of every
// RouteAdvertisements selecting a NAD, so each writer derives it from the
// previous value in its informer cache. If another writer recorded ra-other
// after the cache was last updated, an unconditional patch would drop it.
//
// The write must instead fail with a conflict, leaving the live annotation
// untouched, so that the RouteAdvertisements is requeued. The requeued
// reconcile, working from a refreshed cache, must end with both entries.
func TestUpdateNADsStaleCacheConflictsAndRequeueRecovers(t *testing.T) {
	const ns, name = "ns1", "nad1"
	// what this writer's informer cache shows
	stale := nadWithRAs(ns, name, "1")
	// what the server holds: a competing writer has recorded ra-other
	server := &fakeNADServer{live: nadWithRAs(ns, name, "2", "ra-other")}
	client := server.client()

	c := &Controller{nadClient: client, nadLister: nadListerFor(stale)}
	_, err := c.updateNADs(context.Background(), "ra-mine", []*nadtypes.NetworkAttachmentDefinition{stale})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected a conflict error so the RouteAdvertisements is requeued, got %v", err)
	}
	if want := sets.New("ra-other"); !server.liveRAs(t).Equal(want) {
		t.Fatalf("stale write was applied: annotation is %v, want %v", sets.List(server.liveRAs(t)), sets.List(want))
	}

	// the requeued reconcile runs once the informer has caught up
	fresh := server.live.DeepCopy()
	c.nadLister = nadListerFor(fresh)
	if _, err := c.updateNADs(context.Background(), "ra-mine", []*nadtypes.NetworkAttachmentDefinition{fresh}); err != nil {
		t.Fatalf("updateNADs after requeue: %v", err)
	}
	if want := sets.New("ra-mine", "ra-other"); !server.liveRAs(t).Equal(want) {
		t.Fatalf("lost an update: annotation is %v, want %v", sets.List(server.liveRAs(t)), sets.List(want))
	}
	if want := []string{"1", "2"}; len(server.patches) != 2 || server.patches[0] != want[0] || server.patches[1] != want[1] {
		t.Fatalf("expected patches preconditioned on %v, got %v", want, server.patches)
	}
}
