// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressservice

import (
	"flag"
	"io"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"k8s.io/klog/v2"
)

func TestEgressServiceController(t *testing.T) {
	klog.InitFlags(nil)
	_ = flag.Set("logtostderr", "false")
	klog.SetOutput(io.Discard)
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "EgressService Node Controller Suite")
}
