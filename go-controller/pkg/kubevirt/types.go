package kubevirt

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnZoneExternalIDKey = types.OvnK8sPrefix + "/zone"
	OvnRemoteZone        = "remote"
	OvnLocalZone         = "local"

	NamespaceExternalIDsKey      = "k8s.ovn.org/namespace"
	VirtualMachineExternalIDsKey = "k8s.ovn.org/vm"
)

// DefaultGatewayReconciler implements a pair of Reconcile functions for
// ipv4 and ipv6 to update VM's default gw config after live migration
type DefaultGatewayReconciler struct {
	watchFactory  *factory.WatchFactory
	netInfo       util.NetInfo
	interfaceName string
}

func NewDefaultGatewayReconciler(watchFactory *factory.WatchFactory, netInfo util.NetInfo, interfaceName string) *DefaultGatewayReconciler {
	return &DefaultGatewayReconciler{
		watchFactory:  watchFactory,
		netInfo:       netInfo,
		interfaceName: interfaceName,
	}
}
