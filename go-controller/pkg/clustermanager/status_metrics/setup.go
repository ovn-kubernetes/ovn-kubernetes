// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

// RegisterConfiguredRollers registers resource rollers for features that are
// enabled and implemented. Individual CRD commits append registrations here.
func RegisterConfiguredRollers(m *Manager) {
	if m == nil {
		return
	}
	// Rollers are added in subsequent commits:
	// - EgressFirewall
	// - EgressQoS
	// - NetworkQoS
	// - AdminPolicyBasedExternalRoute
	// - AdminNetworkPolicy / BaselineAdminNetworkPolicy
}
