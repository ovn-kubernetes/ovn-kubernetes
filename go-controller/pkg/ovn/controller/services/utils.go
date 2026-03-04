package services

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	globalconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod
func hasHostEndpoints(endpointIPs []string) bool {
	for _, endpointIP := range endpointIPs {
		if IsHostEndpoint(endpointIP) {
			return true
		}
	}
	return false
}

// IsHostEndpoint determines if the given endpoint ip belongs to a host networked pod
func IsHostEndpoint(endpointIP string) bool {
	for _, clusterNet := range globalconfig.Default.ClusterSubnets {
		if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
			return false
		}
	}
	return true
}

func getExternalIDsForLoadBalancer(service *corev1.Service, netInfo util.NetInfo) map[string]string {
	nsn := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}

	externalIDs := map[string]string{
		types.LoadBalancerOwnerExternalID: nsn.String(),
		types.LoadBalancerKindExternalID:  "Service",
	}

	if netInfo.IsDefault() {
		return externalIDs
	}

	externalIDs[types.NetworkExternalID] = netInfo.GetNetworkName()
	externalIDs[types.NetworkRoleExternalID] = util.GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())

	return externalIDs
}
