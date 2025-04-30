package infraprovider

import (
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/providers/kind"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/providers/openshift"

	"k8s.io/client-go/rest"
)

type Name string

func (n Name) String() string {
	return string(n)
}

var provider api.Provider

// Set detects which infrastructure provider. Arg config is not needed for KinD provider but downstream implementations
// will require access to the kapi to infer what platform k8 is running on.
func Set(_ *rest.Config) {
	if openshift.IsOpenshift() {
		provider = openshift.New()
		// detect if the provider is KinD
	} else if kind.IsKinD() {
		provider = kind.New()
	}
	if provider == nil {
		panic("failed to determine the infrastructure provider")
	}
}

func Get() api.Provider {
	if provider == nil {
		panic("provider not set")
	}
	return provider
}
