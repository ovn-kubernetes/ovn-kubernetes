package node

import (
	"context"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	testnm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/gomega"
)

func Test_configureAdvertisedUDNIsolationNFTables(t *testing.T) {
	tests := []struct {
		name               string
		networkManager     networkmanager.Interface
		initialElements    []*knftables.Element
		nad                *nadapi.NetworkAttachmentDefinition
		isAdvertised       bool
		expectedV4Elements []*knftables.Element
		expectedV6Elements []*knftables.Element
	}{
		{
			name: "Should not remove correct V4 and V6 entries to the for advertised network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should remove alien entries",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
				{
					Set: nftablesAdvertisedUDNsSetV4,
					Key: []string{"alien"},
				}, {
					Set: nftablesAdvertisedUDNsSetV6,
					Key: []string{"alien"},
				},
			},
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should remove entries for networks that no longer exist",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.0.0.0/16"},
					Comment: knftables.PtrTo[string]("removedNetwork"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae60::/60"},
					Comment: knftables.PtrTo[string]("removedNetwork"),
				},
			},
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should remove entries if the network no longer contains them",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.129.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae90::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
		},
		{
			name: "Should remove entries if the network is no longer advertised",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			config.IPv4Mode = true
			config.IPv6Mode = true

			nft := nodenft.SetFakeNFTablesHelper()
			tx := nft.NewTransaction()
			set := &knftables.Set{
				Name:    nftablesAdvertisedUDNsSetV4,
				Comment: knftables.PtrTo("advertised UDN V4 subnets"),
				Type:    "ipv4_addr",
				Flags:   []knftables.SetFlag{knftables.IntervalFlag},
			}
			tx.Add(set)

			set = &knftables.Set{
				Name:    nftablesAdvertisedUDNsSetV6,
				Comment: knftables.PtrTo("advertised UDN V6 subnets"),
				Type:    "ipv6_addr",
				Flags:   []knftables.SetFlag{knftables.IntervalFlag},
			}
			tx.Add(set)
			for _, element := range tt.initialElements {
				tx.Add(element)
			}
			err := nft.Run(context.TODO(), tx)
			g.Expect(err).NotTo(HaveOccurred())

			netInfo, err := util.ParseNADInfo(tt.nad)
			g.Expect(err).NotTo(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			if tt.isAdvertised {
				mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{"testNode": {"vrf"}})
			}
			fakeNetworkManager := &testnm.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{tt.nad.Namespace: mutableNetInfo}}

			err = configureAdvertisedUDNIsolationNFTables(fakeNetworkManager, "testNode")
			g.Expect(err).ToNot(HaveOccurred())

			v4Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v4Elems).To(HaveLen(len(tt.expectedV4Elements)))

			v6Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v6Elems).To(HaveLen(len(tt.expectedV6Elements)))

			for i, element := range tt.expectedV4Elements {
				g.Expect(element.Key).To(HaveLen(len(v4Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v4Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v4Elems[i].Comment))
			}
			for i, element := range tt.expectedV6Elements {
				g.Expect(element.Key).To(HaveLen(len(v6Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v6Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v6Elems[i].Comment))
			}
		})
	}
}
