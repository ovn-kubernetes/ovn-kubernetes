/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tests

import (
	"testing"

	"k8s.io/utils/net"

	"sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/utils/kubernetes"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

func init() {
	ConformanceTests = append(ConformanceTests,
		AdminNetworkPolicyEgressInlineCIDRPeers,
	)
}

var AdminNetworkPolicyEgressInlineCIDRPeers = suite.ConformanceTest{
	ShortName:   "AdminNetworkPolicyEgressInlineCIDRPeers",
	Description: "Tests support for egress traffic to CIDR peers using admin network policy API based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportAdminNetworkPolicy,
	},
	Manifests: []string{"base/admin_network_policy/standard-egress-inline-cidr-rules.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {
		// This test uses `inline-cidr-as-peers-example` ANP
		t.Run("Should support a 'deny-egress' rule policy for egress-cidr-peer", func(t *testing.T) {
			// harry-potter-1 is our client pod in gryffindor namespace
			// Let us pick a pod in ravenclaw namespace and try to connect, it won't work
			// ensure egress is DENIED to 0.0.0.0/0 from gryffindor; egressRule at index1 should take effect
			// luna-lovegood-0 is our server pod in ravenclaw namespace
			serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-ravenclaw", "luna-lovegood-0", s.TimeoutConfig.GetTimeout)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig, false)
			// Let us pick a pod in hufflepuff namespace and try to connect, it won't work
			// ensure egress is DENIED to 0.0.0.0/0 from gryffindor; egressRule at index1 should take effect
			// cedric-diggory-0 is our server pod in hufflepuff namespace
			serverPod = kubernetes.GetPod(t, s.Client, "network-policy-conformance-hufflepuff", "cedric-diggory-0", s.TimeoutConfig.GetTimeout)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig, false)
			// Let us pick a pod in slytherin namespace and try to connect, it will work since we have a higher priority allow rule
			// ensure traffic is allowed to slytherin; egressRule at index0 should take effect
			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod = kubernetes.GetPod(t, s.Client, "network-policy-conformance-slytherin", "draco-malfoy-0", s.TimeoutConfig.GetTimeout)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig, true)
		})
		// To test allow CIDR rule, insert the following rule at index0
		//- name: "allow-egress-to-specific-podIPs"
		//  action: "Allow"
		//  to:
		//  - networks:
		//	  - luna-lovegood-0.IP
		//    - cedric-diggory-0.IP
		t.Run("Should support an 'allow-egress' rule policy for egress-cidr-peer", func(t *testing.T) {
			serverPodRavenclaw := kubernetes.GetPod(t, s.Client, "network-policy-conformance-ravenclaw", "luna-lovegood-0", s.TimeoutConfig.GetTimeout)

			serverPodHufflepuff := kubernetes.GetPod(t, s.Client, "network-policy-conformance-hufflepuff", "cedric-diggory-0", s.TimeoutConfig.GetTimeout)

			anp := kubernetes.GetAdminNetworkPolicy(t, s.Client, "inline-cidr-as-peers-example", s.TimeoutConfig.GetTimeout)
			mutate := anp.DeepCopy()
			var mask string
			if net.IsIPv4String(serverPodRavenclaw.Status.PodIP) {
				mask = "/32"
			} else {
				mask = "/128"
			}
			// insert new rule at index0; append the rest of the rules in the inline-cidr-as-peers-example
			newRule := []v1alpha1.AdminNetworkPolicyEgressRule{
				{
					Name:   "allow-egress-to-specific-podIPs",
					Action: "Allow",
					To: []v1alpha1.AdminNetworkPolicyEgressPeer{
						{
							Networks: []v1alpha1.CIDR{
								v1alpha1.CIDR(serverPodRavenclaw.Status.PodIP + mask),
								v1alpha1.CIDR(serverPodHufflepuff.Status.PodIP + mask),
							},
						},
					},
				},
			}
			mutate.Spec.Egress = append(newRule, mutate.Spec.Egress...)
			kubernetes.PatchAdminNetworkPolicy(t, s.Client, anp, mutate, s.TimeoutConfig.GetTimeout)
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure egress is ALLOWED to luna-lovegood-0.IP and cedric-diggory-0.IP
			// new egressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodRavenclaw.Status.PodIP, int32(80), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodRavenclaw.Status.PodIP, int32(53), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodRavenclaw.Status.PodIP, int32(9003), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodHufflepuff.Status.PodIP, int32(80), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodHufflepuff.Status.PodIP, int32(53), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodHufflepuff.Status.PodIP, int32(9003), s.TimeoutConfig, true)

			// ensure other pods are still unreachable: luna-lovegood-1.IP and cedric-diggory-1.IP
			// deny at egress rule index2 should kick in
			serverPodRavenclaw = kubernetes.GetPod(t, s.Client, "network-policy-conformance-ravenclaw", "luna-lovegood-1", s.TimeoutConfig.GetTimeout)
			serverPodHufflepuff = kubernetes.GetPod(t, s.Client, "network-policy-conformance-hufflepuff", "cedric-diggory-1", s.TimeoutConfig.GetTimeout)
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure egress is ALLOWED to luna-lovegood-0.IP and cedric-diggory-0.IP
			// new egressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodRavenclaw.Status.PodIP, int32(80), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodRavenclaw.Status.PodIP, int32(53), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodRavenclaw.Status.PodIP, int32(9003), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodHufflepuff.Status.PodIP, int32(80), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodHufflepuff.Status.PodIP, int32(53), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodHufflepuff.Status.PodIP, int32(9003), s.TimeoutConfig, false)
		})
	},
}
