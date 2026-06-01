// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"
	"time"

	k8stesting "k8s.io/client-go/testing"

	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
)

func TestUplinkStateSharedInformerFactoryFiltersByNode(t *testing.T) {
	const nodeName = "node-a"

	client := uplinkfake.NewSimpleClientset()
	informerFactory := newUplinkStateSharedInformerFactory(client, nodeName)
	informerFactory.K8s().V1alpha1().UplinkStates().Informer()

	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)

	deadline := time.After(time.Second)
	for {
		for _, action := range client.Actions() {
			listAction, ok := action.(k8stesting.ListAction)
			if !ok || action.GetResource().Resource != "uplinkstates" {
				continue
			}
			if got, want := listAction.GetListRestrictions().Fields.String(), "status.nodeName="+nodeName; got != want {
				t.Fatalf("unexpected UplinkState field selector: got %q, want %q", got, want)
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("UplinkState informer did not issue a list request")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
