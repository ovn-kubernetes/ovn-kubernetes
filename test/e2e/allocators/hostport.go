// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"fmt"

	"k8s.io/kubernetes/test/e2e/framework"

	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const hostPortKey = "host-port"

// AllocateHostPort returns a port in [start, end] via ConfigMap-backed AllocateInt.
// Deallocation is registered as a cleanup function.
func AllocateHostPort(f *framework.Framework, cleanup infraapi.ContextCleanUp, start, end uint16) (uint16, error) {
	if end < start {
		return 0, fmt.Errorf("invalid port range [%d, %d]", start, end)
	}
	n, err := AllocateInt(f, hostPortKey, int(end)-int(start)+1)
	if err != nil {
		return 0, err
	}
	cleanup.AddCleanUpFn(func() error {
		return DeallocateInt(f, hostPortKey, n)
	})
	return start + uint16(n-1), nil
}
