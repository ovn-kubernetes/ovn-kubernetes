// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clusternetworkconnect

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cncv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var InvalidScenarios = []testscenario.ValidateCRScenario{

	// =============================================
	// NetworkSelectors Field Validation
	// =============================================

	// Required field validation
	{
		Description: "missing networkSelectors field",
		ExpectedErr: "spec.networkSelectors: Required value",
		Object: testscenario.WithoutFields(newClusterNetworkConnect("missing-network-selectors", cncv1.ClusterNetworkConnectSpec{
			ConnectSubnets: []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:   []cncv1.ConnectivityType{"PodNetwork"},
		}), "spec.networkSelectors"),
	},
	{
		Description: "empty networkSelectors array",
		ExpectedErr: "spec.networkSelectors: Invalid value: 0: spec.networkSelectors in body should have at least 1 items",
		Object: newClusterNetworkConnect("empty-network-selectors", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// Schema validation (array limits)
	{
		Description: "too many networkSelectors",
		ExpectedErr: "spec.networkSelectors: Too many: 6: must have at most 5 items",
		Object: newClusterNetworkConnect("too-many-network-selectors", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{
				cudnSelector("test1"),
				primaryUDNSelector("test2"),
				cudnSelector("test3"),
				primaryUDNSelector("test4"),
				cudnSelector("test5"),
				primaryUDNSelector("test6"),
			},
			ConnectSubnets: []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:   []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// Schema validation (enum values)
	{
		Description: "invalid networkSelector - non-CUDN/PUDN type",
		ExpectedErr: "Unsupported value: \"InvalidType\"",
		Object: newClusterNetworkConnect("invalid-network-selector", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{{NetworkSelectionType: "InvalidType"}},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "invalid networkSelector - mixed valid and invalid types",
		ExpectedErr: "Unsupported value: \"InvalidType\"",
		Object: newClusterNetworkConnect("invalid-mixed-selectors", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test1"), {NetworkSelectionType: "InvalidType"}},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// CEL validation (only CUDN/PUDN allowed)
	{
		Description: "invalid networkSelector - SecondaryUserDefinedNetworks type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Object: newClusterNetworkConnect("invalid-secondary-udn-selector", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{
				{
					NetworkSelectionType: "SecondaryUserDefinedNetworks",
					SecondaryUserDefinedNetworkSelector: &crdtypes.SecondaryUserDefinedNetworkSelector{
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "test",
							},
						},
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "secondary-net",
							},
						},
					},
				},
			},
			ConnectSubnets: []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:   []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "invalid networkSelector - NetworkAttachmentDefinitions type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Object: newClusterNetworkConnect("invalid-nad-selector", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{
				{
					NetworkSelectionType: "NetworkAttachmentDefinitions",
					NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "test",
							},
						},
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "my-nad",
							},
						},
					},
				},
			},
			ConnectSubnets: []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:   []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "invalid networkSelector - DefaultNetwork type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Object: newClusterNetworkConnect("invalid-default-network-selector", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{{NetworkSelectionType: "DefaultNetwork"}},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// =============================================
	// connectivity Field Validation
	// =============================================

	// Required field validation
	{
		Description: "missing connectivity field",
		ExpectedErr: "spec.connectivity: Required value",
		Object: testscenario.WithoutFields(newClusterNetworkConnect("missing-connectivity-enabled", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
		}), "spec.connectivity"),
	},
	{
		Description: "empty connectivity array",
		ExpectedErr: "spec.connectivity: Invalid value: 0: spec.connectivity in body should have at least 1 items",
		Object: newClusterNetworkConnect("empty-connectivity-enabled", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{},
		}),
	},

	// Schema validation (array limits)
	{
		Description: "too many connectivity values",
		ExpectedErr: "spec.connectivity: Too many: 3: must have at most 2 items",
		Object: newClusterNetworkConnect("too-many-connectivity-enabled", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork", "ServiceNetwork", "PodNetwork"},
		}),
	},

	// Schema validation (enum values)
	{
		Description: "invalid connectivity value",
		ExpectedErr: "spec.connectivity[0]: Unsupported value: \"InvalidConnectivity\"",
		Object: newClusterNetworkConnect("invalid-connectivity-value", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"InvalidConnectivity"},
		}),
	},

	// CEL validation (no duplicates)
	{
		Description: "duplicate connectivity types",
		ExpectedErr: "connectivity cannot contain duplicate values",
		Object: newClusterNetworkConnect("duplicate-connectivity", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork", "PodNetwork"},
		}),
	},

	// =============================================
	// ConnectSubnets Field Validation
	// =============================================

	// Schema validation (networkPrefix limits)
	{
		Description: "networkPrefix below minimum (0)",
		ExpectedErr: "spec.connectSubnets[0].networkPrefix: Invalid value: 0: spec.connectSubnets[0].networkPrefix in body should be greater than or equal to 1",
		Object: newClusterNetworkConnect("networkprefix-below-minimum", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16"}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "networkPrefix above maximum (128)",
		ExpectedErr: "spec.connectSubnets[0].networkPrefix: Invalid value: 128: spec.connectSubnets[0].networkPrefix in body should be less than or equal to 127",
		Object: newClusterNetworkConnect("networkprefix-above-maximum", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 128}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// Schema validation (array limits)
	{
		Description: "too many connectSubnets CIDRs",
		ExpectedErr: "spec.connectSubnets: Too many: 3: must have at most 2 items",
		Object: newClusterNetworkConnect("too-many-connect-subnets", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets: []cncv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
				{CIDR: "fd01::/64", NetworkPrefix: 96},
				{CIDR: "10.0.0.0/16", NetworkPrefix: 24},
			},
			Connectivity: []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// CEL validation (CIDR format)
	{
		Description: "invalid CIDR - host address instead of network",
		ExpectedErr: "CIDR must be a valid network address",
		Object: newClusterNetworkConnect("invalid-cidr-host", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.1/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "invalid CIDR format",
		ExpectedErr: "CIDR must be a valid network address",
		Object: newClusterNetworkConnect("invalid-cidr-format", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "not-a-cidr", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// CEL validation (dual-stack IP families)
	{
		Description: "same IP family CIDRs",
		ExpectedErr: "When 2 CIDRs are set, they must be from different IP families",
		Object: newClusterNetworkConnect("same-family-cidrs", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}, {CIDR: "10.0.0.0/16", NetworkPrefix: 24}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},
	{
		Description: "same IPv6 family CIDRs",
		ExpectedErr: "When 2 CIDRs are set, they must be from different IP families",
		Object: newClusterNetworkConnect("same-ipv6-family-cidrs", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "fd01::/64", NetworkPrefix: 96}, {CIDR: "fd02::/64", NetworkPrefix: 96}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
	},

	// CEL validation (dual-stack networkPrefix host bits must match)
	{
		Description: "dual-stack with mismatched networkPrefix host bits - IPv4 /24 vs IPv6 /64",
		ExpectedErr: "networkPrefix must have matching host bits",
		Object: newClusterNetworkConnect("mismatched-host-bits-v4-24-v6-64", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/16", NetworkPrefix: 24}, {CIDR: "fd01::/48", NetworkPrefix: 64}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
		// IPv4: 32-24=8 host bits, IPv6: 128-64=64 host bits - MISMATCH!
	},
	{
		Description: "dual-stack with mismatched networkPrefix host bits - IPv4 /28 vs IPv6 /123",
		ExpectedErr: "networkPrefix must have matching host bits",
		Object: newClusterNetworkConnect("mismatched-host-bits-v4-28-v6-123", cncv1.ClusterNetworkConnectSpec{
			NetworkSelectors: crdtypes.NetworkSelectors{cudnSelector("test")},
			ConnectSubnets:   []cncv1.ConnectSubnet{{CIDR: "192.168.0.0/24", NetworkPrefix: 28}, {CIDR: "fd01::/120", NetworkPrefix: 123}},
			Connectivity:     []cncv1.ConnectivityType{"PodNetwork"},
		}),
		// IPv4: 32-28=4 host bits, IPv6: 128-123=5 host bits - MISMATCH!
	},
}
