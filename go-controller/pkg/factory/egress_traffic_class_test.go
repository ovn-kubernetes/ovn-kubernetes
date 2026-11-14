// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestTrafficClassControllerGlobalInformers(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	config.OVNKubernetesFeature.EnableMultiNetwork = false
	clients := util.GetOVNClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: "remote"}},
		&trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "traffic"}},
	).GetOVNKubeControllerClientset()
	wf, err := NewOVNKubeControllerWatchFactory(clients, "local")
	if err != nil {
		t.Fatalf("create controller watch factory: %v", err)
	}
	t.Cleanup(wf.Shutdown)
	if err = wf.Start(); err != nil {
		t.Fatalf("start controller watch factory: %v", err)
	}
	var podList, podWatch bool
	for _, action := range clients.KubeClient.(*fake.Clientset).Actions() {
		if action.GetResource().Resource != "pods" {
			continue
		}
		switch action := action.(type) {
		case clienttesting.ListAction:
			podList = true
			if !action.GetListRestrictions().Fields.Empty() {
				t.Fatalf("global Pod list has field selector %s", action.GetListRestrictions().Fields)
			}
		case clienttesting.WatchAction:
			podWatch = true
			if !action.GetWatchRestrictions().Fields.Empty() {
				t.Fatalf("global Pod watch has field selector %s", action.GetWatchRestrictions().Fields)
			}
		}
	}
	if !podList || !podWatch {
		t.Fatalf("missing Pod list/watch: list=%t watch=%t", podList, podWatch)
	}
	classes, err := wf.TrafficClassInformer().Lister().List(labels.Everything())
	if err != nil || len(classes) != 1 {
		t.Fatalf("traffic class cache: %v, %v", classes, err)
	}
	pods, err := wf.PodCoreInformer().Lister().List(labels.Everything())
	if err != nil || len(pods) != 1 {
		t.Fatalf("pod cache without multi-network: %v, %v", pods, err)
	}
	namespaces, err := wf.NamespaceCoreInformer().Lister().List(labels.Everything())
	if err != nil || len(namespaces) != 1 {
		t.Fatalf("namespace cache without multi-network: %v, %v", namespaces, err)
	}
}
