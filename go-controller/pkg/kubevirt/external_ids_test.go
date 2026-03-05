package kubevirt

import (
	ktypes "k8s.io/apimachinery/pkg/types"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ownsItAndIsOrphanOrWrongZone", func() {
	const (
		defaultControllerName = "default-network-controller"
		udnControllerName     = "cluster_udn_happy-tenant-network-controller"
	)

	var (
		vmKey = ktypes.NamespacedName{Namespace: "ns1", Name: "vm1"}
	)

	makeExternalIDs := func(controllerName string, vm ktypes.NamespacedName) map[string]string {
		return map[string]string{
			OvnZoneExternalIDKey:                   OvnLocalZone,
			string(libovsdbops.OwnerControllerKey): controllerName,
			string(libovsdbops.ObjectNameKey):      vm.String(),
		}
	}

	DescribeTable("controller filtering",
		func(externalIDs map[string]string, vms map[ktypes.NamespacedName]bool, controllerName string, expected bool) {
			Expect(ownsItAndIsOrphanOrWrongZone(externalIDs, vms, controllerName)).To(Equal(expected))
		},

		// --- Core fix: cross-controller protection ---
		Entry("returns false when DHCP options belong to a different controller (UDN)",
			makeExternalIDs(udnControllerName, vmKey),
			map[ktypes.NamespacedName]bool{}, // VM not in default controller's map
			defaultControllerName,
			false,
		),
		Entry("returns false when DHCP options belong to a different controller (default)",
			makeExternalIDs(defaultControllerName, vmKey),
			map[ktypes.NamespacedName]bool{},
			udnControllerName,
			false,
		),

		// --- Orphan detection still works for matching controller ---
		Entry("returns true for orphan resource owned by the matching controller",
			makeExternalIDs(defaultControllerName, vmKey),
			map[ktypes.NamespacedName]bool{}, // VM not found → orphan
			defaultControllerName,
			true,
		),

		// --- VM present and local: not orphan ---
		Entry("returns false when VM is present and zone is local",
			makeExternalIDs(defaultControllerName, vmKey),
			map[ktypes.NamespacedName]bool{vmKey: true}, // VM is local
			defaultControllerName,
			false,
		),

		// --- Wrong zone detection still works ---
		Entry("returns true when VM is local but resource zone is remote",
			map[string]string{
				OvnZoneExternalIDKey:                   OvnRemoteZone,
				string(libovsdbops.OwnerControllerKey): defaultControllerName,
				string(libovsdbops.ObjectNameKey):      vmKey.String(),
			},
			map[ktypes.NamespacedName]bool{vmKey: true},
			defaultControllerName,
			true,
		),

		// --- Non-local VM is not considered wrong zone ---
		Entry("returns false when VM is remote (not local) even if resource zone is remote",
			map[string]string{
				OvnZoneExternalIDKey:                   OvnRemoteZone,
				string(libovsdbops.OwnerControllerKey): defaultControllerName,
				string(libovsdbops.ObjectNameKey):      vmKey.String(),
			},
			map[ktypes.NamespacedName]bool{vmKey: false}, // VM is remote
			defaultControllerName,
			false,
		),

		// --- Existing behavior: no zone key → not kubevirt-managed ---
		Entry("returns false when zone external ID is missing",
			map[string]string{
				string(libovsdbops.OwnerControllerKey): defaultControllerName,
				string(libovsdbops.ObjectNameKey):      vmKey.String(),
			},
			map[ktypes.NamespacedName]bool{},
			defaultControllerName,
			false,
		),

		// --- Existing behavior: no VM key → not kubevirt-related ---
		Entry("returns false when VM cannot be extracted from external IDs",
			map[string]string{
				OvnZoneExternalIDKey:                   OvnLocalZone,
				string(libovsdbops.OwnerControllerKey): defaultControllerName,
			},
			map[ktypes.NamespacedName]bool{},
			defaultControllerName,
			false,
		),

		// --- Legacy resources without OwnerControllerKey ---
		Entry("returns true for orphan resource without OwnerControllerKey (legacy)",
			map[string]string{
				OvnZoneExternalIDKey:              OvnLocalZone,
				string(libovsdbops.ObjectNameKey): vmKey.String(),
			},
			map[ktypes.NamespacedName]bool{}, // VM not found → orphan
			defaultControllerName,
			true,
		),
	)
})
