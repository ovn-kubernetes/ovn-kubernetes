// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package logicalswitchmanager

import (
	"slices"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
)

func TestSortPodsForIPAM(t *testing.T) {
	g := gomega.NewWithT(t)
	pod := func(uid string, completed bool, initialized int64) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: ktypes.UID(uid)}}
		if completed {
			p.Status.Phase = corev1.PodSucceeded
		}
		if initialized != 0 {
			p.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodInitialized, LastTransitionTime: metav1.NewTime(time.Unix(initialized, 0)),
			}}
		}
		return p
	}
	oldest := pod("oldest", true, 0)
	older := pod("older", true, 20)
	newest := pod("newest", true, 30)
	tied := pod("tied", true, 30)
	live := pod("live", false, 10)
	objects := []interface{}{oldest, older, newest, live, tied}
	original := slices.Clone(objects)
	ordered, err := SortPodsForIPAM(objects)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ordered).To(gomega.Equal([]*corev1.Pod{live, newest, tied, older, oldest}))
	g.Expect(objects).To(gomega.Equal(original), "sorting must not reorder the caller's slice")
	g.Expect(SortPodsForIPAM(nil)).To(gomega.BeEmpty())
	_, err = SortPodsForIPAM([]interface{}{&corev1.Node{}})
	g.Expect(err).To(gomega.HaveOccurred())
}
