// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	_ "k8s.io/kubernetes/pkg/features"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// The KUBE_FEATURE_WatchListClient env var alone is insufficient because
	// k8s.io/kubernetes/pkg/features replaces client-go's env-var-based feature gates
	// with utilfeature.DefaultMutableFeatureGate.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": false, "AtomicFIFO": false, "StaleControllerConsistencyJob": false, "StaleControllerConsistencyReplicaSet": false, "StaleControllerConsistencyStatefulSet": false, "StaleControllerConsistencyDaemonSet": false}); err != nil {
		panic(err)
	}
}

func TestNetworkConnectController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OVNKube NetworkConnect Controller Suite")
}
