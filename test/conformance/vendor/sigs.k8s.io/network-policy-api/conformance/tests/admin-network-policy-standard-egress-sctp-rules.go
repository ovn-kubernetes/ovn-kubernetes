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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/utils/kubernetes"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

func init() {
	ConformanceTests = append(ConformanceTests,
		AdminNetworkPolicyEgressSCTP,
	)
}

var AdminNetworkPolicyEgressSCTP = suite.ConformanceTest{
	ShortName:   "AdminNetworkPolicyEgressSCTP",
	Description: "Tests support for egress traffic (SCTP protocol) using admin network policy API based on a server and client model",
	Features: []suite.SupportedFeature{
		suite.SupportAdminNetworkPolicy,
	},
	Manifests: []string{"base/admin_network_policy/standard-egress-sctp-rules.yaml"},
	Test: func(t *testing.T, s *suite.ConformanceTestSuite) {

		t.Run("Should support an 'allow-egress' policy for SCTP protocol; ensure rule ordering is respected", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// harry-potter-0 is our server pod in gryffindor namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-gryffindor",
				Name:      "harry-potter-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			// luna-lovegood-0 is our client pod in ravenclaw namespace
			// ensure egress is ALLOWED to gryffindor from ravenclaw
			// egressRule at index0 will take precedence over egressRule at index1; thus ALLOW takes precedence over DENY since rules are ordered
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
		})

		t.Run("Should support an 'allow-egress' policy for SCTP protocol at the specified port", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// cedric-diggory-1 is our server pod in hufflepuff namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-hufflepuff",
				Name:      "cedric-diggory-1",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			// luna-lovegood-0 is our client pod in ravenclaw namespace
			// ensure egress is ALLOWED to hufflepuff from ravenclaw at port 9003; egressRule at index5 should take effect
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			// ensure egress is DENIED to hufflepuff from ravenclaw for rest of the traffic; egressRule at index6 should take effect
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
		})

		t.Run("Should support an 'deny-egress' policy for SCTP protocol; ensure rule ordering is respected", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// harry-potter-0 is our server pod in gryffindor namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-gryffindor",
				Name:      "harry-potter-1",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			anp := &v1alpha1.AdminNetworkPolicy{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Name: "egress-sctp",
			}, anp)
			require.NoErrorf(t, err, "unable to fetch the admin network policy")
			mutate := anp.DeepCopy()
			// swap rules at index0 and index1
			allowRule := mutate.Spec.Egress[0]
			mutate.Spec.Egress[0] = mutate.Spec.Egress[1]
			mutate.Spec.Egress[1] = allowRule
			err = s.Client.Patch(ctx, mutate, client.MergeFrom(anp))
			require.NoErrorf(t, err, "unable to patch the admin network policy")
			// luna-lovegood-0 is our client pod in gryffindor namespace
			// ensure egress is DENIED to gryffindor from ravenclaw
			// egressRule at index0 will take precedence over egressRule at index1; thus DENY takes precedence over ALLOW since rules are ordered
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
		})

		t.Run("Should support a 'deny-egress' policy for SCTP protocol at the specified port", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-slytherin",
				Name:      "draco-malfoy-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			// luna-lovegood-0 is our client pod in ravenclaw namespace
			// ensure egress to slytherin is DENIED from ravenclaw at port 9003; egressRule at index3 should take effect
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, false)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			// ensure egress to slytherin is ALLOWED from ravenclaw for rest of the traffic; matches no rules hence allowed
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
		})

		t.Run("Should support an 'pass-egress' policy for SCTP protocol; ensure rule ordering is respected", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// harry-potter-0 is our server pod in gryffindor namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-gryffindor",
				Name:      "harry-potter-1",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			anp := &v1alpha1.AdminNetworkPolicy{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Name: "egress-sctp",
			}, anp)
			require.NoErrorf(t, err, "unable to fetch the admin network policy")
			mutate := anp.DeepCopy()
			// swap rules at index0 and index2
			denyRule := mutate.Spec.Egress[0]
			mutate.Spec.Egress[0] = mutate.Spec.Egress[2]
			mutate.Spec.Egress[2] = denyRule
			err = s.Client.Patch(ctx, mutate, client.MergeFrom(anp))
			require.NoErrorf(t, err, "unable to patch the admin network policy")
			// luna-lovegood-0 is our client pod in ravenclaw namespace
			// ensure egress is PASSED from gryffindor to ravenclaw
			// egressRule at index0 will take precedence over egressRule at index1&index2; thus PASS takes precedence over ALLOW/DENY since rules are ordered
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
		})

		t.Run("Should support a 'pass-egress' policy for SCTP protocol at the specified port", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), s.TimeoutConfig.GetTimeout)
			defer cancel()
			// This test uses `egress-sctp` ANP
			// draco-malfoy-0 is our server pod in slytherin namespace
			serverPod := &v1.Pod{}
			err := s.Client.Get(ctx, client.ObjectKey{
				Namespace: "network-policy-conformance-slytherin",
				Name:      "draco-malfoy-0",
			}, serverPod)
			require.NoErrorf(t, err, "unable to fetch the server pod")
			anp := &v1alpha1.AdminNetworkPolicy{}
			err = s.Client.Get(ctx, client.ObjectKey{
				Name: "egress-sctp",
			}, anp)
			require.NoErrorf(t, err, "unable to fetch the admin network policy")
			mutate := anp.DeepCopy()
			// swap rules at index3 and index4
			denyRule := mutate.Spec.Egress[3]
			mutate.Spec.Egress[3] = mutate.Spec.Egress[4]
			mutate.Spec.Egress[4] = denyRule
			err = s.Client.Patch(ctx, mutate, client.MergeFrom(anp))
			require.NoErrorf(t, err, "unable to patch the admin network policy")
			// luna-lovegood-0 is our client pod in ravenclaw namespace
			// ensure egress to slytherin is PASSED from ravenclaw at port 9003; egressRule at index3 should take effect
			success := kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-0", "sctp",
				serverPod.Status.PodIP, int32(9003), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
			// luna-lovegood-1 is our client pod in ravenclaw namespace
			// ensure egress to slytherin is ALLOWED from ravenclaw for rest of the traffic; matches no rules hence allowed
			success = kubernetes.PokeServer(t, s.ClientSet, &s.KubeConfig, "network-policy-conformance-ravenclaw", "luna-lovegood-1", "sctp",
				serverPod.Status.PodIP, int32(9005), s.TimeoutConfig.RequestTimeout, true)
			assert.True(t, success)
		})
	},
}
