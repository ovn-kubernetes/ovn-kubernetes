package template

import (
	"strings"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

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
			config.NewCIDRNotProperlyFormattedError("abc").Error(),
		),
		Entry("invalid layer3 cluster-subnet",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{{CIDR: "!", HostSubnet: 16}},
				},
			},
			config.NewInvalidCIDRAddressError().Error(),
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
			config.NewHostSubnetMaskError(24, 24).Error(), // -1 is not a valid host subnet mask, it's converted to 24
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
			config.NewHostSubnetMaskError(16, 24).Error(),
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
			config.NewHostSubnetMaskError(24, 24).Error(),
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
			config.NewInvalidIPv4HostSubnetError().Error(),
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
			config.NewCIDRNotProperlyFormattedError("abc").Error(),
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
			config.NewCIDRNotProperlyFormattedError("!").Error(),
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
			config.NewCIDRNotProperlyFormattedError("!").Error(),
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
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("100.64.10.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("100.64.0.0/16")}).Error(),
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
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("fd98::4/127")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("fd98::/64")}).Error(),
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
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("100.64.10.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("100.64.0.0/16")}).Error(),
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
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("UDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
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
			config.NewIPAMLifecycleNotSupportedError().Error(),
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
			config.NewSubnetsRequiredError().Error(),
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
			config.NewSubnetsMustBeUnsetError().Error(),
		),
		Entry("CUDN, invalid topology: topology layer2 & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Layer3: &udnv1.Layer3Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer2 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Localnet: &udnv1.LocalnetConfig{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer3 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Localnet: &udnv1.LocalnetConfig{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
		),
		Entry("CUDN, invalid topology: topology localnet & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer2: &udnv1.Layer2Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLocalnet)).Error(),
		),
		Entry("CUDN, invalid topology: topology localnet & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer3: &udnv1.Layer3Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLocalnet)).Error(),
		),
		Entry("CUDN, localnet: IPv4 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"192.200.0.0/30"},
				},
			}}},
			config.NewExcludedSubnetNotContainedError("192.200.0.0/30").Error(),
		),
		Entry("CUDN, localnet: IPv6 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"2001:aaa::/127"},
				},
			}}},
			config.NewExcludedSubnetNotContainedError("2001:aaa::/127").Error(),
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
					Annotations: map[string]string{"testAnnotation": "test", "k8s.ovn.org/testAnnotation": "test"},
					Labels:      map[string]string{"testLabel": "test"}},
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
					Annotations:     map[string]string{"testAnnotation": "test"},
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
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
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
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
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
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
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
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
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

	It("should correctly assign transit Subnets", func() {
		// check no overlap, use default values
		netConf := &ovncnitypes.NetConf{
			Role:     strings.ToLower(types.NetworkRolePrimary),
			Topology: strings.ToLower(types.Layer2Topology),
			Subnets:  "10.12.0.0/16,fd12:dbba::/64",
		}
		err := util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.88.0.0/16,fd97::/64"))
		// check Subnet with the default Transit subnet overlap
		netConf = &ovncnitypes.NetConf{
			Role:     strings.ToLower(types.NetworkRolePrimary),
			Topology: strings.ToLower(types.Layer2Topology),
			Subnets:  "100.88.0.0/15,fd97::/63",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.90.0.0/16,fd97:0:0:2::/64"))
		// check joinSubnet with the default Transit subnet overlap
		netConf = &ovncnitypes.NetConf{
			Role:       strings.ToLower(types.NetworkRolePrimary),
			Topology:   strings.ToLower(types.Layer2Topology),
			Subnets:    "10.12.0.0/16,fd12:dbba::/64",
			JoinSubnet: "100.88.0.0/17,fd97::/65",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.89.0.0/16,fd97:0:0:1::/64"))
		// check Subnet with the default Transit subnet overlap, then joinSubnet overlaps with the next selected transit subnet
		netConf = &ovncnitypes.NetConf{
			Role:       strings.ToLower(types.NetworkRolePrimary),
			Topology:   strings.ToLower(types.Layer2Topology),
			Subnets:    "100.88.0.0/15,fd97::/65",
			JoinSubnet: "100.90.0.0/16,fd97:0:0:1::/64",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.91.0.0/16,fd97:0:0:2::/64"))
	})
})
