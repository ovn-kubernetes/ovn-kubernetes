// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	testscenariocudn "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/cudn"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/feature"
)

var _ = Describe("Network Segmentation: API validations", feature.NetworkSegmentation, func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})

			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testscenariocudn.MismatchTopologyConfig),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testscenariocudn.LocalnetInvalidRole),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testscenariocudn.LocalnetInvalidPhyNetName),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testscenariocudn.LocalnetInvalidSubnets),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testscenariocudn.LocalnetInvalidMTU),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testscenariocudn.LocalnetInvalidVLAN),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNInvalid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNInvalid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNInvalid),
		Entry("ClusterUserDefinedNetwork, no-overlay, invalid", testscenariocudn.NoOverlayInvalid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3InvalidSubnets),
	)

	DescribeTable("api-server should reject invalid CRs",
		func(updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(ctx, k8sClient, updateScenarios)
			})
			for _, s := range updateScenarios {
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By(s.Description + ": creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
				By(s.Description + ": applying update to CR should fail")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(HaveOccurred(), "should fail to update CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Later3InvalidSubnetsUpdate),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testscenariocudn.LocalnetValid),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNValid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNValid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNValid),
		Entry("ClusterUserDefinedNetwork, no-overlay, valid", testscenariocudn.NoOverlayValid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnets),
	)

	DescribeTable("api-server should accept valid CRs",
		func(updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(ctx, k8sClient, updateScenarios)
			})
			for _, s := range updateScenarios {
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By(s.Description + ": creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
				By(s.Description + ": applying update to CR should succeed")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(BeNil(), "should not fail to update CR")
			}
		},
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnetsUpdates),
	)
})
