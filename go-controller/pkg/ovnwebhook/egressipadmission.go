package ovnwebhook

import (
	"context"
	"fmt"
	"strings"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/exp/maps"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// checkNodeAnnot defines additional checks for the allowed annotations
type checkEgressIPAnnot func(v annotationChange) error

var commonEgressIPAnnotationChecks = map[string]checkEgressIPAnnot{
	util.EgressIPMarkAnnotation: func(v annotationChange) error {
		if v.action == removed {
			return fmt.Errorf("%s cannot be removed", util.OvnNodeChassisID)
		}
		if v.action == changed {
			return fmt.Errorf("%s cannot be changed once set", util.OvnNodeChassisID)
		}
		return nil
	},
}

type EgressIPAdmission struct {
	annotationChecks  map[string]checkEgressIPAnnot
	annotationKeys    sets.Set[string]
	extraAllowedUsers sets.Set[string]
}

func NewEgressIPAdmissionWebhook(extraAllowedUsers ...string) *EgressIPAdmission {
	checks := make(map[string]checkEgressIPAnnot)
	maps.Copy(checks, commonEgressIPAnnotationChecks)
	return &EgressIPAdmission{
		annotationChecks:  checks,
		annotationKeys:    sets.New[string](maps.Keys(checks)...),
		extraAllowedUsers: sets.New[string](extraAllowedUsers...),
	}
}

var _ admission.CustomValidator = &EgressIPAdmission{}

func (e EgressIPAdmission) ValidateCreate(_ context.Context, newObj runtime.Object) (warnings admission.Warnings, err error) {
	newEgressIP := newObj.(*egressipv1.EgressIP)
	keys := maps.Keys(newEgressIP.Annotations)
	if e.annotationKeys.HasAny(keys...) {
		return nil, fmt.Errorf("annotation[s] %s is/are not allowed to be added while creating Egress IP", strings.Join(keys, ", "))
	}
	return nil, nil
}

func (e EgressIPAdmission) ValidateDelete(_ context.Context, _ runtime.Object) (warnings admission.Warnings, err error) {
	// Ignore deletion, the webhook is configured to only handle presence of EgressIPMarkAnnotation during object creation
	return nil, nil
}

func (e EgressIPAdmission) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	oldEgressIP := oldObj.(*egressipv1.EgressIP)
	newEgressIP := newObj.(*egressipv1.EgressIP)

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	isOVNKubeClusterManager := isOVNKubeClusterManager(req.UserInfo)
	_, isOVNKubeNode := ovnkubeNodeIdentity(req.UserInfo)
	changes := mapDiff(oldEgressIP.Annotations, newEgressIP.Annotations)
	changedKeys := maps.Keys(changes)
	if !isOVNKubeClusterManager {
		if !e.annotationKeys.HasAny(changedKeys...) {
			// the user is not an ovnkube-cluster-manager and hasn't changed any ovnkube-cluster-manager annotations
			return nil, nil
		}

		if !e.extraAllowedUsers.Has(req.UserInfo.Username) {
			// the user is not in extraAllowedUsers
			return nil, fmt.Errorf("user %q is not allowed to set the following annotations on node: %q: %v",
				req.UserInfo.Username,
				newEgressIP.Name,
				e.annotationKeys.Intersection(sets.New[string](changedKeys...)).UnsortedList())
		}
	}

	for _, key := range changedKeys {
		if check := e.annotationChecks[key]; check != nil {
			if err := check(changes[key]); err != nil {
				return nil, fmt.Errorf("user: %q is not allowed to set %s on Egress IP %q: %v", req.UserInfo.Username, key, newEgressIP.Name, err)
			}
		}
	}

	// All the checks beyond this point are ovnkube-node specific
	// If the user is not ovnkube-node exit here
	if !isOVNKubeNode {
		return nil, nil
	}

	// ovnkube-node is not allowed to change annotations outside of it's scope
	if !e.annotationKeys.HasAll(changedKeys...) {
		return nil, fmt.Errorf("ovnkube-node is not allowed to set the following annotations: %v",
			sets.New[string](changedKeys...).Difference(e.annotationKeys).UnsortedList())
	}

	// Verify that nothing but the annotations changed.
	// Since ovnkube-node only has the egressip/status permissions, it is enough to check .Status and .ObjectMeta only.
	// Ignore .ManagedFields fields which are modified on every update.
	oldEgressIPShallowCopy := oldEgressIP
	newEgressIPShallowCopy := newEgressIP
	oldEgressIPShallowCopy.Annotations = nil
	newEgressIPShallowCopy.Annotations = nil
	oldEgressIPShallowCopy.ManagedFields = nil
	newEgressIPShallowCopy.ManagedFields = nil

	if !apiequality.Semantic.DeepEqual(oldEgressIPShallowCopy.ObjectMeta, newEgressIPShallowCopy.ObjectMeta) ||
		!apiequality.Semantic.DeepEqual(oldEgressIPShallowCopy.Status, newEgressIPShallowCopy.Status) {
		return nil, fmt.Errorf("ovnkube-node is not allowed to modify anything other than annotation")
	}
	return nil, nil
}
