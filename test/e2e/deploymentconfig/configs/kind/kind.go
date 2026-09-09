// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

type kind struct {
	requiredImages map[api.ImageID]struct{}
}

func New() api.DeploymentConfig {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	return &kind{
		requiredImages: make(map[api.ImageID]struct{}),
	}
}

func (k *kind) OVNKubernetesNamespace() string {
	return "ovn-kubernetes"
}

func (k *kind) FRRK8sNamespace() string {
	return "frr-k8s-system"
}

func (k *kind) ExternalBridgeName() string {
	return "breth0"
}

func (k *kind) PrimaryInterfaceName() string {
	return "eth0"
}

func (k *kind) IsConfigurationEnabled(config api.Config) bool {
	switch config {
	case api.L3UDNMultiSubnetConfig:
		// Currently enabled by default for Kind cluster. Could use
		// an ENV variable check instead if we need variability later.
		return true
	default:
		return false
	}
}

func (k *kind) NBDBContainerName() string {
	return "nb-ovsdb"
}

func (k *kind) GetImage(imageID api.ImageID) string {
	return images.GetImageConfigs()[imageID]
}

func (k *kind) AddImage(imageID ...api.ImageID) {
	for _, imgID := range imageID {
		k.requiredImages[imgID] = struct{}{}
	}
}

func (k *kind) GetRequiredImages() []api.ImageConfig {
	k.AddImage(images.Agnhost)
	imageConfigs := make([]api.ImageConfig, 0, len(k.requiredImages))
	for imageID := range k.requiredImages {
		imageConfigs = append(imageConfigs, api.ImageConfig{
			ImageID:  imageID,
			PullSpec: k.GetImage(imageID),
		})
	}
	return imageConfigs
}
