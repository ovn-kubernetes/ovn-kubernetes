// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"fmt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"sync"
	"testing"
	"time"
)

func TestInitialNADSnapshotPreservesLiveEvents(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	selected := networkIndexPod("selected", "blue")
	unrelated := networkIndexPod("unrelated", "green")
	implicit := networkIndexPod("implicit-primary", "")
	clients := util.GetOVNClientset(selected, unrelated, implicit).GetClusterManagerClientset()
	wf, err := NewClusterManagerWatchFactory(clients)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wf.Shutdown)
	if err := wf.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	added := sets.New[string]()
	updated := sets.New[string]()
	deleted := sets.New[string]()
	funcs := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			mu.Lock()
			defer mu.Unlock()
			added.Insert(obj.(*corev1.Pod).Name)
		},
		UpdateFunc: func(_, obj interface{}) {
			mu.Lock()
			defer mu.Unlock()
			updated.Insert(obj.(*corev1.Pod).Name)
		},
		DeleteFunc: func(obj interface{}) {
			mu.Lock()
			defer mu.Unlock()
			deleted.Insert(obj.(*corev1.Pod).Name)
		},
	}
	// Change an initially unrelated Pod while the handler is registering.
	// The index snapshot is only a startup optimization, never a live filter.
	handler, err := wf.AddPodHandlerWithInitialNADs([]string{"tenant/blue"}, funcs,
		func(_ context.Context, objects []interface{}) error {
			if len(objects) != 1 || objects[0].(*corev1.Pod).Name != selected.Name {
				return fmt.Errorf("unexpected initial snapshot: %v", objects)
			}
			changed := unrelated.DeepCopy()
			changed.ResourceVersion = "2"
			changed.Annotations["k8s.v1.cni.cncf.io/networks"] = "blue"
			_, err := clients.KubeClient.CoreV1().Pods("tenant").Update(context.Background(), changed, metav1.UpdateOptions{})
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wf.RemovePodHandler(handler) })
	mu.Lock()
	initialAdded := added.Clone()
	mu.Unlock()
	if !initialAdded.Equal(sets.New("selected")) {
		t.Fatalf("initial add replay: got %v, want only selected", initialAdded)
	}

	// A Pod on a newly-associated NAD must still reach this handler even
	// though that NAD was absent from its registration-time keys.
	future := networkIndexPod("future-nad", "new-nad")
	if _, err := clients.KubeClient.CoreV1().Pods("tenant").Create(context.Background(), future, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := clients.KubeClient.CoreV1().Pods("tenant").Delete(context.Background(), selected.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		mu.Lock()
		done := updated.Has("unrelated") && added.Has("future-nad") && deleted.Has("selected")
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("lost a live add, update, or delete around indexed startup")
		case <-ticker.C:
		}
	}

	// Primary-network callers retain the ordinary full-store snapshot.
	var fullSnapshot sets.Set[string]
	fullHandler, err := wf.AddPodHandler(cache.ResourceEventHandlerFuncs{}, func(_ context.Context, objects []interface{}) error {
		fullSnapshot = sets.New[string]()
		for _, obj := range objects {
			fullSnapshot.Insert(obj.(*corev1.Pod).Name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wf.RemovePodHandler(fullHandler)
	if !fullSnapshot.Has(implicit.Name) {
		t.Fatal("unfiltered startup lost a Pod with implicit primary-network membership")
	}
}
