package deploymentconfig

import (
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/configs/kind"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/configs/openshift"
)

var deployment api.DeploymentConfig

func Set() {
	if openshift.IsOpenshift() {
		deployment = openshift.New()
		// upstream currently uses KinD as its preferred platform infra, so if we detect KinD, its upstream
	} else if kind.IsKind() {
		deployment = kind.New()
	}
	if deployment == nil {
		panic("failed to determine the deployment config")
	}
}

func Get() api.DeploymentConfig {
	if deployment == nil {
		panic("deployment config type not set")
	}
	return deployment
}
