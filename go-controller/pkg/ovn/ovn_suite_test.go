package ovn

import (
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
)

func TestClusterNode(t *testing.T) {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// The KUBE_FEATURE_WatchListClient env var alone is insufficient because
	// k8s.io/kubernetes/pkg/features replaces client-go's env-var-based feature gates
	// with utilfeature.DefaultMutableFeatureGate.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": false}); err != nil {
		t.Fatalf("Failed to disable WatchListClient feature gate: %v", err)
	}
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "OVN Operations Suite")
}
