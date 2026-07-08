// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deploymentconfig

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

var deploymentConfig api.DeploymentConfig

// Set deployment config.
func Set(deployment api.DeploymentConfig) {
	deploymentConfig = deployment
}

// Get deployment config.
func Get() api.DeploymentConfig {
	if deploymentConfig == nil {
		panic("deployment config type not set")
	}
	return deploymentConfig
}

// TryGet returns the deployment config and true if set, nil and false otherwise.
func TryGet() (api.DeploymentConfig, bool) {
	if deploymentConfig == nil {
		return nil, false
	}
	return deploymentConfig, true
}
