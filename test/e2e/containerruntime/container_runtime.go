package containerruntime

import (
	"os"
	"strings"
)

type ContainerRuntime string

func (cr ContainerRuntime) String() string {
	return string(cr)
}

const (
	Docker ContainerRuntime = "docker"
	Podman ContainerRuntime = "podman"
)

var runtime ContainerRuntime

func init() {
	if cr, found := os.LookupEnv("CONTAINER_RUNTIME"); found {
		switch strings.ToLower(cr) {
		case Docker.String():
			runtime = Docker
		case Podman.String():
			runtime = Podman
		default:
			panic("unknown container runtime")
		}
	} else {
		runtime = Docker
	}
}

func Get() ContainerRuntime {
	if runtime.String() == "" {
		panic("container runtime is not set")
	}
	return runtime
}
