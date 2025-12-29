package services

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod.
func hasHostEndpoints(endpointIPs []string, netInfo util.NetInfo) bool {
	for _, endpointIP := range endpointIPs {
		if IsHostEndpoint(endpointIP, netInfo) {
			return true
		}
	}
	return false
}

// IsHostEndpoint determines if the given endpoint ip belongs to a host networked pod.
// Checks against netInfo.Subnets() which returns the network's subnets
// (for default network this is config.Default.ClusterSubnets, for UDNs it's the UDN subnets).
func IsHostEndpoint(endpointIP string, netInfo util.NetInfo) bool {
	ip := net.ParseIP(endpointIP)
	for _, subnet := range netInfo.Subnets() {
		if subnet.CIDR.Contains(ip) {
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
