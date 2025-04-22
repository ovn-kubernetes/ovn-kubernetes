// SPDX-License-Identifier:Apache-2.0

package helpers

import (
	"os"
	"os/exec"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

type Executor interface {
	Exec(cmd string, args ...string) (string, error)
}

type hostExecutor struct{}

var (
	Host             hostExecutor
	ContainerRuntime = "docker"
)

func init() {
	if cr := os.Getenv("CONTAINER_RUNTIME"); len(cr) != 0 {
		ContainerRuntime = cr
	}
}

func (hostExecutor) Exec(cmd string, args ...string) (string, error) {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	return string(out), err
}

func ForContainer(containerName string) Executor {
	return &ContainerExecutor{Container: containerName}
}

type ContainerExecutor struct {
	Container string
}

func (e *ContainerExecutor) Exec(cmd string, args ...string) (string, error) {
	newArgs := append([]string{"exec", e.Container, cmd}, args...)
	out, err := exec.Command(ContainerRuntime, newArgs...).CombinedOutput()
	return string(out), err
}

type podExecutor struct {
	namespace string
	name      string
	container string
}

func ForPod(namespace, name, container string) Executor {
	return &podExecutor{
		namespace: namespace,
		name:      name,
		container: container,
	}
}

func (p *podExecutor) Exec(cmd string, args ...string) (string, error) {
	fullArgs := append([]string{"exec", p.name, "-c", p.container, "--", cmd}, args...)
	res, err := e2ekubectl.RunKubectl(p.namespace, fullArgs...)
	if err != nil {
		return "", err
	}
	return res, nil
}
