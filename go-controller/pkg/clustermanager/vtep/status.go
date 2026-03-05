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
	reasonCIDROverlap             = "CIDROverlap"
)

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
