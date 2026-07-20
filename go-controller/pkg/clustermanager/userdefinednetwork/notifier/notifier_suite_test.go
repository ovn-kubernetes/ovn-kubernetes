// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package notifier

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNotifier(t *testing.T) {
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")
	RegisterFailHandler(Fail)
	RunSpecs(t, "Notifier Suite")
}
