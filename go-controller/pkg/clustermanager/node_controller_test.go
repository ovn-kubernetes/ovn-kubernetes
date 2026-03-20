package clustermanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestClusterManagerNodePolicyAlwaysActive(t *testing.T) {
	policy := clusterManagerNodePolicy{}
	if !policy.NodeHasNetwork("node-a", "net-a") {
		t.Fatal("expected cluster-manager node policy to always treat nodes as active")
	}
}

func TestClusterManagerNodePolicyNeverFiltersRemoteActivity(t *testing.T) {
	policy := clusterManagerNodePolicy{}
	if policy.ShouldFilterByRemoteNetworkActivity(&corev1.Node{}) {
		t.Fatal("expected cluster-manager node policy to never filter remote activity")
	}
}
