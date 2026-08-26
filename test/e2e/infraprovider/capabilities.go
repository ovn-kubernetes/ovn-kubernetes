// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package infraprovider

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

// capabilityLister is implemented by providers that expose unsupported features.
type capabilityLister interface {
	UnsupportedCapabilities() []string
}

// Supports reports whether the active infra provider implements capability.
// Providers without UnsupportedCapabilities() are treated as fully capable.
func Supports(capability string) bool {
	p := Get()
	lister, ok := p.(capabilityLister)
	if !ok {
		return true
	}
	for _, unsupported := range lister.UnsupportedCapabilities() {
		if unsupported == capability {
			return false
		}
	}
	return true
}

// SupportsSetupUnderlay is a convenience wrapper for the common underlay gate.
func SupportsSetupUnderlay() bool {
	return Supports(api.CapabilitySetupUnderlay)
}
