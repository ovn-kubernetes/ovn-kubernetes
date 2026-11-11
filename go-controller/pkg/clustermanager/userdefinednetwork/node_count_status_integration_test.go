// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestNodeCountStatusAPIIdentityAndGeneration(t *testing.T) {
	obj := &udnv1.ClusterUserDefinedNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", UID: "uid", Generation: 3}}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(obj); err != nil {
		t.Fatal(err)
	}
	client := udnfake.NewSimpleClientset(obj)
	c := &Controller{udnClient: client, cudnLister: udnlisters.NewClusterUserDefinedNetworkLister(indexer)}
	c.nodeCountStatus = newNodeCountStatusPublisher(c.publishNodeCountStatus)
	c.nodeCountStatus.enqueue = func(string, time.Duration) {}
	t.Cleanup(func() { c.nodeCountStatus.cancel(); controller.Stop(c.nodeCountStatus.reconciler) })
	var patched udnv1.ClusterUserDefinedNetwork
	patches := 0
	client.PrependReactor("patch", "clusteruserdefinednetworks", func(action ktesting.Action) (bool, runtime.Object, error) {
		patches++
		patch := action.(ktesting.PatchAction)
		if patch.GetSubresource() != "status" {
			t.Error("wrong subresource")
		}
		if err := json.Unmarshal(patch.GetPatch(), &patched); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	network := util.GenerateCUDNNetworkName(obj.Name)
	condition := nodeCountCondition(time.Now(), 1)
	if err := c.UpdateSubsystemCondition(context.Background(), network, "ClusterManager", condition); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatal("count publication blocked the caller")
	}
	if err := c.nodeCountStatus.reconcile(context.Background(), network); err != nil {
		t.Fatal(err)
	}
	if patched.UID != obj.UID || len(patched.Status.Conditions) != 1 || patched.Status.Conditions[0].ObservedGeneration != 3 {
		t.Fatalf("missing identity/generation: %+v", patched)
	}
	replacement := obj.DeepCopy()
	replacement.UID = "replacement"
	if err := indexer.Update(replacement); err != nil {
		t.Fatal(err)
	}
	if err := c.publishNodeCountStatus(context.Background(), network, obj.UID, obj.Generation, condition); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatal("stale UID was applied to replacement")
	}
	replacement.Generation++
	if err := indexer.Update(replacement); err != nil {
		t.Fatal(err)
	}
	if err := c.publishNodeCountStatus(context.Background(), network, replacement.UID, 3, condition); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatal("stale generation was applied")
	}
}
