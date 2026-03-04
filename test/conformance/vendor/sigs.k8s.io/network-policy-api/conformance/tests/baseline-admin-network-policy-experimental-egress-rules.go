/*
Copyright 2024 The Kubernetes Authors.

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

	"sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/utils/kubernetes"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

func init() {
	ConformanceTests = append(ConformanceTests,
		BaselineAdminNetworkPolicyEgressNamedPort,
		BaselineAdminNetworkPolicyEgressNodePeers,
	)
}

var BaselineAdminNetworkPolicyEgressNamedPort = suite.ConformanceTest{
	ShortName:   "BaselineAdminNetworkPolicyEgressNamedPort",
	Description: "Tests support for egress traffic on a named port using baseline admin network policy API based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportBaselineAdminNetworkPolicy,
		suite.SupportBaselineAdminNetworkPolicyNamedPorts,
	},
	Manifests: []string{"base/baseline_admin_network_policy/standard-egress-udp-rules.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {

		t.Run("Should support an 'allow-egress' policy for named port", func(t *testing.T) {
			// This test uses `default` BANP
			// harry-potter-1 is our server pod in gryffindor namespace
			serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-gryffindor", "harry-potter-1", s.TimeoutConfig.GetTimeout)

			banp := kubernetes.GetBaselineAdminNetworkPolicy(t, s.Client, "default", s.TimeoutConfig.GetTimeout)
			mutate := banp.DeepCopy()
			dnsPortRule := mutate.Spec.Egress[3]
			dnsPort := "dns"
			// rewrite the udp port 53 rule as named port rule
			dnsPortRule.Ports = &[]v1alpha1.AdminNetworkPolicyPort{
				{
					NamedPort: &dnsPort,
				},
			}
			mutate.Spec.Egress[3] = dnsPortRule
			kubernetes.PatchBaselineAdminNetworkPolicy(t, s.Client, banp, mutate, s.TimeoutConfig.GetTimeout)
			// cedric-diggory-0 is our client pod in hufflepuff namespace
			// ensure egress is ALLOWED to gryffindor from hufflepuff at the dns port, which is defined as UDP at port 53 in pod spec
			// modified ingressRule at index3 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-hufflepuff", "cedric-diggory-0", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig, true)
			// cedric-diggory-1 is our client pod in hufflepuff namespace
			// ensure egress is DENIED to gryffindor from hufflepuff for rest of the traffic; egressRule at index4 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-hufflepuff", "cedric-diggory-1", "udp",
				serverPod.Status.PodIP, int32(5353), s.TimeoutConfig, false)
		})
	},
}

var BaselineAdminNetworkPolicyEgressNodePeers = suite.ConformanceTest{
	ShortName:   "BaselineAdminNetworkPolicyEgressNodePeers",
	Description: "Tests support for egress traffic to node peers using  baseline admin network policy API based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportBaselineAdminNetworkPolicy,
		suite.SupportBaselineAdminNetworkPolicyEgressNodePeers,
	},
	Manifests: []string{"base/baseline_admin_network_policy/experimental-egress-selector-rules.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {
		// centaur-1 is our server host-networked pod in forbidden-forrest namespace
		serverPod := kubernetes.GetPod(t, s.Client, "network-policy-conformance-forbidden-forrest", "centaur-1", s.TimeoutConfig.GetTimeout)
		t.Run("Should support an 'allow-egress' rule policy for egress-node-peer", func(t *testing.T) {
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure egress is ALLOWED to forbidden-forrest from gryffindor at the s.HostNetworkPorts[0] TCP port
			// egressRule at index0 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-0", "tcp",
				serverPod.Status.PodIP, int32(s.HostNetworkPorts[0]), s.TimeoutConfig, true)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(s.HostNetworkPorts[1]), s.TimeoutConfig, true) // Pass rule at index2 takes effect
		})

		t.Run("Should support a 'deny-egress' rule policy for egress-node-peer", func(t *testing.T) {
			// harry-potter-1 is our client pod in gryffindor namespace
			// ensure egress is DENIED to rest of the nodes from gryffindor; egressRule at index1 should take effect
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(s.HostNetworkPorts[4]), s.TimeoutConfig, false)
			kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(s.HostNetworkPorts[6]), s.TimeoutConfig, false)
		})
	},
}
