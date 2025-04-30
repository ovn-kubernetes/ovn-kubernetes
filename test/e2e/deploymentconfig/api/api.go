package api

// DeploymentConfig offers visibility into the configuration OVN-Kubernetes environment for e2e test cases. This includes all host or node level config.
// Remove when OVN-Kubernetes exposes its config via an API.
type DeploymentConfig interface {
	OVNKubernetesNamespace() string
	ExternalBridgeName() string
	PrimaryInterfaceName() string
	SecondaryInterfaceName() string
}
