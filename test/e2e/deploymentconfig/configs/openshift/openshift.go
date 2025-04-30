package openshift

import (
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

func IsOpenshift() bool {
	return true
}

type openshift struct{}

func New() api.DeploymentConfig {
	return &openshift{}
}

func (o openshift) OVNKubernetesNamespace() string {
	return "openshift-ovn-kubernetes"
}

func (o openshift) ExternalBridgeName() string {
	return "br-ex"
}

func (o openshift) PrimaryInterfaceName() string {
	return "ens3"
}
func (o openshift) SecondaryInterfaceName() string {
	return "ens4"
}
