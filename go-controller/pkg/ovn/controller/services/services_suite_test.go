// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"fmt"
	"os"
	"strings"
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
)

func TestMain(m *testing.M) {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// The KUBE_FEATURE_WatchListClient env var is needed for client-go's default
	// env-var feature gate. When k8s.io/kubernetes/pkg/features is linked into a
	// test binary, client-go's gates are replaced by utilfeature.DefaultMutableFeatureGate,
	// so set that as well when the feature has been registered.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	os.Setenv("KUBE_FEATURE_AtomicFIFO", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyDaemonSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyStatefulSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyReplicaSet", "false")
	os.Setenv("KUBE_FEATURE_StaleControllerConsistencyJob", "false")
	if err := os.Setenv("KUBE_FEATURE_WatchListClient", "false"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set WatchListClient feature gate env var: %v\n", err)
		os.Exit(1)
	}
	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": false, "AtomicFIFO": false, "StaleControllerConsistencyJob": false, "StaleControllerConsistencyReplicaSet": false, "StaleControllerConsistencyStatefulSet": false, "StaleControllerConsistencyDaemonSet": false}); err != nil &&
		!strings.Contains(err.Error(), "unrecognized feature gate") {
		fmt.Fprintf(os.Stderr, "Failed to disable WatchListClient feature gate: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
