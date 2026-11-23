// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"strings"
	"sync"
	"testing"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestPendingDeleteOpClearedOnlyAfterSyntheticResync(t *testing.T) {
	const (
		ownerName = "owner-eip"
		egressIP  = "192.0.2.10"
		nodeName  = "node1"
	)

	t.Run("synthetic resync failure leaves the pending delete", func(t *testing.T) {
		sibling := &egressipv1.EgressIP{
			ObjectMeta: metav1.ObjectMeta{Name: "sibling-eip"},
			Spec:       egressipv1.EgressIPSpec{EgressIPs: []string{"not-an-ip"}},
		}
		eIPC := pendingDeleteController(t, ownerName, egressIP, nodeName, sibling)

		err := eIPC.reconcileCloudPrivateIPConfig(context.Background(), deletedCloudPrivateIPConfig(ownerName, egressIP, nodeName), nil)
		if err == nil || !strings.Contains(err.Error(), "synthetic update for EgressIP: sibling-eip failed") {
			t.Fatalf("expected synthetic resync failure for sibling-eip, got %v", err)
		}
		op := pendingOp(t, eIPC, ownerName, egressIP)
		if op.toAdd != "" || op.toDelete != nodeName {
			t.Fatalf("pending delete = {toAdd: %q, toDelete: %q}, want {toAdd: \"\", toDelete: %q}", op.toAdd, op.toDelete, nodeName)
		}
		// The failed sibling must be re-queued under its own key for retry.
		requeued, err := eIPC.retryEgressIPs.RequestRetryObjWithNoBackoff(sibling)
		if err != nil || !requeued {
			t.Fatalf("sibling-eip was not re-queued for retry, requested: %v, err: %v", requeued, err)
		}
	})

	t.Run("successful resync clears only the pending delete", func(t *testing.T) {
		eIPC := pendingDeleteController(t, ownerName, egressIP, nodeName)
		const updateIP = "192.0.2.11"
		eIPC.pendingCloudPrivateIPConfigsOps[ownerName][updateIP] = &cloudPrivateIPConfigOp{toAdd: "node2", toDelete: nodeName}

		if err := eIPC.reconcileCloudPrivateIPConfig(context.Background(), deletedCloudPrivateIPConfig(ownerName, egressIP, nodeName), nil); err != nil {
			t.Fatalf("delete path returned error: %v", err)
		}
		if _, exists := eIPC.pendingCloudPrivateIPConfigsOps[ownerName][egressIP]; exists {
			t.Fatal("pending delete was still present after a successful synthetic resync")
		}
		updateOp, exists := eIPC.pendingCloudPrivateIPConfigsOps[ownerName][updateIP]
		if !exists || updateOp.toAdd != "node2" || updateOp.toDelete != nodeName {
			t.Fatalf("in-progress update op = %+v, exists %v", updateOp, exists)
		}

		// A duplicate clear after the delete is already gone is a no-op.
		eIPC.clearPendingDeleteOp(ownerName, egressIP)
		if _, exists := eIPC.pendingCloudPrivateIPConfigsOps[ownerName][updateIP]; !exists {
			t.Fatal("idempotent clear removed the in-progress update op")
		}
	})
}

func pendingDeleteController(t *testing.T, ownerName, egressIP, nodeName string, egressIPs ...*egressipv1.EgressIP) *egressIPClusterController {
	t.Helper()
	objects := make([]runtime.Object, 0, len(egressIPs))
	for _, egressIPObject := range egressIPs {
		objects = append(objects, egressIPObject)
	}
	return &egressIPClusterController{
		kube:                              &kube.KubeOVN{EIPClient: egressipfake.NewSimpleClientset(objects...)},
		recorder:                          record.NewFakeRecorder(10),
		egressIPAssignmentMutex:           &sync.Mutex{},
		pendingCloudPrivateIPConfigsMutex: &sync.Mutex{},
		retryEgressIPs: objretry.NewRetryFramework("test", nil, nil, nil, &objretry.ResourceHandler{
			ObjType: factory.EgressIPType,
		}),
		pendingCloudPrivateIPConfigsOps: map[string]map[string]*cloudPrivateIPConfigOp{
			ownerName: {
				egressIP: {toDelete: nodeName},
			},
		},
		nodeAllocator: nodeAllocator{Mutex: &sync.Mutex{}, cache: map[string]*egressNode{}},
	}
}

func deletedCloudPrivateIPConfig(ownerName, egressIP, nodeName string) *ocpcloudnetworkapi.CloudPrivateIPConfig {
	return &ocpcloudnetworkapi.CloudPrivateIPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: egressIP,
			Annotations: map[string]string{
				util.OVNEgressIPOwnerRefLabel: ownerName,
			},
		},
		Spec:   ocpcloudnetworkapi.CloudPrivateIPConfigSpec{Node: nodeName},
		Status: ocpcloudnetworkapi.CloudPrivateIPConfigStatus{Node: nodeName},
	}
}

func pendingOp(t *testing.T, eIPC *egressIPClusterController, ownerName, egressIP string) *cloudPrivateIPConfigOp {
	t.Helper()
	ops, pending := eIPC.pendingCloudPrivateIPConfigsOps[ownerName]
	if !pending {
		t.Fatalf("pending ops for %s were removed", ownerName)
	}
	op, exists := ops[egressIP]
	if !exists {
		t.Fatalf("pending op for %s/%s was removed", ownerName, egressIP)
	}
	return op
}
