// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/exp/maps"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// checkPodAnnot defines additional checks for the allowed annotations
type checkPodAnnot func(nodeLister listers.NodeLister, v annotationChange, pod *corev1.Pod, nodeName string) error

// ovnPodAnnotationCheck validates changes to the ovn pod-networks annotation. It is shared by the
// full-mode and DPU allowlists.
var ovnPodAnnotationCheck checkPodAnnot = func(nodeLister listers.NodeLister, v annotationChange, pod *corev1.Pod, nodeName string) error {
	// Ignore kubevirt pods with live migration, the IP can cross node-subnet boundaries
	if kubevirt.IsPodLiveMigratable(pod) {
		return nil
	}

	if pod.Spec.HostNetwork {
		return fmt.Errorf("the annotation is not allowed on host networked pods")
	}

	podAnnot, err := util.UnmarshalPodAnnotation(map[string]string{types.OvnPodAnnotationName: v.value}, types.DefaultNetworkName)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil
		}
		return err
	}
	node, err := nodeLister.Get(nodeName)
	if err != nil {
		return fmt.Errorf("could not get info on node %s from client: %w", nodeName, err)
	}

	subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return err
	}
	for _, ip := range podAnnot.IPs {
		if !util.IsContainedInAnyCIDR(ip, subnets...) {
			return fmt.Errorf("%s does not belong to %s node", ip, nodeName)
		}
	}
	return nil
}

// interconnectPodAnnotations holds annotations allowed for full-mode ovnkube-node:<nodeName>
// and ovnkube-node-dpu identities.
var interconnectPodAnnotations = map[string]checkPodAnnot{
	types.OvnPodAnnotationName: ovnPodAnnotationCheck,
}

// dpuPodAnnotations holds annotations allowed for the ovnkube-node-dpu identity. It is a superset
// of interconnectPodAnnotations and the DPU connection-status annotations to be set on the dpu-host
var dpuPodAnnotations = map[string]checkPodAnnot{
	types.OvnPodAnnotationName:    ovnPodAnnotationCheck,
	util.DPUConnectionStatusAnnot: nil,
}

// dpuHostPodAnnotations holds pod annotations allowed for ovnkube-node running in dpu-host
// mode (identity system:ovn-node-dpu-host:<node>). dpu-host only requests VF plumbing from
// the DPU, so it only sets the connection-details annotation; the connection-status and the
// pod-networks annotation are written by the DPU and the ovnkube-cluster-manager.
var dpuHostPodAnnotations = map[string]checkPodAnnot{
	util.DPUConnectionDetailsAnnot: nil,
}

// PodAdmissionConditionOptions specifies additional validate admission for pod.
type PodAdmissionConditionOption struct {
	// CommonNamePrefix specifies common name in Usename
	CommonNamePrefix string `json:"commonNamePrefix"`
	// AllowedPodAnnotations contains annotation list to check Pod's annotation for webhook
	// this is used for Defaut=false because ovn-node case requires more detailed pod annotation
	// check
	AllowedPodAnnotations []string `json:"allowedPodAnnotations"`
	// AllowedPodAnnotationKeys contains AllowedPodAnnotations value as sets.Set[]
	AllowedPodAnnotationKeys sets.Set[string]
}

// InitPodAdmissionConditionOptions initializes PodAdmissionConditionOption: Load json from fileName
func InitPodAdmissionConditionOptions(fileName string) (podAdmissions []PodAdmissionConditionOption, err error) {
	if fileName != "" {
		file, err := os.ReadFile(fileName)
		if err != nil {
			return nil, err
		}

		if err = json.Unmarshal(file, &podAdmissions); err != nil {
			return nil, err
		}
	}

	// initialize Sets from slices
	for i, v := range podAdmissions {
		podAdmissions[i].AllowedPodAnnotationKeys = sets.New[string](v.AllowedPodAnnotations...)
	}

	return podAdmissions, nil
}

type PodAdmission struct {
	nodeLister            listers.NodeLister
	annotations           map[string]checkPodAnnot
	annotationKeys        sets.Set[string]
	dpuAnnotations        map[string]checkPodAnnot
	dpuAnnotationKeys     sets.Set[string]
	dpuHostAnnotations    map[string]checkPodAnnot
	dpuHostAnnotationKeys sets.Set[string]
	// allAnnotationKeys is the union of every guarded annotation across all identities. It is used
	// to decide whether a non ovnkube-node* user touched any annotation that requires ownership checks.
	allAnnotationKeys sets.Set[string]
	extraAllowedUsers sets.Set[string]
	podAdmissions     []PodAdmissionConditionOption
}

func NewPodAdmissionWebhook(nodeLister listers.NodeLister, podAdmissions []PodAdmissionConditionOption, extraAllowedUsers ...string) *PodAdmission {
	allAnnotationKeys := sets.New[string](maps.Keys(interconnectPodAnnotations)...)
	allAnnotationKeys.Insert(maps.Keys(dpuPodAnnotations)...)
	allAnnotationKeys.Insert(maps.Keys(dpuHostPodAnnotations)...)
	return &PodAdmission{
		nodeLister:            nodeLister,
		annotations:           interconnectPodAnnotations,
		annotationKeys:        sets.New[string](maps.Keys(interconnectPodAnnotations)...),
		dpuAnnotations:        dpuPodAnnotations,
		dpuAnnotationKeys:     sets.New[string](maps.Keys(dpuPodAnnotations)...),
		dpuHostAnnotations:    dpuHostPodAnnotations,
		dpuHostAnnotationKeys: sets.New[string](maps.Keys(dpuHostPodAnnotations)...),
		allAnnotationKeys:     allAnnotationKeys,
		extraAllowedUsers:     sets.New[string](extraAllowedUsers...),
		podAdmissions:         podAdmissions,
	}
}

func (p PodAdmission) ValidateCreate(_ context.Context, _ *corev1.Pod) (warnings admission.Warnings, err error) {
	// Ignore creation, the webhook is configured to only handle pod/status updates
	return nil, nil
}

func (p PodAdmission) ValidateDelete(_ context.Context, _ *corev1.Pod) (warnings admission.Warnings, err error) {
	// Ignore creation, the webhook is configured to only handle pod/status updates
	return nil, nil
}

var _ admission.Validator[*corev1.Pod] = &PodAdmission{}

func (p PodAdmission) ValidateUpdate(ctx context.Context, oldPod, newPod *corev1.Pod) (warnings admission.Warnings, err error) {

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	isOVNKubeNode, podAdmission, nodeName := checkNodeIdentity(p.podAdmissions, req.UserInfo)
	var isDPUHostNode, isDPUUser bool
	if !isOVNKubeNode {
		var dpuHostNodeName string
		if dpuHostNodeName, isDPUHostNode = dpuHostNodeIdentity(req.UserInfo); isDPUHostNode {
			nodeName = dpuHostNodeName
		} else if isDPUUser = isDPUExtraAllowedUser(p.extraAllowedUsers, req.UserInfo.Username); isDPUUser {
			nodeName = newPod.Spec.NodeName
		}
	}

	// effective allowlist: full-mode nodes use the interconnect set (pod-networks only),
	// the DPU SA additionally may set the DPU connection status annotation, and dpu-host is
	// restricted to DPU connection details annotation.
	effectiveAnnotations := p.annotations
	effectiveKeys := p.annotationKeys
	if isDPUUser {
		effectiveAnnotations = p.dpuAnnotations
		effectiveKeys = p.dpuAnnotationKeys
	}
	if isDPUHostNode {
		effectiveAnnotations = p.dpuHostAnnotations
		effectiveKeys = p.dpuHostAnnotationKeys
	}

	changes := mapDiff(oldPod.Annotations, newPod.Annotations)
	changedKeys := maps.Keys(changes)

	// user is in additional acceptance condition list
	if podAdmission != nil {
		// additional acceptance condition check
		if !podAdmission.AllowedPodAnnotationKeys.HasAll(changedKeys...) {
			return nil, fmt.Errorf("%s node: %q is not allowed to set the following annotations on pod: %q: %v", podAdmission.CommonNamePrefix, nodeName, newPod.Name, sets.New[string](changedKeys...).Difference(podAdmission.AllowedPodAnnotationKeys).UnsortedList())
		}
	}

	if !isOVNKubeNode && !isDPUUser && !isDPUHostNode && podAdmission == nil {
		guardedChangedKeys := p.allAnnotationKeys.Intersection(sets.New[string](changedKeys...))
		if len(guardedChangedKeys) == 0 {
			// the user is not an ovnkube-node and hasn't changed any guarded annotations
			return nil, nil
		}

		if !p.extraAllowedUsers.Has(req.UserInfo.Username) {
			return nil, fmt.Errorf("user %q is not allowed to set the following annotations on pod: %q: %v",
				req.UserInfo.Username,
				newPod.Name,
				guardedChangedKeys.UnsortedList())
		}
		// A generic extra-allowed user (e.g. ovnkube-cluster-manager) is only trusted with the
		// default allowlist; the DPU-only annotations are reserved for the dedicated DPU identities.
		if !p.annotationKeys.HasAll(guardedChangedKeys.UnsortedList()...) {
			return nil, fmt.Errorf("user %q is not allowed to set the following annotations on pod: %q: %v",
				req.UserInfo.Username,
				newPod.Name,
				guardedChangedKeys.Difference(p.annotationKeys).UnsortedList())
		}

		// The user is not ovnkube-node, in this case the nodeName comes from the object
		nodeName = newPod.Spec.NodeName
	}

	for _, key := range changedKeys {
		if check := effectiveAnnotations[key]; check != nil {
			if err := check(p.nodeLister, changes[key], newPod, nodeName); err != nil {
				return nil, fmt.Errorf("user: %q is not allowed to set %s on pod %q: %v", req.UserInfo.Username, key, newPod.Name, err)
			}
		}
	}

	// if there is no matched acceptanceCondition as well as ovnkube-node* identities, then skip following check
	if !isOVNKubeNode && !isDPUUser && !isDPUHostNode && podAdmission == nil {
		return nil, nil
	}

	identityDescription := "ovnkube-node on node"
	if isDPUUser {
		identityDescription = "ovnkube-node-dpu"
	} else if isDPUHostNode {
		identityDescription = "ovnkube-node on dpu-host node"
	} else if podAdmission != nil {
		identityDescription = podAdmission.CommonNamePrefix + " on node"
	}

	if oldPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("%s: %q is not allowed to modify annotations on pod %q", identityDescription, nodeName, oldPod.Name)
	}
	if newPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("%s: %q is not allowed to modify annotations on pod %q", identityDescription, nodeName, newPod.Name)
	}

	// ovnkube-node / ovnkube-node-dpu / dpu-host is not allowed to change annotations outside of its scope
	if (isOVNKubeNode || isDPUUser || isDPUHostNode) && !effectiveKeys.HasAll(changedKeys...) {
		return nil, fmt.Errorf("%s: %q is not allowed to set the following annotations on pod: %q: %v",
			identityDescription, nodeName, newPod.Name,
			sets.New[string](changedKeys...).Difference(effectiveKeys).UnsortedList())
	}

	// Verify that nothing but the annotations changed.
	// Since ovnkube-node only has the pod/status permissions, it is enough to check .Status and .ObjectMeta only.
	// Ignore .ManagedFields fields which are modified on every update.
	oldPodShallowCopy := oldPod
	newPodShallowCopy := newPod
	oldPodShallowCopy.Annotations = nil
	newPodShallowCopy.Annotations = nil
	oldPodShallowCopy.ManagedFields = nil
	newPodShallowCopy.ManagedFields = nil
	if !apiequality.Semantic.DeepEqual(oldPodShallowCopy.ObjectMeta, newPodShallowCopy.ObjectMeta) ||
		!apiequality.Semantic.DeepEqual(oldPodShallowCopy.Status, newPodShallowCopy.Status) {
		return nil, fmt.Errorf("%s: %q is not allowed to modify anything other than annotations", identityDescription, nodeName)
	}

	return nil, nil
}
