// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func GenerateNAD(networkName, name, namespace, topology, cidr, role string) *nadapi.NetworkAttachmentDefinition {
	return GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
	))
}

func AnnotateNADWithNetworkID(networkID string, nad *nadapi.NetworkAttachmentDefinition) {
	if len(nad.Annotations) == 0 {
		nad.Annotations = make(map[string]string)
	}
	nad.Annotations[types.OvnNetworkIDAnnotation] = networkID
}

func GenerateNADWithoutMTU(networkName, name, namespace, topology, cidr, role string) *nadapi.NetworkAttachmentDefinition {
	return GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "netAttachDefName": %q,
        "role": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
	))
}

func GenerateNADWithConfig(name, namespace, config string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: config},
	}
}

// AddNetworkConnectApplyReactor adds a reactor to handle Apply (patch) operations on the fake client.
func AddNetworkConnectApplyReactor(fakeClient *networkconnectfake.Clientset) {
	fakeClient.PrependReactor("patch", "clusternetworkconnects", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		name := patchAction.GetName()

		existingObj, err := fakeClient.Tracker().Get(
			networkconnectv1.SchemeGroupVersion.WithResource("clusternetworkconnects"), "", name)
		if err != nil {
			return true, nil, err
		}

		cnc := existingObj.(*networkconnectv1.ClusterNetworkConnect)
		if patchAction.GetSubresource() == "status" {
			// handle status patch
			type StatusPatch struct {
				Status networkconnectv1.ClusterNetworkConnectStatus `json:"status"`
			}

			var patchData StatusPatch
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}

			// Update the status
			// This is a simple overwrite for unit tests. The actual Server-Side Apply logic is not implemented
			// and may differ from the real server results.
			cnc.Status = patchData.Status
		} else {
			// update annotations
			if cnc.Annotations == nil {
				cnc.Annotations = map[string]string{}
			}

			var patchData map[string]interface{}
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}
			if metadata, ok := patchData["metadata"].(map[string]interface{}); ok {
				if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
					for k, v := range annotations {
						cnc.Annotations[k] = v.(string)
					}
				}
			}
		}

		_ = fakeClient.Tracker().Update(
			networkconnectv1.SchemeGroupVersion.WithResource("clusternetworkconnects"), cnc, "")
		return true, cnc, nil
	})
}

// AddVTEPApplyReactor adds a reactor to handle Apply (patch) operations on the VTEP fake client.
// It supports both status subresource patches and metadata patches (e.g. finalizers).
func AddVTEPApplyReactor(fakeClient *vtepfake.Clientset) {
	fakeClient.PrependReactor("patch", "vteps", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		name := patchAction.GetName()

		existingObj, err := fakeClient.Tracker().Get(
			vtepv1.SchemeGroupVersion.WithResource("vteps"), "", name)
		if err != nil {
			return true, nil, err
		}

		vtep := existingObj.(*vtepv1.VTEP)
		if patchAction.GetSubresource() == "status" {
			type StatusPatch struct {
				Status vtepv1.VTEPStatus `json:"status"`
			}

			var patchData StatusPatch
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}

			vtep.Status = patchData.Status
		} else {
			type MetadataPatch struct {
				Metadata struct {
					Finalizers []string `json:"finalizers"`
				} `json:"metadata"`
			}

			var patchData MetadataPatch
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}

			vtep.Finalizers = patchData.Metadata.Finalizers
		}

		_ = fakeClient.Tracker().Update(
			vtepv1.SchemeGroupVersion.WithResource("vteps"), vtep, "")

		// Simulate API server garbage collection: when an object has a
		// non-zero DeletionTimestamp and no remaining finalizers, the real
		// API server deletes it from etcd during the update via
		// ShouldDeleteDuringUpdate (see k8s.io/apiserver store.go). The
		// fake client does not implement this; an upstream attempt to add
		// it (kubernetes/kubernetes#122460) was never merged. Without this,
		// deleted VTEPs linger in the informer cache and cause false CIDR
		// overlap detections against dying objects.
		if !vtep.DeletionTimestamp.IsZero() && len(vtep.Finalizers) == 0 {
			_ = fakeClient.Tracker().Delete(
				vtepv1.SchemeGroupVersion.WithResource("vteps"), "", vtep.Name)
		}
		return true, vtep, nil
	})
}

func BuildNAD(name, namespace string, network *ovncnitypes.NetConf) (*nadapi.NetworkAttachmentDefinition, error) {
	config, err := json.Marshal(network)
	if err != nil {
		return nil, err
	}
	return GenerateNADWithConfig(name, namespace, string(config)), nil
}

// AddRAApplyReactor handles ApplyStatus calls for RouteAdvertisements on the
// fake client. The fake client does not implement server-side apply, so the RA
// controller's ApplyStatus call would be silently ignored and the Accepted
// status would never be persisted. This reactor decodes the status patch and
// updates the object in the fake tracker so informer caches see the change.
func AddRAApplyReactor(fakeClient *rafake.Clientset) {
	fakeClient.PrependReactor("patch", "routeadvertisements", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		if patchAction.GetSubresource() != "status" {
			return false, nil, nil
		}
		name := patchAction.GetName()
		existingObj, err := fakeClient.Tracker().Get(
			rav1.SchemeGroupVersion.WithResource("routeadvertisements"), "", name)
		if err != nil {
			return true, nil, err
		}
		ra := existingObj.(*rav1.RouteAdvertisements).DeepCopy()

		type patchShape struct {
			Status rav1.RouteAdvertisementsStatus `json:"status"`
		}
		var p patchShape
		if err := json.Unmarshal(patchAction.GetPatch(), &p); err != nil {
			return true, nil, err
		}
		ra.Status = p.Status
		if err := fakeClient.Tracker().Update(
			rav1.SchemeGroupVersion.WithResource("routeadvertisements"), ra, ""); err != nil {
			return true, nil, err
		}
		return true, ra, nil
	})
}

// FakeClientWithReactor is satisfied by any fake clientset that supports
// PrependReactor (e.g. *frrfake.Clientset, *rafake.Clientset, …).
type FakeClientWithReactor interface {
	PrependReactor(verb, resource string, reaction ktesting.ReactionFunc)
}

// AddGenerateNameReactor wires a Create reactor that assigns a deterministic
// name to objects whose GenerateName is set. The Kubernetes fake client does
// not implement generateName natively; without this reactor objects created
// with GenerateName end up with an empty name and cannot be found by List/Get.
func AddGenerateNameReactor(fakeClient FakeClientWithReactor) {
	var count uint32
	fakeClient.PrependReactor("create", "*",
		func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
			ret = action.(ktesting.CreateAction).GetObject()
			meta, ok := ret.(metav1.Object)
			if !ok {
				return
			}
			if meta.GetName() == "" && meta.GetGenerateName() != "" {
				meta.SetName(meta.GetGenerateName() + fmt.Sprintf("%d", atomic.AddUint32(&count, 1)))
			}
			return
		},
	)
}
