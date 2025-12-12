package deploymentconfig

import (
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/api"
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
