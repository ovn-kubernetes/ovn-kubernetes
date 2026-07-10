// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"strings"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UDN Isolation", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.Zone = nodeName
	})

	It("ACLs should be updated to the Primary tier ", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeController := getFakeController(types.DefaultNetworkControllerName)

		// build port group with one ACL that has default tier
		pgIDs := fakeController.getSecondaryPodsPortGroupDbIDs()
		pgName := libovsdbutil.GetPortGroupName(pgIDs)
		egressDenyIDs := fakeController.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLEgress)
		match := libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLEgress)
		// in the real code we use BuildACL here instead of BuildACLWithDefaultTier
		egressDenyACL := libovsdbutil.BuildACLWithDefaultTier(egressDenyIDs, types.PrimaryUDNDenyPriority, match, nbdb.ACLActionDrop,
			nil, libovsdbutil.LportEgress)
		// required to make sure port group correctly references the ACL
		egressDenyACL.UUID = egressDenyIDs.String() + "-UUID"
		pg := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{egressDenyACL})

		nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{egressDenyACL, pg},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer nbCleanup.Cleanup()
		fakeController.nbClient = nbClient

		// now run the setupUDNACLs function which should create all ACLs and update the existing ACLs to the Primary tier
		Expect(fakeController.setupUDNACLs(nil)).To(Succeed())

		// verify that the egressDenyACL is updated to the Primary 0
		acls, err := libovsdbops.FindACLs(nbClient, []*nbdb.ACL{egressDenyACL})
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(1))
		Expect(acls[0].Tier).To(Equal(types.PrimaryACLTier))
	})

	It("Should handle syncing legacy DBIDs", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeController := getFakeController(types.DefaultNetworkControllerName)

		By("initializing the database with legacy secondary IDs")
		pgIDs := fakeController.getSecondaryPodsPortGroupDbIDs()
		pgName := libovsdbutil.GetPortGroupName(pgIDs)
		egressDenyIDs := fakeController.getUDNACLDbIDs(denySecondaryACL, libovsdbutil.ACLEgress)
		match := libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLEgress)
		egressDenyACL := libovsdbutil.BuildACL(egressDenyIDs, types.PrimaryUDNDenyPriority, match, nbdb.ACLActionDrop,
			nil, libovsdbutil.LportEgress, isolationTier)
		// required to make sure port group correctly references the ACL
		egressDenyACL.UUID = egressDenyIDs.String() + "-UUID"

		pg := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{egressDenyACL})

		nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{egressDenyACL, pg},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer nbCleanup.Cleanup()
		fakeController.nbClient = nbClient
		By("running UDN Isolation sync to update ACLs")
		Expect(fakeController.syncUDNIsolation()).To(Succeed())
		By("expect updated port group with proper external_ids")
		pgs, err := libovsdbops.FindPortGroupsWithPredicate(nbClient, func(_ *nbdb.PortGroup) bool { return true })
		Expect(err).NotTo(HaveOccurred())
		Expect(pgs).To(HaveLen(1))
		Expect(pgs[0].ExternalIDs).To(Equal(fakeController.getSecondaryPodsPortGroupDbIDs().GetExternalIDs()))
		By("expect updated ACL with proper external_ids")
		acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(_ *nbdb.ACL) bool { return true })
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(1))
		Expect(acls[0].ExternalIDs).To(Equal(fakeController.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLEgress).GetExternalIDs()))
		By("expect updated ACL with proper name")
		Expect(*acls[0].Name).To(BeEmpty())
	})

	It("queues advertised local nodes when a primary Layer3 UDN adds a subnet", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.AdvertisedUDNIsolationMode = config.AdvertisedUDNIsolationModeStrict
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeOVN := NewFakeOVN(true)

		netInfoBefore := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
		nadBefore, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoBefore.netconf())
		Expect(err).NotTo(HaveOccurred())
		node, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoBefore)
		Expect(err).NotTo(HaveOccurred())

		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{},
			node,
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nadBefore}},
		)
		defer fakeOVN.shutdown()

		l3Controller, ok := fakeOVN.fullL3UDNControllers[netInfoBefore.netName]
		Expect(ok).To(BeTrue())
		mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
		mutableNetInfo.SetNetworkID(2)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)).To(Succeed())
		l3Controller.lastNADKeys = sets.New[string](ns + "/" + nadName)

		netInfoAfter := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16,192.169.0.0/16", "192.168.1.0/24")
		nadAfter, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoAfter.netconf())
		Expect(err).NotTo(HaveOccurred())
		parsedNetInfoAfter, err := util.ParseNADInfo(nadAfter)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfoAfter := util.NewMutableNetInfo(parsedNetInfoAfter)
		mutableNetInfoAfter.SetNetworkID(2)
		mutableNetInfoAfter.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})

		reconciledNodes := sets.New[string]()
		Expect(l3Controller.reconcile(mutableNetInfoAfter, func(node string) {
			reconciledNodes.Insert(node)
		})).To(Succeed())
		Expect(reconciledNodes.Has(nodeName)).To(BeTrue())
	})

	It("updates advertised network isolation ACLs when a primary Layer3 UDN adds a subnet", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.AdvertisedUDNIsolationMode = config.AdvertisedUDNIsolationModeStrict
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeOVN := NewFakeOVN(true)

		netInfoBefore := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
		nadBefore, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoBefore.netconf())
		Expect(err).NotTo(HaveOccurred())
		node, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoBefore)
		Expect(err).NotTo(HaveOccurred())

		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{},
			node,
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nadBefore}},
		)
		defer fakeOVN.shutdown()

		l3Controller, ok := fakeOVN.fullL3UDNControllers[netInfoBefore.netName]
		Expect(ok).To(BeTrue())
		mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
		mutableNetInfo.SetNetworkID(2)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)).To(Succeed())

		_, err = l3Controller.addressSetFactory.EnsureAddressSet(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
		Expect(err).NotTo(HaveOccurred())
		Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, &nbdb.LogicalSwitch{
			Name: l3Controller.GetNetworkScopedSwitchName(nodeName),
		})).To(Succeed())

		Expect(l3Controller.addAdvertisedNetworkIsolation(nodeName)).To(Succeed())
		getPassACL := func() *nbdb.ACL {
			acls, err := libovsdbops.FindACLsWithPredicate(fakeOVN.nbClient, func(acl *nbdb.ACL) bool {
				return acl.Action == nbdb.ACLActionPass &&
					acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] == netInfoBefore.netName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(acls).To(HaveLen(1))
			return acls[0]
		}
		Expect(getPassACL().Match).To(ContainSubstring("192.168.0.0/16"))
		Expect(getPassACL().Match).NotTo(ContainSubstring("192.169.0.0/16"))

		netInfoAfter := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16,192.169.0.0/16", "192.168.1.0/24")
		nadAfter, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoAfter.netconf())
		Expect(err).NotTo(HaveOccurred())
		parsedNetInfoAfter, err := util.ParseNADInfo(nadAfter)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfoAfter := util.NewMutableNetInfo(parsedNetInfoAfter)
		mutableNetInfoAfter.SetNetworkID(2)
		mutableNetInfoAfter.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfoAfter)).To(Succeed())

		Expect(l3Controller.addAdvertisedNetworkIsolation(nodeName)).To(Succeed())
		passACLMatch := getPassACL().Match
		Expect(passACLMatch).To(ContainSubstring("192.168.0.0/16"))
		Expect(passACLMatch).To(ContainSubstring("192.169.0.0/16"))
		Expect(strings.Count(passACLMatch, "ip4.src ==")).To(Equal(2))
		Expect(strings.Count(passACLMatch, "ip4.dst ==")).To(Equal(2))
	})
})
