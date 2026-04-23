// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func TestAdder(t *testing.T) {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets don't support WatchList semantics introduced in K8s 1.35,
	// causing informers to hang waiting for bookmark events.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "EgressIP Controller Suite")
}
