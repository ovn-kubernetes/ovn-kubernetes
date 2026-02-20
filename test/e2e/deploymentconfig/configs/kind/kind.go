package kind

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

func IsKind() bool {
	_, err := exec.LookPath("kind")
	if err != nil {
		return false
	}
	outBytes, err := exec.Command("kind", "get", "clusters").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("failed to get KinD clusters: stdout: %q, err: %v", string(outBytes), err))
	}
	if strings.Contains(string(outBytes), "ovn") {
		return true
	}
	return false
}

type kind struct{}

func New() api.DeploymentConfig {
	return kind{}
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
