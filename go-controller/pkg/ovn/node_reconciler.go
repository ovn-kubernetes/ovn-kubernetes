package ovn

import topologycontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/topology"

// NodeReconciler queues node keys for reconciliation.
// Used by UDN controllers to requeue node work without per-network node watchers.
type NodeReconciler interface {
	Reconcile(key string)
	RegisterNetworkController(handler topologycontroller.NodeHandler)
	DeregisterNetworkController(netName string)
}
