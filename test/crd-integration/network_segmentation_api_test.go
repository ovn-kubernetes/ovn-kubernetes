// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	testscenariocudn "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/cudn"
)

const networkSegmentationSpecTimeout = 30 * time.Second

var _ = Describe("Network Segmentation: API validations", func() {
	DescribeTable("api-server should reject invalid CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(networkSegmentationSpecTimeout))

			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testscenariocudn.MismatchTopologyConfig, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testscenariocudn.LocalnetInvalidRole, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testscenariocudn.LocalnetInvalidPhyNetName, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testscenariocudn.LocalnetInvalidSubnets, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testscenariocudn.LocalnetInvalidMTU, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testscenariocudn.LocalnetInvalidVLAN, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, localnet, invalid macSecurity", testscenariocudn.LocalnetInvalidMACSecurity, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNInvalid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNInvalid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNInvalid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, no-overlay, invalid", testscenariocudn.NoOverlayInvalid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3InvalidSubnets, SpecTimeout(networkSegmentationSpecTimeout)),
	)

	DescribeTable("api-server should reject invalid CRs",
		func(ctx SpecContext, updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(cleanupUpdateCRScenario, updateScenarios, NodeTimeout(networkSegmentationSpecTimeout))
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
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3InvalidSubnetsUpdate, SpecTimeout(networkSegmentationSpecTimeout)),
	)

	DescribeTable("api-server should accept valid CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(networkSegmentationSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testscenariocudn.LocalnetValid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNValid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNValid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNValid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, no-overlay, valid", testscenariocudn.NoOverlayValid, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnets, SpecTimeout(networkSegmentationSpecTimeout)),
	)

	DescribeTable("api-server should reject invalid updates",
		func(ctx SpecContext, scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(cleanupUpdateCRScenario, scenarios, NodeTimeout(networkSegmentationSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By("Creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")

				By("Updating CR")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(HaveOccurred(), "should fail to update CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet, mac-security", testscenariocudn.LocalnetUpdatesRejected, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("ClusterUserDefinedNetwork, layer2, mac-security", testscenariocudn.Layer2CUDNUpdatesRejected, SpecTimeout(networkSegmentationSpecTimeout)),
		Entry("UserDefinedNetwork, layer2, mac-security", testscenariocudn.Layer2UDNUpdatesRejected, SpecTimeout(networkSegmentationSpecTimeout)),
	)

	DescribeTable("api-server should accept valid CRs",
		func(ctx SpecContext, updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(cleanupUpdateCRScenario, updateScenarios, NodeTimeout(networkSegmentationSpecTimeout))
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
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnetsUpdates, SpecTimeout(networkSegmentationSpecTimeout)),
	)
})
