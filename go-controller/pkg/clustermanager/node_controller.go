package clustermanager

import (
	corev1 "k8s.io/api/core/v1"

	sharednode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
)

type clusterManagerNodeController struct {
	*sharednode.NodeController
}

type clusterManagerNodePolicy struct{}

func (clusterManagerNodePolicy) NodeHasNetwork(_, _ string) bool {
	return true
}

func (clusterManagerNodePolicy) ShouldFilterByRemoteNetworkActivity(_ *corev1.Node) bool {
	return false
}

func newClusterManagerNodeController(wf *factory.WatchFactory) *clusterManagerNodeController {
	return &clusterManagerNodeController{
		NodeController: sharednode.NewController(wf, "clustermanager-node", clusterManagerNodePolicy{}),
	}
}
