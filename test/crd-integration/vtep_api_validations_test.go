// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/vtep"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/feature"
)

var _ = Describe("EVPN: VTEP API validations", feature.RouteAdvertisements, feature.EVPN, func() {
	DescribeTable("api-server should reject invalid VTEP CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid VTEP CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid VTEP configurations", vtep.Invalid),
	)

	DescribeTable("api-server should accept valid VTEP CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid VTEP CR successfully")
			}
		},
		Entry("Valid VTEP configurations", vtep.Valid),
	)

	DescribeTable("api-server should reject invalid VTEP updates",
		func(scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By("Creating initial VTEP: " + s.Description)
				initialObj, updateObj, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, initialObj)
				Expect(err).NotTo(HaveOccurred(), "should create initial VTEP CR successfully")

				By("Updating VTEP (should fail): " + s.Description)
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initialObj.GetNamespace(), Name: initialObj.GetName()}, initialObj)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObj.SetResourceVersion(initialObj.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObj)
				Expect(err).To(HaveOccurred(), "should fail to update VTEP CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid VTEP update configurations", vtep.InvalidUpdates),
	)

	DescribeTable("api-server should accept valid VTEP updates",
		func(scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By("Creating initial VTEP: " + s.Description)
				initialObj, updateObj, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, initialObj)
				Expect(err).NotTo(HaveOccurred(), "should create initial VTEP CR successfully")

				By("Updating VTEP (should succeed): " + s.Description)
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initialObj.GetNamespace(), Name: initialObj.GetName()}, initialObj)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObj.SetResourceVersion(initialObj.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObj)
				Expect(err).NotTo(HaveOccurred(), "should update VTEP CR successfully")
			}
		},
		Entry("Valid VTEP update configurations", vtep.ValidUpdates),
	)
})
