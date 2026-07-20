// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMergeStatusCondition(t *testing.T) {
	g := gomega.NewWithT(t)
	transitionTime := metav1.NewTime(time.Unix(1, 0))
	existing := []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		LastTransitionTime: transitionTime,
	}}

	condition, changed := MergeStatusCondition(existing, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "ready",
	})
	g.Expect(changed).To(gomega.BeFalse())
	g.Expect(condition.LastTransitionTime).To(gomega.Equal(transitionTime))

	condition, changed = MergeStatusCondition(existing, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "StillReady",
		Message: strings.Repeat("x", maxConditionMessageLength+1),
	})
	g.Expect(changed).To(gomega.BeTrue())
	g.Expect(condition.Reason).To(gomega.Equal("StillReady"))
	g.Expect(condition.Message).To(gomega.HaveLen(maxConditionMessageLength))
	g.Expect(condition.LastTransitionTime).To(gomega.Equal(transitionTime))
	g.Expect(existing[0].Reason).To(gomega.Equal("Ready"))

	condition, changed = MergeStatusCondition(existing, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  "Failed",
		Message: "failed",
	})
	g.Expect(changed).To(gomega.BeTrue())
	g.Expect(condition.LastTransitionTime.Time).To(gomega.BeTemporally(">", transitionTime.Time))
}

func TestConditionToApply(t *testing.T) {
	g := gomega.NewWithT(t)
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 3,
		LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		Reason:             "Ready",
		Message:            "ready",
	}

	apply := ConditionToApply(condition)
	g.Expect(apply.Type).To(gomega.HaveValue(gomega.Equal(condition.Type)))
	g.Expect(apply.Status).To(gomega.HaveValue(gomega.Equal(condition.Status)))
	g.Expect(apply.ObservedGeneration).To(gomega.HaveValue(gomega.Equal(condition.ObservedGeneration)))
	g.Expect(apply.LastTransitionTime).To(gomega.HaveValue(gomega.Equal(condition.LastTransitionTime)))
	g.Expect(apply.Reason).To(gomega.HaveValue(gomega.Equal(condition.Reason)))
	g.Expect(apply.Message).To(gomega.HaveValue(gomega.Equal(condition.Message)))
}
