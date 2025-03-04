package label

import "github.com/onsi/ginkgo/v2"

func ComponentName() ginkgo.Labels {
	return NewComponent("ovn-kubernetes")
}
