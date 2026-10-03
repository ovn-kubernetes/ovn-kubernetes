// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer_test

import (
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRenderer(t *testing.T) {
	defaults.OvnKubeImageRepository = "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu"
	defaults.OvnKubeImageTag = "3f14ccbb-50100192-7f2439e"
	defaults.OvnKubeImageDigest = "sha256:f3532b6b7513fb4add4df4878dac7251170a38935df0ae841b0767decbcf445f"

	RegisterFailHandler(Fail)
	RunSpecs(t, "Renderer Suite")
}
