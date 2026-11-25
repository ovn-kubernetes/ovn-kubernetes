// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/providers/base"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

type kind struct {
	base.Provider
	engine *container.Engine
}

// Kind CI runs the node shutdown/startup spec only while kind implements this.
var _ api.NodeInfrastructure = &kind{}

func New() api.Provider {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	ce := getContainerRuntime()
	cmdRunner := runner.NewDirectRunner()
	engine := container.NewEngine(ce.String(), cmdRunner)
	return &kind{Provider: base.Provider{ExternalContainerProvider: engine}, engine: engine}
}

func (k *kind) Name() string {
	return "kind"
}

func (k *kind) PrimaryNetwork() (api.Network, error) {
	return k.GetNetwork("kind")
}

func (k *kind) GetK8NodeNetworkInterface(container string, network api.Network) (api.NetworkInterface, error) {
	return k.engine.GetNetworkInterface(container, network.Name())
}

func (k *kind) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	return k.engine.ExecContainerCommand(nodeName, cmd)
}

func (k *kind) PreloadImages(imgs []deploymentconfigapi.ImageConfig) {
	clusterName := kindClusterName()
	if clusterName == "" {
		framework.Logf("Warning: could not determine KIND cluster name, skipping image preload")
		return
	}
	pullBackoff := wait.Backoff{Duration: 5 * time.Second, Factor: 2, Steps: 5}
	for _, img := range imgs {
		framework.Logf("Preloading image %s into KIND cluster %s", img.PullSpec, clusterName)
		var out []byte
		err := wait.ExponentialBackoff(pullBackoff, func() (bool, error) {
			var pullErr error
			out, pullErr = exec.Command(engine.String(), "pull", img.PullSpec).CombinedOutput()
			if pullErr != nil {
				framework.Logf("Retrying pull for image %s: %v (%s)", img.PullSpec, pullErr, out)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			framework.Logf("Warning: failed to pull image %s after retries: %v (%s)", img.PullSpec, err, out)
			continue
		}
		if engine == podman {
			os.Remove("/tmp/image.tar")
			out, err = exec.Command(engine.String(), "save", "-o", "/tmp/image.tar", img.PullSpec).CombinedOutput()
			if err != nil {
				framework.Logf("Warning: failed to save image %s: %v (%s)", img.PullSpec, err, out)
				continue
			}
			out, err = exec.Command("kind", "load", "image-archive", "/tmp/image.tar", "--name", clusterName).CombinedOutput()
		} else {
			out, err = exec.Command("kind", "load", "docker-image", img.PullSpec, "--name", clusterName).CombinedOutput()
		}
		if err != nil {
			framework.Logf("Warning: failed to load image %s into KIND cluster %s: %v (%s)", img.PullSpec, clusterName, err, out)
			continue
		}
		framework.Logf("Preloaded image %s into KIND cluster %s", img.PullSpec, clusterName)
	}
}

func kindClusterName() string {
	currentCtx, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		return ""
	}
	ctx := strings.TrimSpace(string(currentCtx))
	// KIND contexts are named "kind-<cluster-name>"
	if strings.HasPrefix(ctx, "kind-") {
		return strings.TrimPrefix(ctx, "kind-")
	}
	return ""
}

func (k *kind) ShutdownNode(nodeName string) error {
	return k.engine.StopContainer(nodeName)
}

func (k *kind) StartNode(nodeName string) error {
	return k.engine.StartContainer(nodeName)
}

func (k *kind) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	return &base.Context{
		TestContext:                      context,
		ExternalContainerContextProvider: k.engine.WithTestContext(context),
		Provider:                         k,
	}
}
