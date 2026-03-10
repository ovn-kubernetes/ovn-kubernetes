// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import "testing"

func TestCleanupNoOverlaySNATExemptionAddressSetNilFactory(t *testing.T) {
	if err := cleanupNoOverlaySNATExemptionAddressSet(nil, nil, ""); err != nil {
		t.Fatalf("expected nil factory cleanup to be a no-op, got: %v", err)
	}
}
