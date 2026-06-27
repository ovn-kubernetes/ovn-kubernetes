// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"encoding/json"
	"fmt"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
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

// AddUplinkApplyReactor adds a reactor to handle Apply operations on the
// Uplink fake client.
func AddUplinkApplyReactor(fakeClient *uplinkfake.Clientset) {
	fakeClient.PrependReactor("patch", "uplinks", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		name := patchAction.GetName()

		existingObj, err := fakeClient.Tracker().Get(
			uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinks"), "", name)
		if err != nil {
			return true, nil, err
		}

		uplink := existingObj.(*uplinkv1alpha1.Uplink)
		if patchAction.GetSubresource() == "status" {
			type StatusPatch struct {
				Status uplinkv1alpha1.UplinkStatus `json:"status"`
			}

			var patchData StatusPatch
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}
			uplink.Status = patchData.Status
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
			uplink.Finalizers = patchData.Metadata.Finalizers
		}

		_ = fakeClient.Tracker().Update(
			uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinks"), uplink, "")
		if !uplink.DeletionTimestamp.IsZero() && len(uplink.Finalizers) == 0 {
			_ = fakeClient.Tracker().Delete(
				uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinks"),
				"",
				uplink.Name,
			)
		}
		return true, uplink, nil
	})

	fakeClient.PrependReactor("patch", "uplinkstates", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		name := patchAction.GetName()

		existingObj, err := fakeClient.Tracker().Get(
			uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinkstates"), "", name)
		if err != nil {
			return true, nil, err
		}

		uplinkState := existingObj.(*uplinkv1alpha1.UplinkState)
		if patchAction.GetSubresource() == "status" {
			type StatusPatch struct {
				Status map[string]json.RawMessage `json:"status"`
			}

			var patchData StatusPatch
			if err := json.Unmarshal(patchAction.GetPatch(), &patchData); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "uplinkName",
				func(value string) { uplinkState.Status.UplinkName = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "nodeName",
				func(value string) { uplinkState.Status.NodeName = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "type",
				func(value uplinkv1alpha1.UplinkType) { uplinkState.Status.Type = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "hostInterfaceName",
				func(value uplinkv1alpha1.InterfaceName) { uplinkState.Status.HostInterfaceName = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "ovsBridge",
				func(value *uplinkv1alpha1.OVSBridgeStatus) { uplinkState.Status.OVSBridge = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "macAddress",
				func(value uplinkv1alpha1.MACAddress) { uplinkState.Status.MACAddress = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "ipAddresses",
				func(value []uplinkv1alpha1.IPAddressCIDR) { uplinkState.Status.IPAddresses = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "defaultGateways",
				func(value []uplinkv1alpha1.IPAddress) { uplinkState.Status.DefaultGateways = value }); err != nil {
				return true, nil, err
			}
			if err := patchUplinkStateStatusField(patchData.Status, "conditions",
				func(value []metav1.Condition) { uplinkState.Status.Conditions = value }); err != nil {
				return true, nil, err
			}
		}

		_ = fakeClient.Tracker().Update(
			uplinkv1alpha1.SchemeGroupVersion.WithResource("uplinkstates"),
			uplinkState,
			"")
		return true, uplinkState, nil
	})
}

func patchUplinkStateStatusField[T any](statusPatch map[string]json.RawMessage, field string, set func(T)) error {
	rawValue, ok := statusPatch[field]
	if !ok {
		return nil
	}
	var value T
	if err := json.Unmarshal(rawValue, &value); err != nil {
		return fmt.Errorf("failed to decode UplinkState status field %q: %w",
			field, err)
	}
	set(value)
	return nil
}

func BuildNAD(name, namespace string, network *ovncnitypes.NetConf) (*nadapi.NetworkAttachmentDefinition, error) {
	config, err := json.Marshal(network)
	if err != nil {
		return nil, err
	}
	return GenerateNADWithConfig(name, namespace, string(config)), nil
}
