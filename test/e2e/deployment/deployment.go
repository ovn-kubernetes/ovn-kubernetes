package deployment

import (
	"fmt"
	"os/exec"
	"strings"
)

// Deployment offers the configuration options of OVN-Kubernetes to e2e test cases
type Deployment interface {
	OVNKubernetesNamespace() string
	ExternalBridgeName() string
}

var deployment Deployment

func Set() {
	// upstream currently uses KinD as its preferred platform infra, so if we detect KinD, its upstream
	if isKind() {
		deployment = newUpstream()
	}
	if isOpenShift() {
		deployment = newOpenshift()
	}
	if deployment == nil {
		panic("failed to determine the deployment")
	}
}

func Get() Deployment {
	if deployment == nil {
		panic("deployment type not set")
	}
	return deployment
}

func isKind() bool {
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

func isOpenShift() bool {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		panic("failed to find kubectl in PATH")
	}
	crdName := "infrastructures.deployment.openshift.io"
	outBytes, err := exec.Command("kubectl", "get", "crd").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("failed to list CRDs: %v", err))
	}
	return strings.Contains(string(outBytes), crdName)
}
