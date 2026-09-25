// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	nqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	nqoslisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Use real informer registrations; only failure injection and registration
// accounting are supplied by this wrapper. These informers outlive controllers.
type lifecycleInformer struct {
	cache.SharedIndexInformer
	handlers    map[cache.ResourceEventHandlerRegistration]cache.ResourceEventHandler
	failAdd     bool
	failRemoval bool
}

func newLifecycleInformer(obj runtime.Object) *lifecycleInformer {
	return &lifecycleInformer{
		SharedIndexInformer: cache.NewSharedIndexInformer(&cache.ListWatch{}, obj, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		handlers:            make(map[cache.ResourceEventHandlerRegistration]cache.ResourceEventHandler),
	}
}

func (i *lifecycleInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	if i.failAdd {
		return nil, errors.New("injected registration failure")
	}
	handle, err := i.SharedIndexInformer.AddEventHandler(handler)
	if err == nil {
		i.handlers[handle] = handler
	}
	return handle, err
}

func (i *lifecycleInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	err := i.SharedIndexInformer.RemoveEventHandler(handle)
	delete(i.handlers, handle)
	if i.failRemoval {
		return errors.New("injected removal failure")
	}
	return err
}

type lifecycleQoSInformer struct{ *lifecycleInformer }

func (i lifecycleQoSInformer) Informer() cache.SharedIndexInformer { return i.lifecycleInformer }
func (i lifecycleQoSInformer) Lister() nqoslisters.NetworkQoSLister {
	return nqoslisters.NewNetworkQoSLister(i.GetIndexer())
}

type lifecyclePodInformer struct{ *lifecycleInformer }

func (i lifecyclePodInformer) Informer() cache.SharedIndexInformer { return i.lifecycleInformer }
func (i lifecyclePodInformer) Lister() corelisters.PodLister {
	return corelisters.NewPodLister(i.GetIndexer())
}

type lifecycleNamespaceInformer struct{ *lifecycleInformer }

func (i lifecycleNamespaceInformer) Informer() cache.SharedIndexInformer { return i.lifecycleInformer }
func (i lifecycleNamespaceInformer) Lister() corelisters.NamespaceLister {
	return corelisters.NewNamespaceLister(i.GetIndexer())
}

func lifecycleController(informers []*lifecycleInformer) (*Controller, error) {
	return NewController("lifecycle", &util.DefaultNetInfo{}, nil, nil, nil,
		lifecycleQoSInformer{informers[0]}, lifecycleNamespaceInformer{informers[1]}, lifecyclePodInformer{informers[2]},
		nil, nil, nil, "node")
}

func lifecycleInformers() []*lifecycleInformer {
	return []*lifecycleInformer{newLifecycleInformer(&nqosapi.NetworkQoS{}), newLifecycleInformer(&corev1.Namespace{}), newLifecycleInformer(&corev1.Pod{})}
}

func TestControllerRegistrationFailureCleansUp(t *testing.T) {
	for stage := 0; stage < 3; stage++ {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			informers := lifecycleInformers()
			informers[stage].failAdd = true
			if _, err := lifecycleController(informers); err == nil {
				t.Fatal("expected registration error")
			}
			for _, informer := range informers {
				if len(informer.handlers) != 0 {
					t.Fatal("failed constructor retained a callback")
				}
			}
		})
	}
}

func TestStoppedControllersReleaseSharedInformerRegistrations(t *testing.T) {
	informers := lifecycleInformers()
	survivor, err := lifecycleController(informers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(survivor.shutdown)
	for n := 0; n < 20; n++ {
		c, err := lifecycleController(informers)
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		close(stop)
		// Exercise Run's early cache-sync exit, not just the shutdown helper.
		c.Run(1, stop)
		c.shutdown() // Idempotent; must not affect the surviving controller.
		if !c.nqosQueue.ShuttingDown() || !c.nqosPodQueue.ShuttingDown() || !c.nqosNamespaceQueue.ShuttingDown() {
			t.Fatal("queues survive cancelled cache synchronization")
		}
		for _, informer := range informers {
			if len(informer.handlers) != 1 {
				t.Fatalf("iteration %d: registration count did not return to baseline: %d", n, len(informer.handlers))
			}
		}
	}
	for _, handler := range informers[0].handlers {
		handler.OnAdd(&nqosapi.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns"}}, false)
	}
	if survivor.nqosQueue.Len() != 1 || survivor.nqosQueue.ShuttingDown() {
		t.Fatal("stopping another controller disabled the surviving listener")
	}
}

func TestShutdownContinuesAfterRemovalError(t *testing.T) {
	informers := lifecycleInformers()
	c, err := lifecycleController(informers)
	if err != nil {
		t.Fatal(err)
	}
	informers[0].failRemoval = true
	c.shutdown()
	for _, informer := range informers {
		if len(informer.handlers) != 0 {
			t.Fatal("cleanup stopped at the first removal error")
		}
	}
}

func TestMetricsCleanupPreservesOtherControllers(t *testing.T) {
	stopped, live := "lifecycle-stopped", "lifecycle-live"
	t.Cleanup(func() {
		(&Controller{controllerName: stopped}).teardownMetricsCollector()
		(&Controller{controllerName: live}).teardownMetricsCollector()
	})
	for _, name := range []string{stopped, live} {
		updateNetworkQoSCount(name, 1)
		recordNetworkQoSReconcileDuration(name, 1)
		recordPodReconcileDuration(name, 1)
		recordNamespaceReconcileDuration(name, 1)
		recordStatusPatchDuration(name, 1)
	}
	(&Controller{controllerName: stopped}).teardownMetricsCollector()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "network" {
					continue
				}
				if label.GetValue() == stopped {
					t.Fatalf("stopped controller retained metric %s", family.GetName())
				}
				if label.GetValue() == live {
					remaining++
				}
			}
		}
	}
	if remaining != 5 {
		t.Fatalf("expected all five live controller metric series, got %d", remaining)
	}
}
