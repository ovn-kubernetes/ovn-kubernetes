// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

// These variables define subnets used for allocations in e2e tests. It aims to
// be a central registry to track the ranges used for different purposes and
// facilitate tracking for collision avoidance with other commonly used ranges
// in test environments. Allocators defined in this package source from these
// ranges to allocate. If need be, exclusions within these ranges can be defined
// for the allocators, see newSubnetSpec.
// TODO these would best be defined through DeploymentConfig

var (
	// avoiding:
	// - 10.88.0.0/16		(podman default network, transit subnet)
	// - 10.96.0.0/16  		(Kubernetes services)
	// - 10.128.0.0/14 		(cluster default network)
	// - 10.243.0.0/16 		(cluster default network)
	// - 10.244.0.0/16 		(cluster default network)
	// - 100.64.0.0/16 		(default join subnet)
	// - 100.65.0.0/16 		(UDN primary join subnet)
	// - 172.18.0.0/16 		(KIND primary network)
	// - 172.19.0.0/16 		(XGW network)
	// - 172.22.0.0/16 		(MetalLB client network)
	// - 172.26.0.0/16 		(default BGP server network)
	// - 172.30.0.0/16 		(Kubernetes services)
	// - 169.254.169.0/29 	(masquerade subnet)
	// - fd01::/48    		(cluster default network)
	// - fd02::/112	   		(Kubernetes services)
	// - fd98::/64     		(default join subnet)
	// - fd69::/125			(masquerade subnet)
	// - fd97::/64		    (transit subnet)

	// UDN subnets, 1024 available
	udnSubnets  = "10.0.0.0/10/20"
	udnSubnets6 = "fd10::/42/52"

	// BGP peering, 1024 available each
	bgpPeerSubnets  = "172.25.0.0/19/29"
	bgpPeerSubnets6 = "fd25::/102/112"

	// EVPN IP-VRF subnets, 1024 available
	ipvrfSubnets  = "172.27.0.0/19/29"
	ipvrfSubnets6 = "fd27::/102/112"

	// EVPN VTEP subnets, 1024 available
	vtepSubnets  = "100.66.0.0/14/24"
	vtepSubnets6 = "fd66:4200::/102/112"
)
