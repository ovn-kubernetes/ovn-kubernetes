// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
)

const maxConditionMessageLength = 32768

type EventType = string

// There are only 2 allowed event types for now: Normal and Warning
const (
	EventTypeNormal  EventType = corev1.EventTypeNormal
	EventTypeWarning EventType = corev1.EventTypeWarning
)

// EventDetails may be used to pass event details to the event recorder, that is not used directly.
// It based on the EventRecorder interface for core.Events. It doesn't have related objects,
// as they are not used in the current implementation.
type EventDetails struct {
	EventType    EventType
	Reason, Note string
}

// MergeStatusCondition returns the requested condition with its transition
// time merged from the existing condition and reports whether it changed.
func MergeStatusCondition(existing []metav1.Condition, condition metav1.Condition) (metav1.Condition, bool) {
	condition.Message = trimConditionMessage(condition.Message)
	conditions := append([]metav1.Condition(nil), existing...)
	changed := meta.SetStatusCondition(&conditions, condition)
	return *meta.FindStatusCondition(conditions, condition.Type), changed
}

// ConditionToApply converts a status condition to its apply configuration.
func ConditionToApply(condition metav1.Condition) *metaapply.ConditionApplyConfiguration {
	apply := metaapply.Condition().
		WithType(condition.Type).
		WithStatus(condition.Status).
		WithReason(condition.Reason).
		WithMessage(condition.Message).
		WithLastTransitionTime(condition.LastTransitionTime)
	if condition.ObservedGeneration != 0 {
		apply = apply.WithObservedGeneration(condition.ObservedGeneration)
	}
	return apply
}

func trimConditionMessage(message string) string {
	if len(message) > maxConditionMessageLength {
		return message[:maxConditionMessageLength]
	}
	return message
}
