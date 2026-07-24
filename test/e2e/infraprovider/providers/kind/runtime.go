// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"fmt"
	"os"
	"strings"
)

type ContainerRuntime string

func (ce ContainerRuntime) String() string {
	return string(ce)
}

const (
	docker ContainerRuntime = "docker"
	podman ContainerRuntime = "podman"
)

var engine ContainerRuntime

func init() {
	if cr, found := os.LookupEnv("CONTAINER_RUNTIME"); found {
		switch strings.ToLower(cr) {
		case docker.String():
			engine = docker
		case podman.String():
			engine = podman
		default:
			panic(fmt.Sprintf("unknown container engine %q. Supported engines are docker or podman.", cr))
		}
	} else {
		engine = docker
	}
}

func GetContainerRuntime() ContainerRuntime {
	if engine.String() == "" {
		panic("container engine is not set")
	}
	return engine
}
