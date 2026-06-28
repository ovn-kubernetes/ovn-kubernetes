// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	os.Setenv("KUBE_FEATURE_AtomicFIFO", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyDaemonSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyStatefulSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyReplicaSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyJob", "false")
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
}

func TestClusterManager(t *testing.T) {
	// Disable WatchListClient for this suite. Fake NAD clients used in tests
	// don't provide watchlist bookmarks and informer sync may time out.
	if err := os.Setenv("KUBE_FEATURE_WatchListClient", "false"); err != nil {
		t.Fatalf("Failed to set KUBE_FEATURE_WatchListClient: %v", err)
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster Manager Operations Suite")
}
