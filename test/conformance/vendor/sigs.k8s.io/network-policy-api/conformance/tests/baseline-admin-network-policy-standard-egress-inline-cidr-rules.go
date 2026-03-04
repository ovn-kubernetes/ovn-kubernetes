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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/net"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/utils/kubernetes"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

func init() {
	ConformanceTests = append(ConformanceTests,
		BaselineAdminNetworkPolicyEgressInlineCIDRPeers,
	)
}

var BaselineAdminNetworkPolicyEgressInlineCIDRPeers = suite.ConformanceTest{
	ShortName:   "BaselineAdminNetworkPolicyEgressInlineCIDRPeers",
	Description: "Tests support for egress traffic to CIDR peers using baseline admin network policy API based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportBaselineAdminNetworkPolicy,
	},
	Manifests: []string{"base/baseline_admin_network_policy/standard-egress-inline-cidr-rules.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {
		ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
		defer cancel()
		t.Run("Should support a 'deny-egress' rule policy for egress-cidr-peer", func(t *testing.T) {
			// harry-potter-1 is our client pod in gryffindor namespace
			// Let us pick a pod in ravenclaw namespace and try to connect, it won't work
			// ensure egress is DENIED to 0.0.0.0/0 from gryffindor; egressRule at index1 should take effect
			// luna-lovegood-0 is our server pod in ravenclaw namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-ravenclaw",
				Name:      "luna-lovegood-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			// Let us pick a pod in hufflepuff namespace and try to connect, it won't work
			// ensure egress is DENIED to 0.0.0.0/0 from gryffindor; egressRule at index1 should take effect
			// cedric-diggory-0 is our server pod in hufflepuff namespace
			serverPod = &v1.Pod{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-hufflepuff",
				Name:      "cedric-diggory-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			// Let us pick a pod in slytherin namespace and try to connect, it will work since we have a higher priority allow rule
			// ensure traffic is allowed to slytherin; egressRule at index0 should take effect
			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod = &v1.Pod{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-slytherin",
				Name:      "draco-malfoy-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPod.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPod.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
		})
		// To test allow CIDR rule, insert the following rule at index0
		//- name: "allow-egress-to-specific-podIPs"
		//  action: "Allow"
		//  to:
		//  - networks:
		//	  - luna-lovegood-0.IP
		//    - cedric-diggory-0.IP
		t.Run("Should support an 'allow-egress' rule policy for egress-cidr-peer", func(t *testing.T) {
			serverPodRavenclaw := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-ravenclaw",
				Name:      "luna-lovegood-0",
			}, serverPodRavenclaw)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			serverPodHufflepuff := &v1.Pod{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-hufflepuff",
				Name:      "cedric-diggory-0",
			}, serverPodHufflepuff)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			banp := &v1alpha1.BaselineAdminNetworkPolicy{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Name: "default",
			}, banp)
			require.NoErrorf(t, err, "unable to fetch the baseline admin network policy")
			mutate := banp.DeepCopy()
			var mask string
			if net.IsIPv4String(serverPodRavenclaw.Status.PodIP) {
				mask = "/32"
			} else {
				mask = "/128"
			}
			// insert new rule at index0; append the rest of the rules in the default BANP
			newRule := []v1alpha1.BaselineAdminNetworkPolicyEgressRule{
				{
					Name:   "allow-egress-to-specific-podIPs",
					Action: "Allow",
					To: []v1alpha1.BaselineAdminNetworkPolicyEgressPeer{
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
			err = s.Client.Patch(ctx, mutate, client.MergeFrom(banp))
			require.NoErrorf(t, err, "unable to patch the baseline admin network policy")
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure egress is ALLOWED to luna-lovegood-0.IP and cedric-diggory-0.IP
			// new egressRule at index0 should take effect
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodRavenclaw.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodRavenclaw.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodRavenclaw.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodHufflepuff.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodHufflepuff.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodHufflepuff.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			// ensure other pods are still unreachable: luna-lovegood-1.IP and cedric-diggory-1.IP
			// deny at egress rule index2 should kick in
			err = s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-ravenclaw",
				Name:      "luna-lovegood-1",
			}, serverPodRavenclaw)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			err = s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-hufflepuff",
				Name:      "cedric-diggory-1",
			}, serverPodHufflepuff)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			// harry-potter-0 is our client pod in gryffindor namespace
			// ensure egress is ALLOWED to luna-lovegood-0.IP and cedric-diggory-0.IP
			// new egressRule at index0 should take effect
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodRavenclaw.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodRavenclaw.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodRavenclaw.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "tcp",
				serverPodHufflepuff.Status.PodIP, int32(80), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "udp",
				serverPodHufflepuff.Status.PodIP, int32(53), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-gryffindor", "harry-potter-1", "sctp",
				serverPodHufflepuff.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
		})
	},
}
