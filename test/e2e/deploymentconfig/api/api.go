// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package api

// Config represents a deployment configuration flag queryable by E2E tests.
// This interface replaces direct ENV variable checks, which don't work for all providers
// (some use CRDs, ConfigMaps, or other mechanisms).
// Add all new feature flags as Config constants and check via IsConfigurationEnabled().
type Config string

const (
	// L3UDNMultiSubnetConfig indicates whether L3 User Defined Network multi-subnet support is enabled
	L3UDNMultiSubnetConfig Config = "L3UDNMultiSubnet"
	// NoOverlay indicates whether the default network uses the no-overlay transport
	NoOverlay Config = "NoOverlay"
	// LinkLocalForwardingDisabled indicates whether the disable-link-local-forwarding gateway option is set
	LinkLocalForwardingDisabled Config = "LinkLocalForwardingDisabled"
)

type ImageID int

type ImageConfig struct {
	ImageID  ImageID
	PullSpec string
}

// Image IDs for OVN-Kubernetes e2e test images
const (
	Agnhost ImageID = iota
	Netshoot
	Nginx
	FRR
	DNSMasq
	FedoraKubevirtContainerDisk
	MaxDefaultImageIDs
)

// DeploymentConfig offers visibility into the configuration OVN-Kubernetes environment for e2e test cases. This includes all host or node level config.
// Remove when OVN-Kubernetes exposes its config via an API.
type DeploymentConfig interface {
	OVNKubernetesNamespace() string
	FRRK8sNamespace() string
	ExternalBridgeName() string
	PrimaryInterfaceName() string
	GetAgnHostContainerImage() string
	// IsConfigurationEnabled checks whether a specific configuration flag is enabled in the deployment.
	IsConfigurationEnabled(config Config) bool
}
