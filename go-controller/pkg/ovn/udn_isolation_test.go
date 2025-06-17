package ovn

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UDN Isolation", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	It("ACLs should be updated to the Primary tier ", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeController := getFakeController(DefaultNetworkControllerName)

		// build port group with one ACL that has default tier
		pgIDs := fakeController.getSecondaryPodsPortGroupDbIDs()
		pgName := libovsdbutil.GetPortGroupName(pgIDs)
		egressDenyIDs := fakeController.getUDNACLDbIDs(DenySecondaryACL, libovsdbutil.ACLEgress)
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
})
