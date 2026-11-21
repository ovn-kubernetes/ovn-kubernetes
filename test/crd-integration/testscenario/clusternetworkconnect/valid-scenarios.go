// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clusternetworkconnect

import (
	cncv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var ValidScenarios = []testscenario.ValidateCRScenario{
	{
		Description: "valid CUDN selector with single connectivity type",
		Object: newClusterNetworkConnect("valid-cudn-single", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "valid PUDN selector with both connectivity types",
		Object: newClusterNetworkConnect("valid-pudn-both", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{primaryUDNSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork", "ServiceNetwork"},
		}),
	},
	{
		Description: "valid dual-stack CIDRs with matching host bits",
		Object: newClusterNetworkConnect("valid-dual-stack", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}, {CIDR: "fd01::/112", NetworkPrefix: 120}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "valid single IPv6 CIDR",
		Object: newClusterNetworkConnect("valid-ipv6-only", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "fd01::/64", NetworkPrefix: 96}},
			Connectivity:     []cncv1.ConnectivityType{"ServiceNetwork"},
		}),
	},
	{
		Description: "valid multiple network selectors",
		Object: newClusterNetworkConnect("valid-multiple-selectors", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test1"), primaryUDNSelector("test2")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}, {CIDR: "fd01::/112", NetworkPrefix: 120}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork", "ServiceNetwork"},
		}),
	},
}
