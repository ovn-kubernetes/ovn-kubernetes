package template

import (
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetAttachDefTemplate", func() {

	// before each test, set the IPv4Mode and IPv6Mode to true
	BeforeEach(func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
	})

	DescribeTable("should fail to render NAD spec given",
		func(spec *udnv1.UserDefinedNetworkSpec, expectedError string) {
			_, err := RenderNADSpec("foo", "bar", spec)
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		},
		Entry("invalid layer2 subnets",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Subnets: udnv1.DualStackCIDRs{"abc"},
				},
			},
			"CIDR \"abc\" not properly formatted",
		),
		Entry("invalid layer3 cluster-subnet",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{{CIDR: "!", HostSubnet: 16}},
				},
			},
			"invalid CIDR address",
		),
		Entry("invalid layer3 host-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: -1},
					},
				},
			},
			"cannot use a host subnet length mask shorter than or equal to the cluster subnet mask",
		),
		Entry("layer3 host-subnet mask is smaller then cluster-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 16},
					},
				},
			},
			"cannot use a host subnet length mask shorter than or equal to the cluster subnet mask",
		),
		Entry("layer3 host-subnet mask equal to cluster-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 24},
					},
				},
			},
			"cannot use a host subnet length mask shorter than or equal to the cluster subnet mask",
		),
		Entry("invalid layer3 host-subnet; IPv4 mask is bigger then 32",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 33},
					},
				},
			},
			"invalid host subnet, IPv4 subnet must be < 32",
		),
		Entry("invalid layer2 join subnets",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"abc"},
				},
			},
			"CIDR \"abc\" not properly formatted",
		),
		Entry("invalid layer2 dual-stack join subnets, invalid IPv4 CIDR",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"fd50::0/125", "!"},
				},
			},
			"CIDR \"!\" not properly formatted",
		),
		Entry("invalid layer2 dual-stack join subnets, invalid IPv6 CIDR",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"10.10.0.0/24", "!"},
				},
			},
			"CIDR \"!\" not properly formatted",
		),
		// The validation for max number of subnets is moved to the CRD validation,
		// no need to test it here.
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv4",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.64.10.0/24"},
				},
			},
			"pod or join subnet overlaps with already configured internal subnets",
		),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv6",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"fd98::4/127"},
				},
			},
			"pod or join subnet overlaps with already configured internal subnets",
		),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, dual-stack",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.64.10.0/24", "fd98::4/127"},
				},
			},
			"pod or join subnet overlaps with already configured internal subnets",
		),
	)

	DescribeTable("should fail to render NAD manifest, given",
		func(obj client.Object, expectedError string) {
			_, err := RenderNetAttachDefManifest(obj, "test")
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		},
		Entry("UDN, invalid topology: topology layer2 & layer3 config",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Layer3: &udnv1.Layer3Config{}}},
			"topology Layer2 is specified but Layer2 config is nil",
		),
		Entry("UDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}},
			"topology Layer3 is specified but Layer3 config is nil",
		),
		Entry("UDN, invalid IPAM config: IPAM lifecycle & disabled ipam mode",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/16"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
						Mode:      udnv1.IPAMDisabled,
					},
				},
			}},
			"lifecycle Persistent is only supported when ipam.mode is Enabled",
		),
		Entry("UDN, invalid IPAM config: IPAM enabled & no subnet",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{},
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMEnabled,
					},
				},
			}},
			"subnets is required with ipam.mode is Enabled or unset",
		),
		Entry("UDN, invalid IPAM config: IPAM disabled & subnet",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/16"},
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMDisabled,
					},
				},
			}},
			"subnets must be unset when ipam.mode is Disabled",
		),
		Entry("CUDN, invalid topology: topology layer2 & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Layer3: &udnv1.Layer3Config{}}}},
			"topology Layer2 is specified but Layer2 config is nil",
		),
		Entry("CUDN, invalid topology: topology layer2 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Localnet: &udnv1.LocalnetConfig{}}}},
			"topology Layer2 is specified but Layer2 config is nil",
		),
		Entry("CUDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}}},
			"topology Layer3 is specified but Layer3 config is nil",
		),
		Entry("CUDN, invalid topology: topology layer3 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Localnet: &udnv1.LocalnetConfig{}}}},
			"topology Layer3 is specified but Layer3 config is nil",
		),
		Entry("CUDN, invalid topology: topology localnet & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer2: &udnv1.Layer2Config{}}}},
			"topology Localnet is specified but Localnet config is nil",
		),
		Entry("CUDN, invalid topology: topology localnet & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer3: &udnv1.Layer3Config{}}}},
			"topology Localnet is specified but Localnet config is nil",
		),
		Entry("CUDN, localnet: IPv4 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"192.200.0.0/30"},
				},
			}}},
			"do not contain exluded subnets 192.200.0.0/30",
		),
		Entry("CUDN, localnet: IPv6 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"2001:aaa::/127"},
				},
			}}},
			"do not contain exluded subnets 2001:aaa::/127",
		),
	)

	It("should return no error given no UDN", func() {
		_, err := RenderNetAttachDefManifest(nil, "")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should fail given no target namespace", func() {
		cudn := &udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{}},
		}
		_, err := RenderNetAttachDefManifest(cudn, "")
		Expect(err).To(HaveOccurred())
	})

	It("should fail given unknown type", func() {
		_, err := RenderNetAttachDefManifest(&netv1.NetworkAttachmentDefinition{}, "foo")
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("should create UDN NAD from spec",
		func(testSpec udnv1.UserDefinedNetworkSpec, expectedNadNetConf string) {
			testUdn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Namespace: "mynamespace", Name: "test-net", UID: "1",
					Labels: map[string]string{"testLabel": "test"}},
				Spec: testSpec,
			}
			testNs := "mynamespace"
			ownerRef := metav1.OwnerReference{
				APIVersion:         "k8s.ovn.org/v1",
				Kind:               "UserDefinedNetwork",
				Name:               "test-net",
				UID:                "1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
			}
			expectedNAD := &netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-net",
					Labels:          map[string]string{"k8s.ovn.org/user-defined-network": "", "testLabel": "test"},
					OwnerReferences: []metav1.OwnerReference{ownerRef},
					Finalizers:      []string{"k8s.ovn.org/user-defined-network-protection"},
				},
				Spec: netv1.NetworkAttachmentDefinitionSpec{Config: expectedNadNetConf},
			}

			// must be defined so the primary user defined network can match the ip families of the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnablePersistentIPs = true
			nad, err := RenderNetAttachDefManifest(testUdn, testNs)
			Expect(err).NotTo(HaveOccurred())
			Expect(nad.TypeMeta).To(Equal(expectedNAD.TypeMeta))
			Expect(nad.ObjectMeta).To(Equal(expectedNAD.ObjectMeta))
			Expect(nad.Spec.Config).To(MatchJSON(expectedNAD.Spec.Config))
		},
		Entry("primary network, layer3",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
						{CIDR: "2001:dbb::/60"},
					},
					MTU: 1500,
				},
			},
			`{
				"cniVersion": "1.0.0",
				"type": "ovn-k8s-cni-overlay",
				"name": "mynamespace_test-net",
				"netAttachDefName": "mynamespace/test-net",
				"role": "primary",
				"topology": "layer3",
				"joinSubnet": "100.65.0.0/16,fd99::/64",
				"subnets": "192.168.100.0/16,2001:dbb::/60",
				"mtu": 1500
			}`,
		),
		Entry("primary network, layer2",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
        	}`,
		),
		Entry("primary network, should override join-subnets when specified",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.62.0.0/24", "fd92::/64"},
					MTU:         1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.62.0.0/24,fd92::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, no join-subnets should be set",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
	)

	DescribeTable("should create CUDN NAD from spec",
		func(testSpec udnv1.NetworkSpec, expectedNadNetConf string) {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", UID: "1"},
				Spec:       udnv1.ClusterUserDefinedNetworkSpec{Network: testSpec},
			}
			testNs := "mynamespace"

			expectedOwnerRef := metav1.OwnerReference{
				APIVersion:         "k8s.ovn.org/v1",
				Kind:               "ClusterUserDefinedNetwork",
				Name:               "test-net",
				UID:                "1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
			}
			expectedNAD := &netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-net",
					OwnerReferences: []metav1.OwnerReference{expectedOwnerRef},
					Labels:          map[string]string{"k8s.ovn.org/user-defined-network": ""},
					Finalizers:      []string{"k8s.ovn.org/user-defined-network-protection"},
				},
				Spec: netv1.NetworkAttachmentDefinitionSpec{Config: expectedNadNetConf},
			}
			// must be defined so the primary user defined network can match the ip families of the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnablePersistentIPs = true
			nad, err := RenderNetAttachDefManifest(cudn, testNs)
			Expect(err).NotTo(HaveOccurred())
			Expect(nad.TypeMeta).To(Equal(expectedNAD.TypeMeta))
			Expect(nad.ObjectMeta).To(Equal(expectedNAD.ObjectMeta))
			Expect(nad.Spec.Config).To(MatchJSON(expectedNAD.Spec.Config))
		},
		Entry("primary network, layer3",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
						{CIDR: "2001:dbb::/60"},
					},
					MTU: 1500,
				},
			},
			`{
				"cniVersion": "1.0.0",
				"type": "ovn-k8s-cni-overlay",
				"name": "cluster_udn_test-net",
				"netAttachDefName": "mynamespace/test-net",
				"role": "primary",
				"topology": "layer3",
				"joinSubnet": "100.65.0.0/16,fd99::/64",
				"subnets": "192.168.100.0/16,2001:dbb::/60",
				"mtu": 1500
			}`,
		),
		Entry("primary network, layer2",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
        	}`,
		),
		Entry("primary network, should override join-subnets when specified",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.62.0.0/24", "fd92::/64"},
					MTU:         1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.62.0.0/24,fd92::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, layer2",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, localnet",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					MTU:                 1600,
					VLAN:                &udnv1.VLANConfig{Mode: udnv1.VLANModeAccess, Access: &udnv1.AccessVLANConfig{ID: 200}},
					Subnets:             udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					ExcludeSubnets:      []udnv1.CIDR{"192.168.100.1/32", "2001:dbb::0/128"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
              "excludeSubnets": "192.168.100.1/32,2001:dbb::0/128",
			  "mtu": 1600,
              "vlanID": 200, 
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, localnet, when MTU is unset it should set default MTU",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					VLAN:                &udnv1.VLANConfig{Mode: udnv1.VLANModeAccess, Access: &udnv1.AccessVLANConfig{ID: 200}},
					Subnets:             udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					ExcludeSubnets:      []udnv1.CIDR{"192.168.100.1/32", "2001:dbb::0/128"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.0.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
              "excludeSubnets": "192.168.100.1/32,2001:dbb::0/128",
			  "mtu": 1500,
              "vlanID": 200, 
			  "allowPersistentIPs": true
			}`,
		),
	)
})
