package infraprovider

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/providers/kind"

	"k8s.io/client-go/rest"
)

type Name string

func (n Name) String() string {
	return string(n)
}

var provider api.Provider

// Set detects which infrastructure provider. Arg config is not needed for KinD provider but downstream implementations
// will require access to the kapi to infer what platform k8 is running on.
func Set(_ *rest.Config) error {
	// detect if the provider is KinD
	if kind.IsProvider() {
		provider = kind.New()
	}
	if provider == nil {
		return fmt.Errorf("failed to determine the infrastructure provider")
	}
	return nil
}

func Get() api.Provider {
	if provider == nil {
		panic("provider not set")
	}
	return provider
}
