package testing

import (
	"encoding/json"
	"fmt"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func GenerateNAD(networkName, name, namespace, topology, cidr, role string) *nadapi.NetworkAttachmentDefinition {
	return GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "0.4.0",
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
        "cniVersion": "0.4.0",
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
