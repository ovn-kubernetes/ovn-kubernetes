package services

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// hasHostEndpoints determines if an LBEndpointsList contains at least one host networked endpoint.
func hasHostEndpoints(lbes util.LBEndpointsList) bool {
	for _, lbe := range lbes {
		for _, endpointIP := range lbe.V4IPs {
			if IsHostEndpoint(endpointIP) {
				return true
			}
		}
		for _, endpointIP := range lbe.V6IPs {
			if IsHostEndpoint(endpointIP) {
				return true
			}
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
