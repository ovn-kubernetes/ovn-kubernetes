// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDPUSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPU Suite")
}
