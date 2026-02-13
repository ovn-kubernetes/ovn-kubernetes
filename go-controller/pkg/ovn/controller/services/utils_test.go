package services

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestExternalIDsForLoadBalancer(t *testing.T) {
	name := "svc-ab23"
	namespace := "ns"
	defaultNetInfo := util.DefaultNetInfo{}
	config.IPv4Mode = true
	UDNNetInfo, err := getSampleUDNNetInfo(namespace, "layer3")
	require.NoError(t, err)
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			// also handle no TypeMeta, which can happen.
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
			types.NetworkExternalID:           UDNNetInfo.GetNetworkName(),
			types.NetworkRoleExternalID:       types.NetworkRolePrimary,
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, UDNNetInfo),
	)

}

func TestIsHostEndpoint(t *testing.T) {
	// Save and restore original config
	oldClusterSubnet := config.Default.ClusterSubnets
	defer func() {
		config.Default.ClusterSubnets = oldClusterSubnet
	}()

	// Setup test cluster subnets
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: cidr4, HostSubnetLength: 26},
		{CIDR: cidr6, HostSubnetLength: 26},
	}

	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		{
			name:     "IP in cluster subnet - not host endpoint",
			ip:       "10.128.1.5",
			expected: false,
		},
		{
			name:     "IP outside cluster subnet - is host endpoint",
			ip:       "192.168.1.5",
			expected: true,
		},
		{
			name:     "IPv6 in cluster subnet - not host endpoint",
			ip:       "fe00::5",
			expected: false,
		},
		{
			name:     "IPv6 outside cluster subnet - is host endpoint",
			ip:       "fd00::5",
			expected: true,
		},
		{
			name:     "IP at lower cluster subnet boundary - not host endpoint",
			ip:       "10.128.0.0",
			expected: false,
		},
		{
			name:     "IP at upper cluster subnet boundary - not host endpoint",
			ip:       "10.128.255.255",
			expected: false,
		},
		// The following 2 cases show edge cases with invalid IP addresses and show that existing code isn't
		// prepared to handle those. This is a potential TBD / TODO.
		{
			name:     "Empty IP - is host endpoint",
			ip:       "",
			expected: true,
		},
		{
			name:     "Unparsable IP - is host endpoint",
			ip:       "garbage",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsHostEndpoint(tt.ip)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHasHostEndpoints(t *testing.T) {
	// Save and restore original config
	oldClusterSubnet := config.Default.ClusterSubnets
	defer func() {
		config.Default.ClusterSubnets = oldClusterSubnet
	}()

	// Setup test cluster subnets
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: cidr4, HostSubnetLength: 26},
		{CIDR: cidr6, HostSubnetLength: 26},
	}

	tests := []struct {
		name     string
		lbes     util.LBEndpointsList
		expected bool
	}{
		{
			name: "all endpoints in cluster subnet",
			lbes: util.LBEndpointsList{
				{V4IPs: []string{"10.128.1.5", "10.128.1.6"}},
			},
			expected: false,
		},
		{
			name: "has host endpoint in V4IPs",
			lbes: util.LBEndpointsList{
				{V4IPs: []string{"10.128.1.5", "192.168.1.10"}},
			},
			expected: true,
		},
		{
			name: "has host endpoint in V6IPs",
			lbes: util.LBEndpointsList{
				{V6IPs: []string{"fe00::1", "fd00::1"}},
			},
			expected: true,
		},
		{
			name: "all endpoints in cluster subnet - mixed v4 and v6",
			lbes: util.LBEndpointsList{
				{
					V4IPs: []string{"10.128.1.5"},
					V6IPs: []string{"fe00::1"},
				},
			},
			expected: false,
		},
		{
			name: "host endpoint in first LBEndpoint",
			lbes: util.LBEndpointsList{
				{V4IPs: []string{"192.168.1.10"}},
				{V4IPs: []string{"10.128.1.5"}},
			},
			expected: true,
		},
		{
			name: "host endpoint in second LBEndpoint",
			lbes: util.LBEndpointsList{
				{V4IPs: []string{"10.128.1.5"}},
				{V4IPs: []string{"192.168.1.10"}},
			},
			expected: true,
		},
		{
			name:     "empty endpoints",
			lbes:     util.LBEndpointsList{},
			expected: false,
		},
		{
			name: "endpoint with empty IP lists",
			lbes: util.LBEndpointsList{
				{V4IPs: []string{}, V6IPs: []string{}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasHostEndpoints(tt.lbes)
			assert.Equal(t, tt.expected, result)
		})
	}
}
