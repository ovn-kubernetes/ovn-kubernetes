package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestParseSubnets(t *testing.T) {
	tests := []struct {
		desc             string
		topology         string
		subnets          string
		excludes         string
		expectedSubnets  []config.CIDRNetworkEntry
		expectedExcludes []*net.IPNet
		expectError      bool
	}{
		{
			desc:     "multiple subnets layer 3 topology",
			topology: types.Layer3Topology,
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
			topology: types.Layer3Topology,
		},
		{
			desc:     "multiple subnets and excludes layer 2 topology",
			topology: types.Layer2Topology,
			subnets:  "192.168.1.1/26, fda6::/48",
			excludes: "192.168.1.38/32, fda6::38/128",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
			expectedExcludes: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc:     "empty subnets layer 2 topology",
			topology: types.Layer2Topology,
		},
		{
			desc:        "invalid formatted excludes layer 2 topology",
			topology:    types.Layer2Topology,
			subnets:     "192.168.1.1/26",
			excludes:    "192.168.1.1/26/32",
			expectError: true,
		},
		{
			desc:        "invalid not contained excludes layer 2 topology",
			topology:    types.Layer2Topology,
			subnets:     "fda6::/48",
			excludes:    "fda7::38/128",
			expectError: true,
		},
		{
			desc:     "multiple subnets and excludes localnet topology",
			topology: types.LocalnetTopology,
			subnets:  "192.168.1.1/26, fda6::/48",
			excludes: "192.168.1.38/32, fda6::38/128",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
			expectedExcludes: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc:     "empty subnets localnet topology",
			topology: types.LocalnetTopology,
		},
		{
			desc:        "invalid formatted excludes localnet topology",
			topology:    types.LocalnetTopology,
			subnets:     "fda6::/48",
			excludes:    "fda6::1/48/128",
			expectError: true,
		},
		{
			desc:        "invalid not contained excludes localnet topology",
			topology:    types.LocalnetTopology,
			subnets:     "192.168.1.1/26",
			excludes:    "192.168.2.38/32",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			subnets, excludes, err := parseSubnets(tc.subnets, tc.excludes, tc.topology)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
			g.Expect(excludes).To(gomega.ConsistOf(tc.expectedExcludes))
		})
	}
}

func TestParseNetconf(t *testing.T) {
	type testConfig struct {
		desc                        string
		inputNetAttachDefConfigSpec string
		ipv4Mode                    bool
		ipv6Mode                    bool
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
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: net-attach-def not managed by OVN"),
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
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: json: cannot unmarshal string into Go struct field NetConf.ipam of type types.IPAM"),
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
			ipv4Mode: true,
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
			ipv4Mode: true,
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
			ipv4Mode: true,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:   "layer2",
				NADName:    "ns1/nad1",
				MTU:        1400,
				Role:       "primary",
				Subnets:    "192.168.200.0/16",
				NetConf:    cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
				JoinSubnet: "100.66.0.0/16,fd99::/64",
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
			ipv4Mode: true,
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
			ipv4Mode: true,
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
			desc: "invalid attachment definition, specifies ipv4 subnet address in ipv6 only cluster",
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
			ipv4Mode:      false,
			ipv6Mode:      true,
			expectedError: fmt.Errorf("subnet 192.168.0.0/16/24 for network tenant-red is invalid, cluster does not support ipv4"),
		},
		{
			desc: "invalid attachment definition, specifies ipv4 exception in ipv6 only cluster",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "fd01::1234/32",
			"excludeSubnets": "192.168.200.0/16",
			"role": "secondary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			ipv4Mode:      false,
			ipv6Mode:      true,
			expectedError: fmt.Errorf("the provided network subnets [fd01::/32/64] do not contain exluded subnets 192.168.0.0/16"),
		},
		{
			desc: "invalid attachment definition, specifies ipv6 subnet address in ipv4 only cluster",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "fd01::1234/32",
			"role": "secondary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			ipv4Mode:      true,
			ipv6Mode:      false,
			expectedError: fmt.Errorf("subnet fd01::/32/64 for network tenant-red is invalid, cluster does not support ipv6"),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			if test.unsupportedReason != "" {
				t.Skip(test.unsupportedReason)
			}
			g := gomega.NewWithT(t)
			config.IPv4Mode = test.ipv4Mode
			config.IPv6Mode = test.ipv6Mode

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
			g.Expect(err).To(gomega.BeNil())
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
			g.Expect(err).To(gomega.BeNil())
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
			g.Expect(err).To(gomega.BeNil())
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
		},
		{
			desc:           "Multiple attachments to the same network in the same pod are not supported",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: fmt.Sprintf("%[1]s,%[1]s", GetNADName(namespaceName, attachmentName)),
			},
			expectedError: fmt.Errorf("unexpected error: more than one of the same NAD ns1/attachment1 specified for pod ns1/test-pod"),
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
			expectedError: fmt.Errorf("unexpected primary network \"l3-network\" specified with a NetworkSelectionElement &{Name:attachment1 Namespace:ns1 IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}"),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).To(gomega.BeNil())
			if test.inputNetConf.NADName != "" {
				netInfo.AddNADs(test.inputNetConf.NADName)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   test.inputNamespace,
					Annotations: test.inputPodAnnotations,
				},
			}

			isAttachmentRequested, _, err := GetPodNADToNetworkMapping(pod, netInfo)

			if err != nil {
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			}
			g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
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
