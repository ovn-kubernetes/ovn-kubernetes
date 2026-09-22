// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

var _ = Describe("UplinkState CRD", func() {
	// maxDefaultGateways mirrors the MaxItems marker on status.defaultGateways.
	const maxDefaultGateways = 256

	// uplinkStateWithDefaultGateways builds an UplinkState whose status carries
	// n distinct default gateway addresses. The status has no subresource, so
	// it is validated on Create.
	uplinkStateWithDefaultGateways := func(n int) *uplinkv1alpha1.UplinkState {
		gateways := make([]uplinkv1alpha1.IPAddress, 0, n)
		for i := 0; i < n; i++ {
			gateways = append(gateways, uplinkv1alpha1.IPAddress(fmt.Sprintf("10.0.%d.%d", i/256, i%256)))
		}
		return &uplinkv1alpha1.UplinkState{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "test-default-gateways-"},
			Spec: uplinkv1alpha1.UplinkStateSpec{
				UplinkName: "uplink",
				NodeName:   "node",
			},
			Status: uplinkv1alpha1.UplinkStateStatus{DefaultGateways: gateways},
		}
	}

	Context("status.defaultGateways", func() {
		It("accepts the maximum number of default gateways", func() {
			state := uplinkStateWithDefaultGateways(maxDefaultGateways)
			Expect(k8sClient.Create(ctx, state)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, state) })
			Expect(state.Status.DefaultGateways).To(HaveLen(maxDefaultGateways))
		})

		It("rejects more default gateways than the maximum", func() {
			state := uplinkStateWithDefaultGateways(maxDefaultGateways + 1)
			err := k8sClient.Create(ctx, state)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Too many"))
		})
	})
})
