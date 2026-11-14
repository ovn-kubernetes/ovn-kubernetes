// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"fmt"
	"os"
	"strings"
)

type containerRuntime string

func (ce containerRuntime) String() string {
	return string(ce)
}

const (
	docker containerRuntime = "docker"
	podman containerRuntime = "podman"
)

// EnvContainerRuntime selects the container runtime that holds the cluster's
// node containers. Optional, defaults to docker.
const EnvContainerRuntime = "CONTAINER_RUNTIME"

// parseContainerRuntime resolves name to a supported container runtime. An
// empty name falls back to CONTAINER_RUNTIME, then to docker.
func parseContainerRuntime(name string) (containerRuntime, error) {
	if name == "" {
		name = os.Getenv(EnvContainerRuntime)
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return docker, nil
	case docker.String():
		return docker, nil
	case podman.String():
		return podman, nil
	default:
		return "", fmt.Errorf("unknown container runtime %q, supported runtimes are %s or %s", name, docker, podman)
	}
}
