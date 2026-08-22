// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/utils/image"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

type kind struct {
	kubeConfig *rest.Config
}

func New(config *rest.Config) api.DeploymentConfig {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	return kind{kubeConfig: config}
}

func (k kind) OVNKubernetesNamespace() string {
	return "ovn-kubernetes"
}

func (k kind) FRRK8sNamespace() string {
	return "frr-k8s-system"
}

func (k kind) ExternalBridgeName() string {
	return "breth0"
}

func (k kind) PrimaryInterfaceName() string {
	return "eth0"
}

func (k kind) GetAgnHostContainerImage() string {
	return image.GetE2EImage(image.Agnhost)
}

func (k kind) IsConfigurationEnabled(config api.Config) bool {
	switch config {
	case api.L3UDNMultiSubnetConfig:
		// Currently enabled by default for Kind cluster. Could use
		// an ENV variable check instead if we need variability later.
		return true
	default:
		return false
	}
}

func (k kind) GetMachineNetworkSubnets() (ipv4, ipv6 sets.Set[string], err error) {
	return deploymentconfig.GetMachineNetworkSubnets(k.kubeConfig)
}
