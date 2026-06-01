// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"testing"

	"github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
)

func TestGetState(t *testing.T) {
	g := gomega.NewWithT(t)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	lister := uplinklisters.NewUplinkStateLister(indexer)
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: StateName("blue", "node-a")},
		Status: uplinkv1alpha1.UplinkStateStatus{
			UplinkName: "blue",
			NodeName:   "node-a",
		},
	}
	g.Expect(indexer.Add(state)).To(gomega.Succeed())

	actual, err := GetState(lister, "blue", "node-a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(actual).To(gomega.BeIdenticalTo(state))

	state.Status.NodeName = "node-b"
	_, err = GetState(lister, "blue", "node-a")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("expected uplinkName \"blue\" and nodeName \"node-a\"")))

	_, err = GetState(lister, "red", "node-a")
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}
