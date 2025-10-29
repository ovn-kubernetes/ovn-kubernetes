package util

import (
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestParseNetworkSubnets(t *testing.T) {
	tests := []struct {
		desc            string
		topology        string
		subnets         string
		expectedSubnets []config.CIDRNetworkEntry
		expectError     bool
	}{
		{
			desc:     "multiple subnets layer 3 topology",
			topology: ovntypes.Layer3Topology,
			subnets:  "192.168.1.1/26/28, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("192.168.1.0/26"),
					HostSubnetLength: 28,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fda6::/48"),
					HostSubnetLength: 64,
				},
			},
		},
		{
			desc:     "empty subnets layer 3 topology",
			topology: ovntypes.Layer3Topology,
		},
		{
			desc:     "multiple subnets layer 2 topology",
			topology: ovntypes.Layer2Topology,
			subnets:  "192.168.1.1/26, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
		},
		{
			desc:     "empty subnets layer 2 topology",
			topology: ovntypes.Layer2Topology,
			subnets:  "",
		},
		{
			desc:     "multiple subnets localnet topology",
			topology: ovntypes.LocalnetTopology,
			subnets:  "192.168.1.1/26, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
		},
		{
			desc:     "empty subnets localnet topology",
			topology: ovntypes.LocalnetTopology,
			subnets:  "",
		},
		{
			desc:        "unsupported topology",
			topology:    "unsupported",
			subnets:     "192.168.1.1/26",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			subnets, err := parseNetworkSubnets(tc.subnets, tc.topology)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
		})
	}
}

func TestParseSubnetList(t *testing.T) {
	tests := []struct {
		desc            string
		subnets         string
		expectedSubnets []*net.IPNet
		expectError     bool
	}{
		{
			desc:            "multiple subnets",
			subnets:         "192.168.1.38/32, fda6::38/128",
			expectedSubnets: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc: "empty subnets",
		},
		{
			desc:        "invalid formatted subnets",
			subnets:     "192.168.1.1/26/32",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			subnets, err := parseSubnetList(tc.subnets)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
		})
	}
}

func TestValidateSubnetContainment(t *testing.T) {
	tests := []struct {
		desc             string
		subnets          []*net.IPNet
		containerSubnets []config.CIDRNetworkEntry
		expectError      bool
	}{
		{
			desc:    "valid containment",
			subnets: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
			containerSubnets: []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("192.168.1.0/26")},
				{CIDR: ovntest.MustParseIPNet("fda6::/48")},
			},
		},
		{
			desc:    "invalid containment",
			subnets: ovntest.MustParseIPNets("fda7::38/128"),
			containerSubnets: []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("fda6::/48")},
			},
			expectError: true,
		},
		{
			desc:             "empty subnets",
			containerSubnets: []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("192.168.1.0/26")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := validateSubnetContainment(tc.subnets, tc.containerSubnets, config.NewExcludedSubnetNotContainedError)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err).To(gomega.BeAssignableToTypeOf(&config.ValidationError{}))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestParseNetconf(t *testing.T) {
	type testConfig struct {
		desc                        string
		inputNetAttachDefConfigSpec string
		expectedNetConf             *ovncnitypes.NetConf
		expectedError               error
		unsupportedReason           string
	}

	tests := []testConfig{
		{
			desc:          "empty network attachment configuration",
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: unexpected end of JSON input"),
		},
		{
			desc: "net-attach-def-name attribute does not match the metadata",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("net-attach-def name (ns1/nad1) is inconsistent with config (default/tenantred)"),
		},
		{
			desc: "attachment definition with no `name` attribute",
			inputNetAttachDefConfigSpec: `
    {
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: invalid name in in secondary network netconf ()"),
		},
		{
			desc: "attachment definition for another plugin",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "some other thing",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("net-attach-def not managed by OVN"),
		},
		{
			desc: "attachment definition with IPAM key defined, using a wrong type",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred",
            "ipam": "this is wrong"
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: json: cannot unmarshal string into Go struct field NetConf.NetConf.ipam of type types.IPAM"),
		},
		{
			desc: "attachment definition with IPAM key defined",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "ns1/nad1",
            "ipam": {"type": "ninjaturtle"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: IPAM key is not supported. Use OVN-K provided IPAM via the `subnets` attribute"),
		},
		{
			desc: "attachment definition missing the NAD name attribute",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: missing NADName in secondary network netconf tenantred"),
		},
		{
			desc: "valid attachment definition for a localnet topology with a VLAN",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "localnet",
				NADName:  "ns1/nad1",
				MTU:      1400,
				VLANID:   10,
				NetConf:  cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for the default network",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				NADName: "ns1/nad1",
				MTU:     1400,
				NetConf: cnitypes.NetConf{Name: "default", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for a localnet topology with a VLAN using a plugins list",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "cniVersion": "1.0.0",
            "plugins": [
              {
                "type": "ovn-k8s-cni-overlay",
                "topology": "localnet",
                "vlanID": 10,
                "netAttachDefName": "ns1/nad1"
              }
            ]
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "localnet",
				NADName:  "ns1/nad1",
				MTU:      1400,
				VLANID:   10,
				NetConf:  cnitypes.NetConf{Name: "tenantred", CNIVersion: "1.0.0", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for a localnet topology with persistent IPs and a subnet",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:           "localnet",
				NADName:            "ns1/nad1",
				MTU:                1400,
				NetConf:            cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
				AllowPersistentIPs: true,
				Subnets:            "192.168.200.0/16",
			},
		},
		{
			desc: "valid attachment definition for a layer2 topology with persistent IPs and a subnet",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:           "layer2",
				NADName:            "ns1/nad1",
				MTU:                1400,
				AllowPersistentIPs: true,
				Subnets:            "192.168.200.0/16",
				NetConf:            cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "invalid attachment definition for a layer3 topology with persistent IPs",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("layer3 topology does not allow persistent IPs"),
		},
		{
			desc: "valid attachment definition for a layer2 topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
			"subnets": "192.168.200.0/16",
			"role": "primary",
            "netAttachDefName": "ns1/nad1",
			"joinSubnet": "100.66.0.0/16,fd99::/64"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:      "layer2",
				NADName:       "ns1/nad1",
				MTU:           1400,
				Role:          "primary",
				Subnets:       "192.168.200.0/16",
				TransitSubnet: config.ClusterManager.V4TransitSubnet,
				NetConf:       cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
				JoinSubnet:    "100.66.0.0/16,fd99::/64",
			},
		},
		{
			desc: "valid attachment definition for a layer3 topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "primary",
			"netAttachDefName": "ns1/nad1",
			"joinSubnet": "100.66.0.0/16"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:   "layer3",
				NADName:    "ns1/nad1",
				MTU:        1400,
				Role:       "primary",
				Subnets:    "192.168.200.0/16",
				NetConf:    cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
				JoinSubnet: "100.66.0.0/16",
			},
		},
		{
			desc: "valid attachment definition for a layer3 topology with role:secondary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "secondary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "layer3",
				NADName:  "ns1/nad1",
				MTU:      1400,
				Role:     "secondary",
				Subnets:  "192.168.200.0/16",
				NetConf:  cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "invalid attachment definition for a layer3 topology with role:Primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "Primary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("invalid network role value Primary"),
		},
		{
			desc: "invalid attachment definition for a localnet topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"role": "primary",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("unexpected network field \"role\" primary for \"localnet\" topology, " +
				"localnet topology does not allow network roles to be set since its always a secondary network"),
		},
		{
			desc: "invalid attachment definition for a localnet topology with joinsubnet provided",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"joinSubnet": "100.66.0.0/16",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("localnet topology does not allow specifying join-subnet as services are not supported"),
		},
		{
			desc: "A layer2 primary UDN requires a subnet",
			inputNetAttachDefConfigSpec: `
{
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "netAttachDefName": "ns1/nad1",
            "role": "primary"
}`,
			expectedError: fmt.Errorf("the subnet attribute must be defined for layer2 primary user defined networks"),
		},
		{
			desc: "invalid not contained excludes layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "fda6::/48",
            "excludeSubnets": "fda7::38/128",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("invalid subnet configuration: error while parsing subnets: the provided network subnets do not contain excluded subnets fda7::38/128"),
		},
		{
			desc: "multiple subnets and excludes localnet topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "subnets": "192.168.1.1/26, fda6::/48",
            "excludeSubnets": "192.168.1.38/32, fda6::38/128",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:       "localnet",
				NADName:        "ns1/nad1",
				MTU:            1400,
				Subnets:        "192.168.1.1/26, fda6::/48",
				ExcludeSubnets: "192.168.1.38/32, fda6::38/128",
				NetConf:        cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "multiple subnets and reserved subnets layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24, fda6::/64",
            "reservedSubnets": "192.168.1.0/28, fda6::0/80",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24, fda6::/64",
				ReservedSubnets: "192.168.1.0/28, fda6::0/80",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "layer 2 with both excludes and reserved subnets",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24",
            "excludeSubnets": "192.168.1.200/29",
            "reservedSubnets": "192.168.1.0/28",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24",
				ExcludeSubnets:  "192.168.1.200/29",
				ReservedSubnets: "192.168.1.0/28",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "dual-stack reserved subnets layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24, 2001:db8::/64",
            "reservedSubnets": "192.168.1.0/28, 2001:db8::/80",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24, 2001:db8::/64",
				ReservedSubnets: "192.168.1.0/28, 2001:db8::/80",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			config.IPv4Mode = true
			config.IPv6Mode = true
			// Enable feature flags for reserved subnets tests
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true
			if test.unsupportedReason != "" {
				t.Skip(test.unsupportedReason)
			}
			g := gomega.NewWithT(t)
			networkAttachmentDefinition := applyNADDefaults(
				&nadv1.NetworkAttachmentDefinition{
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: test.inputNetAttachDefConfigSpec,
					},
				})
			if test.expectedError != nil {
				_, err := ParseNetConf(networkAttachmentDefinition)
				g.Expect(err).To(gomega.MatchError(test.expectedError.Error()))
			} else {
				g.Expect(ParseNetConf(networkAttachmentDefinition)).To(gomega.Equal(test.expectedNetConf))
			}
		})
	}
}

func TestJoinSubnets(t *testing.T) {
	type testConfig struct {
		desc            string
		inputNetConf    *ovncnitypes.NetConf
		expectedSubnets []*net.IPNet
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(config.Gateway.V4JoinSubnet),
				ovntest.MustParseIPNet(config.Gateway.V6JoinSubnet),
			},
		},
		{
			desc: "secondaryL3NetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6),
			},
		},
		{
			desc: "secondaryL2NetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.Layer2Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6),
			},
		},
		{
			desc: "secondaryLocalNetInfo with nil join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			expectedSubnets: nil,
		},
		{
			desc: "secondaryL2NetInfo with user configured v4 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer2Topology,
				JoinSubnet: "100.68.0.0/16",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("100.68.0.0/16"),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL3NetInfo with user configured v6 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer3Topology,
				JoinSubnet: "2001:db8:abcd:1234::/64",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet("2001:db8:abcd:1234::/64"), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL3NetInfo with user configured v4&&v6 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer3Topology,
				JoinSubnet: "100.68.0.0/16,2001:db8:abcd:1234::/64",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("100.68.0.0/16"),
				ovntest.MustParseIPNet("2001:db8:abcd:1234::/64"), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL2NetInfo with user configured empty join subnet value takes default value",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer2Topology,
				JoinSubnet: "",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6), // given user only provided v4, we set v6 to default value
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.JoinSubnets()).To(gomega.Equal(test.expectedSubnets))
			if netInfo.TopologyType() != ovntypes.LocalnetTopology {
				g.Expect(netInfo.JoinSubnetV4()).To(gomega.Equal(test.expectedSubnets[0]))
				g.Expect(netInfo.JoinSubnetV6()).To(gomega.Equal(test.expectedSubnets[1]))
			}
		})
	}
}

func TestIsPrimaryNetwork(t *testing.T) {
	type testConfig struct {
		desc            string
		inputNetConf    *ovncnitypes.NetConf
		expectedPrimary bool
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "defaultNetInfo with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: false,
		},
		{
			desc: "defaultNetInfo with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL3 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL3 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: true,
		},
		{
			desc: "secondaryNetInfoL3 with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL2 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL2 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: true,
		},
		{
			desc: "secondaryNetInfoL2 with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.IsPrimaryNetwork()).To(gomega.Equal(test.expectedPrimary))
		})
	}
}

func TestIsDefault(t *testing.T) {
	type testConfig struct {
		desc               string
		inputNetConf       *ovncnitypes.NetConf
		expectedDefaultVal bool
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedDefaultVal: true,
		},
		{
			desc: "secondaryNetInfoL3 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedDefaultVal: false,
		},
		{
			desc: "secondaryNetInfoL2 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedDefaultVal: false,
		},
		{
			desc: "secondaryNetInfoLocalNet with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			expectedDefaultVal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.IsDefault()).To(gomega.Equal(test.expectedDefaultVal))
		})
	}
}

func TestGetPodNADToNetworkMapping(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                          string
		inputNamespace                string
		inputNetConf                  *ovncnitypes.NetConf
		inputPodAnnotations           map[string]string
		expectedError                 error
		expectedIsAttachmentRequested bool
		expectedNetworkMapping        map[string]struct{}
	}

	tests := []testConfig{
		{
			desc:                "Looking for a network *not* present in the pod's attachment requests",
			inputNamespace:      namespaceName,
			inputPodAnnotations: map[string]string{nadv1.NetworkAttachmentAnnot: "[]"},
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: false,
			expectedNetworkMapping:        map[string]struct{}{},
		},
		{
			desc: "Looking for a network present in the pod's attachment requests",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkMapping:        map[string]struct{}{GetNADName(namespaceName, attachmentName): {}},
		},
		{
			desc:           "Multiple attachments to the same network in the same pod",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: fmt.Sprintf("%[1]s,%[1]s", GetNADName(namespaceName, attachmentName)),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkMapping: map[string]struct{}{
				GetNADName(namespaceName, attachmentName):                      {},
				GetIndexedNADKey(GetNADName(namespaceName, attachmentName), 1): {},
			},
		},
		{
			desc:           "Attaching to a secondary network to a user defined primary network is not supported",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedError:          fmt.Errorf("unexpected primary network \"l3-network\" specified with a NetworkSelectionElement &{Name:attachment1 Namespace:ns1 IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}"),
			expectedNetworkMapping: map[string]struct{}{},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   test.inputNamespace,
					Annotations: test.inputPodAnnotations,
				},
			}

			isAttachmentRequested, networkMap, err := GetPodNADToNetworkMapping(pod, netInfo)
			if test.expectedError != nil {
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
			actualNetworkMapping := map[string]struct{}{}
			for nadName := range networkMap {
				actualNetworkMapping[nadName] = struct{}{}
			}
			g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
			g.Expect(actualNetworkMapping).To(gomega.Equal(test.expectedNetworkMapping))
		})
	}
}

func TestGetPodNADToNetworkMappingWithActiveNetwork(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                             string
		inputNamespace                   string
		inputNetConf                     *ovncnitypes.NetConf
		inputPrimaryUDNConfig            *ovncnitypes.NetConf
		inputPodAnnotations              map[string]string
		expectedError                    error
		expectedIsAttachmentRequested    bool
		expectedNetworkSelectionElements map[string]*nadv1.NetworkSelectionElement
		enablePreconfiguredUDNAddresses  bool
		injectPrimaryUDNNADs             []string
	}

	tests := []testConfig{
		{
			desc: "there isn't a primary UDN",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the netinfo is different from the active network",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "another-network"},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, "another-network"),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: false,
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs but the pod does not request it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Using deprecated UDN IPAMClaim annotation",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Pod has both defaultNSE with ipam-claim reference and UDNIPAMClaim annotations, specifying the same IPAMClaim CR",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:          `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Pod has both defaultNSE with ipam-claim reference and UDNIPAMClaim annotations, specifying different IPAMClaim CR." +
				"DefaultNSE's ipam-claim reference should take precedence",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:          `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-right-of-the-horse",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a secondary layer2 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer3 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer3Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer3Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP, MAC and IPAMClaimRef on default network annotation for it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff", "ipam-claim-reference": "my-ipam-claim"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPRequest:          []string{"192.168.0.3/24", "fda6::3/48"},
					MacRequest:         "aa:bb:cc:dd:ee:ff",
					IPAMClaimReference: "my-ipam-claim",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected namespace",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "other-namespace", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE namespace "other-namespace", expected "ovn-kubernetes"`),
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected name",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "unexpected-name", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE name "unexpected-name", expected "default"`),
		},
		{
			desc:           "should fail when no nad of the active network found on the pod namespace",
			inputNamespace: "non-existent-ns",
			expectedError:  fmt.Errorf(`no active NAD found for namespace "non-existent-ns"`),
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				NADName:  GetNADName(namespaceName, attachmentName),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
		},
		{
			desc: "primary l2 CUDN (replicated NADs), should return the correct active network according to pod namespace",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "cluster_udn_l2p"},
				NADName:  GetNADName("red", "l2p"),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "cluster_udn_l2p"},
				NADName:  GetNADName("red", "l2p"),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			injectPrimaryUDNNADs: []string{"blue/l2p", "green/l2p"},
			inputNamespace:       "blue",
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedIsAttachmentRequested:   true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"blue/l2p": {
					Name:       "l2p",
					Namespace:  "blue",
					IPRequest:  []string{"192.168.0.3/24", "fda6::3/48"},
					MacRequest: "aa:bb:cc:dd:ee:ff",
				},
			},
		},

		{
			desc: "default-network ips and mac is is ignored for Layer3 topology",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:       "attachment1",
					Namespace:  "ns1",
					IPRequest:  nil,
					MacRequest: "",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "default-network with bad format",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"foo}`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`failed getting default-network annotation for pod "ns1/test-pod": %w`, fmt.Errorf(`GetK8sPodDefaultNetwork: failed to parse CRD object: parsePodNetworkAnnotation: failed to parse pod Network Attachment Selection Annotation JSON format: unexpected end of JSON input`)),
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
			})

			// Set custom network config based on test requirements
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = test.enablePreconfiguredUDNAddresses
			if test.enablePreconfiguredUDNAddresses {
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			}

			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			var primaryUDNNetInfo NetInfo
			if test.inputPrimaryUDNConfig != nil {
				primaryUDNNetInfo, err = NewNetInfo(test.inputPrimaryUDNConfig)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if test.inputPrimaryUDNConfig.NADName != "" {
					mutableNetInfo := NewMutableNetInfo(primaryUDNNetInfo)
					mutableNetInfo.AddNADs(test.inputPrimaryUDNConfig.NADName)
					if len(test.injectPrimaryUDNNADs) > 0 {
						mutableNetInfo.AddNADs(test.injectPrimaryUDNNADs...)
					}
					primaryUDNNetInfo = mutableNetInfo
				}
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   namespaceName,
					Annotations: test.inputPodAnnotations,
				},
			}
			if test.inputNamespace != "" {
				pod.Namespace = test.inputNamespace
			}

			isAttachmentRequested, networkSelectionElements, err := GetPodNADToNetworkMappingWithActiveNetwork(
				pod,
				netInfo,
				primaryUDNNetInfo,
			)

			if test.expectedError != nil {
				g.Expect(err).To(gomega.HaveOccurred(), "unexpected success operation, epecting error")
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			} else {
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
				g.Expect(networkSelectionElements).To(gomega.Equal(test.expectedNetworkSelectionElements))
			}
		})
	}
}

func TestSubnetOverlapCheck(t *testing.T) {
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/14")
	_, cidr6, _ := net.ParseCIDR("fe00::/16")
	_, svcCidr4, _ := net.ParseCIDR("172.30.0.0/16")
	_, svcCidr6, _ := net.ParseCIDR("fe01::/16")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 24}, {CIDR: cidr6, HostSubnetLength: 64}}
	config.Kubernetes.ServiceCIDRs = []*net.IPNet{svcCidr4, svcCidr6}
	config.Gateway.V4MasqueradeSubnet = "169.254.169.0/29"
	config.Gateway.V6MasqueradeSubnet = "fd69::/125"
	config.Gateway.V4JoinSubnet = "100.64.0.0/16"
	config.Gateway.V6JoinSubnet = "fd98::/64"
	config.ClusterManager.V4TransitSubnet = "100.88.0.0/16"
	config.ClusterManager.V6TransitSubnet = "fd97::/64"
	type testConfig struct {
		desc                        string
		inputNetAttachDefConfigSpec string
		expectedError               error
	}

	tests := []testConfig{
		{
			desc: "return error when IPv4 POD subnet in net-attach-def overlaps with transit switch subnet",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer3",
                    "subnets": "100.88.0.0/17",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("100.88.0.0/17")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetTransit, Subnet: MustParseCIDR(config.ClusterManager.V4TransitSubnet)}),
		},
		{
			desc: "return error when IPv4 POD subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "10.129.0.0/16",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("10.129.0.0/16")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetCluster, Subnet: cidr4}),
		},
		{
			desc: "return error when IPv4 join subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
				{
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16",
                    "joinSubnet": "100.64.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: MustParseCIDR("100.64.0.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: MustParseCIDR(config.Gateway.V4JoinSubnet)}),
		},
		{
			desc: "return error when IPv6 POD subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16,fe01::/24",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("fe01::/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetService, Subnet: svcCidr6},
			),
		},
		{
			desc: "return error when IPv6 join subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16,fe02::/24",
                    "joinSubnet": "100.65.0.0/24,fd69::/112",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: MustParseCIDR("fd69::/112")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetMasquerade, Subnet: MustParseCIDR(config.Gateway.V6MasqueradeSubnet)},
			),
		},
		{
			desc: "excluded subnet should not be considered for overlap check",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "10.0.0.0/8",
                    "excludeSubnets": "10.128.0.0/14",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
		},
		{
			desc: "return error when the network is not ovnk",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "test",
                    "type": "sriov-cni"
                }
			`,
			expectedError: ErrorAttachDefNotOvnManaged,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			networkAttachmentDefinition := applyNADDefaults(
				&nadv1.NetworkAttachmentDefinition{
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: test.inputNetAttachDefConfigSpec,
					},
				})
			if test.expectedError != nil {
				_, err := ParseNADInfo(networkAttachmentDefinition)
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(test.expectedError.Error())))
			} else {
				_, err := ParseNADInfo(networkAttachmentDefinition)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestNewNetInfo(t *testing.T) {
	type testConfig struct {
		desc          string
		subnets       string
		ipv4Cluster   bool
		ipv6Cluster   bool
		expectedError error
	}

	tests := []testConfig{
		{
			desc:        "ipv4 primary network in ipv4 cluster",
			subnets:     "192.168.200.0/16",
			ipv4Cluster: true,
		},
		{
			desc:          "ipv4 primary network in ipv6 cluster",
			subnets:       "192.168.200.0/16",
			ipv6Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv4 subnets but the cluster does not support ipv4"),
		},
		{
			desc:        "ipv4 primary network in dualstack cluster",
			subnets:     "192.168.200.0/16",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
		{
			desc:          "ipv6 primary network in ipv4 cluster",
			subnets:       "fda6::/48",
			ipv4Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv6 subnets but the cluster does not support ipv6"),
		},
		{
			desc:        "ipv6 primary network in ipv6 cluster",
			subnets:     "fda6::/48",
			ipv6Cluster: true,
		},
		{
			desc:        "ipv6 primary network in dualstack cluster",
			subnets:     "fda6::/48",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
		{
			desc:          "dualstack primary network in ipv4 cluster",
			subnets:       "192.168.200.0/16, fda6::/48",
			ipv4Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv6 subnets but the cluster does not support ipv6"),
		},
		{
			desc:          "dualstack primary network in ipv6 cluster",
			subnets:       "192.168.200.0/16, fda6::/48",
			ipv6Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv4 subnets but the cluster does not support ipv4"),
		},
		{
			desc:        "dualstack primary network in dualstack cluster",
			subnets:     "192.168.200.0/16, fda6::/48",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			inputNetConf := &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  test.subnets,
			}
			config.IPv4Mode = test.ipv4Cluster
			config.IPv6Mode = test.ipv6Cluster
			g := gomega.NewWithT(t)
			_, err := NewNetInfo(inputNetConf)
			if test.expectedError != nil {
				g.Expect(err).To(gomega.MatchError(test.expectedError), "should return an error for invalid network configuration")
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error for valid network configuration")
			}
		})
	}
}

func TestAreNetworksCompatible(t *testing.T) {
	tests := []struct {
		desc                   string
		aNetwork               NetInfo
		anotherNetwork         NetInfo
		expectedResult         bool
		expectationDescription string
	}{
		{
			desc:                   "physical network name update",
			aNetwork:               &userDefinedNetInfo{physicalNetworkName: "A"},
			anotherNetwork:         &userDefinedNetInfo{physicalNetworkName: "B"},
			expectedResult:         false,
			expectationDescription: "we should reconcile on physical network name updates",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(AreNetworksCompatible(test.aNetwork, test.anotherNetwork)).To(
				gomega.Equal(test.expectedResult),
				test.expectationDescription,
			)
		})
	}
}

func applyNADDefaults(nad *nadv1.NetworkAttachmentDefinition) *nadv1.NetworkAttachmentDefinition {
	const (
		name      = "nad1"
		namespace = "ns1"
	)
	nad.Name = name
	nad.Namespace = namespace
	return nad
}

func TestGetNodeManagementIP(t *testing.T) {
	testCases := []struct {
		name       string
		netConf    *ovncnitypes.NetConf
		hostSubnet string
		expectedIP *net.IPNet
	}{
		{
			name: "DefaultNetInfo should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer3 UDN should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l3-network"},
				Topology:              ovntypes.Layer3Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer2 primary UDN without infrastructure subnets should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate the second usable IP from infrastructure subnet",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.4/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.5/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the broadcast IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.5",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.255/32, 10.0.0.100/32",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.100/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet without conflicting with default gateway",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.2",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet without conflicting with the default GW ip",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.2",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::/126",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::2/64"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6 - last available IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "2001:db8::ffff:ffff:ffff:fffe",
				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::ffff:ffff:ffff:fffe/127",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::ffff:ffff:ffff:ffff/64"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true

			netInfo, err := NewNetInfo(tc.netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			hostSubnet := ovntest.MustParseIPNet(tc.hostSubnet)

			result := netInfo.GetNodeManagementIP(hostSubnet)
			if result == nil {
				t.Fatalf("GetNodeManagementIP returned nil")
			}

			if !result.IP.Equal(tc.expectedIP.IP) {
				t.Errorf("Expected IP %s, got %s", tc.expectedIP.IP.String(), result.IP.String())
			}

			if result.Mask.String() != tc.expectedIP.Mask.String() {
				t.Errorf("Expected mask %s, got %s", tc.expectedIP.Mask.String(), result.Mask.String())
			}
		})
	}
}

func TestGetNodeGatewayIP(t *testing.T) {
	testCases := []struct {
		name       string
		netConf    *ovncnitypes.NetConf
		hostSubnet string
		expectedIP *net.IPNet
	}{
		{
			name: "DefaultNetInfo should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer3 UDN should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l3-network"},
				Topology:              ovntypes.Layer3Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN without infrastructure subnets should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with custom default gateway IP should return that custom IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:           cnitypes.NetConf{Name: "l2-network"},
				Topology:          ovntypes.Layer2Topology,
				Role:              ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs: "10.0.0.5",
				Subnets:           "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.5/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate the first usable IP from infrastructure subnet",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.4/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.4/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the network IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the broadcast IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.255/32, 10.0.0.9/32",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.9/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,

				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::/126",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::1/64"),
		},
		{
			name: "Localnet topology should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(tc.netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true

			hostSubnet := ovntest.MustParseIPNet(tc.hostSubnet)

			result := netInfo.GetNodeGatewayIP(hostSubnet)
			if result == nil {
				t.Fatalf("GetNodeGatewayIP returned nil")
			}

			if !result.IP.Equal(tc.expectedIP.IP) {
				t.Errorf("Expected IP %s, got %s", tc.expectedIP.IP.String(), result.IP.String())
			}

			if result.Mask.String() != tc.expectedIP.Mask.String() {
				t.Errorf("Expected mask %s, got %s", tc.expectedIP.Mask.String(), result.Mask.String())
			}
		})
	}
}
