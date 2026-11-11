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

// TestUpdateNADsRecomputesOnConflict covers the lost-update hazard in the
// route-advertisements annotation. It holds the list of every
// RouteAdvertisements selecting a NAD, so each writer derives it from its own
// previous value, and the patch carries no concurrency control of its own.
// Without a precondition two RouteAdvertisements updating one NAD drop one of
// the two entries, which matters because consumers scope reconciliation on
// this annotation and would silently skip the network whose entry was lost.
//
// The reactor stands in for the other writer: it rejects the first patch the
// way the apiserver rejects a stale resourceVersion, having meanwhile recorded
// ra-other. A correct implementation re-reads, recomputes against that, and
// ends with both entries.
func TestUpdateNADsRecomputesOnConflict(t *testing.T) {
	const ns, name = "ns1", "nad1"
	gr := schema.GroupResource{Group: "k8s.cni.cncf.io", Resource: "network-attachment-definitions"}

	// what this writer's informer cache shows
	stale := nadWithRAs(ns, name, "1")
	// what the server holds; the reactor advances it to stand in for the
	// competing writer
	live := nadWithRAs(ns, name, "1")

	client := nadfake.NewSimpleClientset()
	var patches int
	client.PrependReactor("get", "network-attachment-definitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, live.DeepCopy(), nil
		})
	client.PrependReactor("patch", "network-attachment-definitions",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			patches++
			var patch struct {
				Metadata struct {
					ResourceVersion string            `json:"resourceVersion"`
					Annotations     map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(a.(k8stesting.PatchAction).GetPatch(), &patch); err != nil {
				return true, nil, err
			}
			if patches == 1 {
				// the competing writer lands between this writer's read and
				// its write, which is precisely the interleaving that drops
				// an entry when the patch has no precondition
				live = nadWithRAs(ns, name, "2", "ra-other")
				return true, nil, apierrors.NewConflict(gr, name, nil)
			}
			if patch.Metadata.ResourceVersion != live.ResourceVersion {
				return true, nil, apierrors.NewConflict(gr, name, nil)
			}
			updated := live.DeepCopy()
			maps.Copy(updated.Annotations, patch.Metadata.Annotations)
			live = updated
			return true, updated.DeepCopy(), nil
		})

	c := &Controller{nadClient: client, nadLister: nadListerFor(stale)}
	if _, err := c.updateNADs(context.Background(), "ra-mine", []*nadtypes.NetworkAttachmentDefinition{stale}); err != nil {
		t.Fatalf("updateNADs: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(live.Annotations[types.OvnRouteAdvertisementsKey]), &got); err != nil {
		t.Fatalf("annotation is not a JSON list: %v", err)
	}
	if want := sets.New("ra-mine", "ra-other"); !sets.New(got...).Equal(want) {
		t.Fatalf("lost an update: annotation is %v, want %v", sets.List(sets.New(got...)), sets.List(want))
	}
	if patches < 2 {
		t.Fatalf("expected the conflicting patch to be retried, saw %d patch calls", patches)
	}
}
