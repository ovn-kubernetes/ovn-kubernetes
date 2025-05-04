package containerengine

import (
	"fmt"
	"os"
	"strings"
)

type ContainerEngine string

func (ce ContainerEngine) String() string {
	return string(ce)
}

const (
	Docker ContainerEngine = "docker"
	Podman ContainerEngine = "podman"
)

var engine ContainerEngine

func init() {
	if cr, found := os.LookupEnv("CONTAINER_RUNTIME"); found {
		switch strings.ToLower(cr) {
		case Docker.String():
			engine = Docker
		case Podman.String():
			engine = Podman
		default:
			panic(fmt.Sprintf("unknown container engine %q. Supported engines are docker or podman.", cr))
		}
	} else {
		engine = Docker
	}
}

func Get() ContainerEngine {
	if engine.String() == "" {
		panic("container engine is not set")
	}
	return engine
}
