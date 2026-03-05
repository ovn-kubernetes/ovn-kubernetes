package egressfirewall

import (
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	networkqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	fakenetworkmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestEgressFirewall(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "EgressFirewall Suite")
}

var _ = ginkgo.Describe("OVN test basic functions", func() {

	const (
		node1Name string = "node1"
		node1Addr string = "9.9.9.9"
		node2Name string = "node2"
		node2Addr string = "10.10.10.10"
	)

	var (
		app          *cli.App
		nodeLabel    = map[string]string{"use": "this"}
		iFactory     *factory.WatchFactory
		fakeClient   *fake.Clientset
		initialDB    libovsdbtest.TestSetup
		nbClient     libovsdbclient.Client
		nbsbCleanup  *libovsdbtest.Context
		efController *EFController
		node1        = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   node1Name,
				Labels: nodeLabel,
				Annotations: map[string]string{
					util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\"]", node1Addr),
				},
			}}
		node2 = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: node2Name,
				Annotations: map[string]string{
					util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\"]", node2Addr),
				},
			}}
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each test
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		fakeClient = fake.NewSimpleClientset(node1, node2)
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		kubeInterface := &kube.KubeOVN{Kube: kube.Kube{KClient: fakeClient}, ANPClient: anpfake.NewSimpleClientset(),
			EIPClient: egressipv1fake.NewSimpleClientset(), EgressFirewallClient: &egressfirewallfake.Clientset{},
			EgressServiceClient: &egressservicefake.Clientset{}, NetworkQoSClient: &networkqosfake.Clientset{}}

		var err error
		iFactory, err = factory.NewMasterWatchFactory(&util.OVNMasterClientset{
			KubeClient:           fakeClient,
			EgressFirewallClient: kubeInterface.EgressFirewallClient,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		nbClient, _, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(initialDB)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkManager := &fakenetworkmanager.FakeNetworkManager{}
		efController, err = NewEFController("test", "global", kubeInterface, nbClient, iFactory.NamespaceInformer().Lister(),
			iFactory.NodeCoreInformer(), iFactory.EgressFirewallInformer(), networkManager, nil, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = iFactory.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.AfterEach(func() {
		if iFactory != nil {
			iFactory.Shutdown()
		}
		nbsbCleanup.Cleanup()
	})

	ginkgo.It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "((tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "((udp) || (tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "((udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 )))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			gomega.Expect(test.expectedMatch).To(gomega.Equal(l4Match))
		}
	})
	ginkgo.It("computes correct match function", func() {
		type testcase struct {
			clusterSubnets []string
			pgName         string
			ipv4Mode       bool
			ipv6Mode       bool
			destinations   []matchTarget
			ports          []egressfirewallapi.EgressFirewallPort
			output         string
		}
		_, clusterSubnetV4, _ := net.ParseCIDR("10.128.0.0/14")
		_, clusterSubnetV6, _ := net.ParseCIDR("2002:0:0:1234::/64")
		testcases := []testcase{
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", nil}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", nil}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", nil}, {matchKindV6AddressSet, "destv6", nil}},
				ports:          nil,
				output:         "(ip4.dst == $destv4 || ip6.dst == $destv6) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", nil}, {matchKindV6AddressSet, "", nil}},
				ports:          nil,
				output:         "(ip4.dst == $destv4) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6CIDR, "2001::/64", nil}},
				ports:          nil,
				output:         "(ip6.dst == 2001::/64) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", nil}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && inport == @a123456",
			},
			// with cluster subnet exclusion
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", []*net.IPNet{clusterSubnetV4}}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", []*net.IPNet{clusterSubnetV6}}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", []*net.IPNet{clusterSubnetV4}}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && inport == @a123456",
			},
		}

		for _, tc := range testcases {
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets

			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.pgName, tc.destinations, tc.ports)
			gomega.Expect(matchExpression).To(gomega.Equal(tc.output))
		}
	})
	ginkgo.It("correctly parses egressFirewallRules", func() {
		type testcase struct {
			egressFirewallRule egressfirewallapi.EgressFirewallRule
			id                 int
			err                bool
			errOutput          string
			output             egressFirewallRule
			clusterSubnets     []string
			hasNodeSelector    bool
		}
		_, clusterSubnetV4, _ := net.ParseCIDR("10.128.0.0/16")
		_, clusterSubnetV6, _ := net.ParseCIDR("2002:0:0:1234::/64")
		testcases := []testcase{
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32"},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3./32"},
				},
				id:        1,
				err:       true,
				errOutput: "invalid CIDR address: 1.2.3./32",
				output:    egressFirewallRule{},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
				id:  2,
				err: false,
				output: egressFirewallRule{
					id:     2,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
			},
			// check clusterSubnet intersection
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32", clusterSubnetIntersection: nil},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: []*net.IPNet{clusterSubnetV4}},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.0/24"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.0/24", clusterSubnetIntersection: []*net.IPNet{clusterSubnetV4}},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: []*net.IPNet{clusterSubnetV6}},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1235::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1235::/80", clusterSubnetIntersection: nil},
				},
			},
			// dual stack
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: []*net.IPNet{clusterSubnetV4}},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: []*net.IPNet{clusterSubnetV6}},
				},
			},
			// nodeSelector tests
			// selector matches nothing
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To: egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to: destination{nodeAddrs: map[string][]string{}, nodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
				hasNodeSelector: true,
			},
			// empty selector, match all
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: map[string][]string{node1Name: {node1Addr}, node2Name: {node2Addr}}, nodeSelector: &metav1.LabelSelector{}},
				},
				hasNodeSelector: true,
			},
			// match one node
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: map[string][]string{node1Name: {node1Addr}}, nodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
				hasNodeSelector: true,
			},
		}
		for _, tc := range testcases {
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets
			entry := &cacheEntry{subnets: subnetsForNetInfo(&util.DefaultNetInfo{})}
			output, err := efController.newEgressFirewallRule("default", tc.egressFirewallRule, tc.id, entry)
			if tc.err == true {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(tc.errOutput).To(gomega.Equal(err.Error()))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(*output).To(gomega.Equal(tc.output))
			}
			gomega.Expect(entry.hasNodeSelector).To(gomega.Equal(tc.hasNodeSelector))
		}
	})
})

type output struct {
	cidrSelector              string
	dnsName                   string
	clusterSubnetIntersection []*net.IPNet
	nodeSelector              *metav1.LabelSelector
}

func TestValidateAndGetEgressFirewallDestination(t *testing.T) {
	clusterSubnetStr := "10.1.0.0/16"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	extraClusterSubnetStr := "10.2.0.0/16"
	_, extraClusterSubnet, _ := net.ParseCIDR(extraClusterSubnetStr)
	udnClusterSubnetStr := "9.0.0.0/16"
	_, udnClusterSubnet, _ := net.ParseCIDR(udnClusterSubnetStr)
	validUDNName := "udn-test"
	testcases := []struct {
		name                      string
		egressFirewallDestination egressfirewallapi.EgressFirewallDestination
		dnsNameResolverEnabled    bool
		expectedErr               bool
		expectedOutput            output
		udnName                   string
	}{
		{
			name: "should correctly validate dns name",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "www.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "www.example.com",
			},
		},
		{
			name: "should throw an error for wildcard dns name when dns name resolver is not enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            true,
		},
		{
			name: "should correctly validate wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "*.example.com",
			},
		},
		{
			name: "should throw an error for tld dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for tld wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for dns name with more than 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz123456789012.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should validate dns name with 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
		},
		{
			name: "should throw an error for a dns name with a label starting with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "-example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for a dns name with a label ending with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "example-.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should correctly validate cidr selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "1.2.3.5/23",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "1.2.3.5/23",
				clusterSubnetIntersection: nil,
			},
		},
		{
			name: "should throw an error for invalid cidr selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "1.2.3.5",
			},
			expectedErr: true,
		},
		{
			name: "should correctly validate cidr selector and single cluster subnet intersection",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "10.1.1.1/24",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "10.1.1.1/24",
				clusterSubnetIntersection: []*net.IPNet{clusterSubnet},
			},
		},
		{
			name: "should correctly validate cidr selector and multiple cluster subnets intersection",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "0.0.0.0/0",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "0.0.0.0/0",
				clusterSubnetIntersection: []*net.IPNet{clusterSubnet, extraClusterSubnet},
			},
		},
		{
			name: "should correctly validate UDN cidr selector without cluster subnet intersection",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "10.1.1.1/24",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "10.1.1.1/24",
				clusterSubnetIntersection: nil,
			},
			udnName: validUDNName,
		},
		{
			name: "should correctly validate UDN cidr selector with cluster subnet intersection",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "9.0.1.1/24",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "9.0.1.1/24",
				clusterSubnetIntersection: []*net.IPNet{udnClusterSubnet},
			},
			udnName: validUDNName,
		},
		{
			name: "should correctly validate node selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				NodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
			expectedErr: false,
			expectedOutput: output{
				nodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		},
		{
			name: "should correctly validate empty node selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				NodeSelector: &metav1.LabelSelector{},
			},
			expectedErr: false,
			expectedOutput: output{
				nodeSelector: &metav1.LabelSelector{},
			},
		},
	}

	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to PrepareTestConfig: %v", err)
	}

	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}, {CIDR: extraClusterSubnet}}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {

			config.OVNKubernetesFeature.EnableDNSNameResolver = tc.dnsNameResolverEnabled
			netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: validUDNName},
				Topology: types.Layer3Topology,
				Subnets:  udnClusterSubnetStr,
			})
			require.NoError(t, err)
			primaryNetworks := map[string]util.NetInfo{
				validUDNName: netInfo,
			}
			networkManager := &fakenetworkmanager.FakeNetworkManager{PrimaryNetworks: primaryNetworks}
			efController := EFController{networkManager: networkManager}
			network := "default"
			if len(tc.udnName) > 0 {
				network = tc.udnName
			}
			entry := &cacheEntry{subnets: subnetsForNetInfo(&util.DefaultNetInfo{})}
			if len(tc.udnName) > 0 {
				entry.subnets = subnetsForNetInfo(netInfo)
			}

			cidrSelector, dnsName, clusterSubnetIntersection, nodeSelector, err :=
				efController.validateAndGetEgressFirewallDestination(network, tc.egressFirewallDestination, entry)
			if tc.expectedErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedOutput.dnsName, dnsName)
				assert.Equal(t, tc.expectedOutput.cidrSelector, cidrSelector)
				assert.Equal(t, tc.expectedOutput.clusterSubnetIntersection, clusterSubnetIntersection)
				assert.Equal(t, tc.expectedOutput.nodeSelector, nodeSelector)
			}
		})
	}
}
