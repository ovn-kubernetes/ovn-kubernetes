// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	networkqosv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
)

var _ = Describe("NetworkQoS CRD", func() {
	Context("spec.egress[].classifier.ports[].protocol", func() {
		// newNetworkQoS builds a NetworkQoS whose only egress rule matches the given protocol on port 80.
		newNetworkQoS := func(protocol string) *networkqosv1alpha1.NetworkQoS {
			return &networkqosv1alpha1.NetworkQoS{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "test-protocol-", Namespace: "default"},
				Spec: networkqosv1alpha1.Spec{
					Priority: 10,
					Egress: []networkqosv1alpha1.Rule{{
						DSCP:      10,
						Bandwidth: networkqosv1alpha1.Bandwidth{Rate: 1, Burst: 1},
						Classifier: networkqosv1alpha1.Classifier{
							Ports: []*networkqosv1alpha1.Port{{Protocol: protocol, Port: ptr.To[int32](80)}},
						},
					}},
				},
			}
		}

		DescribeTable("accepts a supported protocol",
			func(protocol string) {
				nqos := newNetworkQoS(protocol)
				Expect(k8sClient.Create(ctx, nqos)).To(Succeed(), "failed to create NetworkQoS with protocol %q", protocol)
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, nqos) })
			},
			Entry("TCP", "TCP"),
			Entry("UDP", "UDP"),
			Entry("SCTP", "SCTP"),
		)

		DescribeTable("rejects any other protocol",
			func(protocol string) {
				nqos := newNetworkQoS(protocol)
				err := k8sClient.Create(ctx, nqos)
				if err == nil {
					DeferCleanup(func() { _ = k8sClient.Delete(ctx, nqos) })
				}
				Expect(err).To(MatchError(ContainSubstring(`supported values: "TCP", "UDP", "SCTP"`)),
					"NetworkQoS with protocol %q must be rejected", protocol)
			},
			Entry("lowercase", "tcp"),
			Entry("unknown protocol", "ICMP"),
			Entry("supported name as a prefix", "TCP || ip4"),
			Entry("supported name in the middle", "ip4 || UDP || ip6"),
			Entry("supported name as a suffix", "ip4 || SCTP"),
		)
	})
})
