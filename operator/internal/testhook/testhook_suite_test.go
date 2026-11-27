// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testhook_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTesthook(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Testhook Suite")
}
