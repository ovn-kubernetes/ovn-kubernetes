/*
Copyright 2022 The Kubernetes Authors.

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

	"sigs.k8s.io/network-policy-api/conformance/utils/kubernetes"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

func init() {
	ConformanceTests = append(ConformanceTests,
		AdminNetworkPolicyPriorityField,
	)
}

var AdminNetworkPolicyPriorityField = suite.ConformanceTest{
	ShortName:   "AdminNetworkPolicyPriorityField",
	Description: "Tests support for admin network policy API's .spec.priority field based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportAdminNetworkPolicy,
		suite.SupportBaselineAdminNetworkPolicy, // priority change of ANP should play well with existing BANP's
	},
	Manifests: []string{"base/admin_network_policy/standard-priority-field.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {

		t.Run("Should Deny traffic from slytherin to gryffindor respecting ANP", func(t *testing.T) {
			// This test uses `priority-50-example` ANP; takes precedence over old-priority-60-new-priority-40-example ANP
			// harry-potter-0 is our server pod in gryffindor namespace
			serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-gryffindor", "harry-potter-0", s.TimeoutConfig.GetTimeout)
			// draco-malfoy-0 is our client pod in slytherin namespace
			// ensure ingress is DENIED to gryffindor from slytherin
			// inressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-slytherin", "draco-malfoy-0", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, false)
			// draco-malfoy-1 is our client pod in slytherin namespace
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-slytherin", "draco-malfoy-1", "tcp",
				serverPod.Status.PodIP, int32(8080), s.TimeoutConfig, false)
		})

		t.Run("Should Deny traffic to slytherin from gryffindor respecting ANP", func(t *testing.T) {
			// This test uses `priority-50-example` ANP; takes precedence over old-priority-60-new-priority-40-example ANP
			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-slytherin", "draco-malfoy-0", s.TimeoutConfig.GetTimeout)
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure ingress is DENIED to gryffindor from slytherin
			// egressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-0", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, false)
			// harry-potter-1 is our client pod in gryffindor namespace
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(8080), s.TimeoutConfig, false)
		})

		t.Run("Should respect ANP priority field; thus passing both ingress and egress traffic over to BANP", func(t *testing.T) {
			// This test uses `old-priority-60-new-priority-40-example` ANP
			anp := kubernetes.GetAdminNetworkPolicy(t, s.Client, "old-priority-60-new-priority-40-example", s.TimeoutConfig.GetTimeout)
			mutate := anp.DeepCopy()
			// change priority from 60 to 40
			mutate.Spec.Priority = 40
			kubernetes.PatchAdminNetworkPolicy(t, s.Client, anp, mutate, s.TimeoutConfig.GetTimeout)
			// harry-potter-0 is our server pod in gryffindor namespace
			serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-gryffindor", "harry-potter-0", s.TimeoutConfig.GetTimeout)
			// draco-malfoy-0 is our client pod in slytherin namespace
			// ensure ingress is PASSED to gryffindor from slytherin - the baseline admin network policy ALLOW should take effect
			// inressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-slytherin", "draco-malfoy-0", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, true)
			// draco-malfoy-1 is our client pod in slytherin namespace
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-slytherin", "draco-malfoy-1", "tcp",
				serverPod.Status.PodIP, int32(8080), s.TimeoutConfig, true)

			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod = kubernetes.GetPod(t, s.Client, "network-policy-conformance-slytherin", "draco-malfoy-0", s.TimeoutConfig.GetTimeout)
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure ingress is PASSED to gryffindor from slytherin - the baseline admin network policy ALLOW should take effect
			// egressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-0", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig, true)
			// harry-potter-1 is our client pod in gryffindor namespace
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(8080), s.TimeoutConfig, true)
		})
	},
}
