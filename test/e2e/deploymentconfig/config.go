package deploymentconfig

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig/configs/kind"

	"k8s.io/client-go/rest"
)

var deployment api.DeploymentConfig

func Set(_ *rest.Config) error {
	// upstream currently uses KinD as its preferred platform infra, so if we detect KinD, its upstream
	if kind.IsKind() {
		deployment = kind.New()
	}
	if deployment == nil {
		return fmt.Errorf("failed to determine the deployment config")
	}
	return nil
}

func Get() api.DeploymentConfig {
	if deployment == nil {
		panic("deployment config type not set")
	}
	return deployment
}
