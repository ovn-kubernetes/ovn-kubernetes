package vtep

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/klog/v2"

	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/applyconfiguration/vtep/v1"
)

const (
	fieldManager = "clustermanager-vtep-controller"

	conditionTypeAccepted = "Accepted"

	reasonManagedModeNotSupported = "ManagedModeNotSupported"
	reasonAllocated               = "Allocated"
	reasonAllocationFailed        = "AllocationFailed"
	reasonCIDROverlap             = "CIDROverlap"

	fieldManagerNodeAlloc = "clustermanager-vtep-controller-nodes"
)

// updateNodeAllocations applies the desired node-to-VTEP-IP mappings to the
// VTEP status via SSA using a dedicated field manager. A diff guard skips the
// API call when the desired state matches the current status.
func (c *Controller) updateNodeAllocations(vtep *vtepv1.VTEP, desired map[string]vtepv1.DualStackIPs) error {
	current := make(map[string]vtepv1.DualStackIPs, len(vtep.Status.NodeAllocations))
	for _, a := range vtep.Status.NodeAllocations {
		current[a.NodeName] = a.VTEPIPs
	}

	if nodeAllocationsEqual(current, desired) {
		return nil
	}

	// NOTE: In the future for 5000 nodes revisit if we
	// want to use StrategicMergicPatches for sending a
	// true delta instead of the full list. In case of SSA
	// the diff is performened on the API server side.
	statusApply := vtepapply.VTEPStatus()
	for nodeName, ips := range desired {
		statusApply = statusApply.WithNodeAllocations(
			vtepapply.NodeVTEPAllocation().
				WithNodeName(nodeName).
				WithVTEPIPs(ips),
		)
	}

	_, err := c.vtepClient.K8sV1().VTEPs().ApplyStatus(
		context.Background(),
		vtepapply.VTEP(vtep.Name).WithStatus(statusApply),
		metav1.ApplyOptions{
			FieldManager: fieldManagerNodeAlloc,
			Force:        true,
		},
	)
	if err != nil {
		klog.Errorf("Failed to update node allocations for VTEP %s: %v", vtep.Name, err)
		return fmt.Errorf("failed to update node allocations for VTEP %s: %w", vtep.Name, err)
	}
	return nil
}

// nodeAllocationsEqual reports whether two node-to-VTEP-IP maps are identical,
// used as a diff guard to skip redundant SSA calls.
func nodeAllocationsEqual(a, b map[string]vtepv1.DualStackIPs) bool {
	if len(a) != len(b) {
		return false
	}
	for node, aIPs := range a {
		bIPs, ok := b[node]
		if !ok || len(aIPs) != len(bIPs) {
			return false
		}
		for i := range aIPs {
			if aIPs[i] != bIPs[i] {
				return false
			}
		}
	}
	return true
}

func (c *Controller) updateStatusCondition(vtep *vtepv1.VTEP, conditionType string, status metav1.ConditionStatus, reason, message string) error {
	const maxMessageLen = 32768
	if len(message) >= maxMessageLen {
		message = message[:maxMessageLen-1]
	}

	existingCondition := meta.FindStatusCondition(vtep.Status.Conditions, conditionType)
	if existingCondition != nil &&
		existingCondition.Status == status &&
		existingCondition.Reason == reason &&
		existingCondition.Message == message {
		return nil
	}

	condition := metaapply.Condition().
		WithType(conditionType).
		WithStatus(status).
		WithReason(reason).
		WithMessage(message)

	if existingCondition == nil || existingCondition.Status != status {
		condition = condition.WithLastTransitionTime(metav1.NewTime(time.Now()))
	} else {
		// LastTransitionTime should only be updated in case the Status (True/False) changes
		condition = condition.WithLastTransitionTime(existingCondition.LastTransitionTime)
	}

	_, err := c.vtepClient.K8sV1().VTEPs().ApplyStatus(
		context.Background(),
		vtepapply.VTEP(vtep.Name).WithStatus(
			vtepapply.VTEPStatus().WithConditions(condition),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		klog.Errorf("Failed to update status condition %q for VTEP %s: %v", conditionType, vtep.Name, err)
		return fmt.Errorf("failed to update status condition %q for VTEP %s: %w", conditionType, vtep.Name, err)
	}
	return nil
}
