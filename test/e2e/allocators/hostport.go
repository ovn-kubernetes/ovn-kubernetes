// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/kubernetes/test/e2e/framework"
)

const hostPortKey = "host-port"

// AllocateHostPort returns a port in [start, end] via ConfigMap-backed AllocateInt.
func AllocateHostPort(f *framework.Framework, start, end uint16) (uint16, error) {
	if end < start {
		return 0, fmt.Errorf("invalid port range [%d, %d]", start, end)
	}
	n, err := AllocateInt(f, hostPortKey, int(end)-int(start)+1)
	if err != nil {
		return 0, err
	}
	ginkgo.DeferCleanup(func() error {
		return DeallocateInt(f, hostPortKey, n)
	})
	return start + uint16(n-1), nil
}
