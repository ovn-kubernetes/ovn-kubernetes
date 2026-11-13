// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/vtep"
)

const vtepSpecTimeout = 30 * time.Second

var _ = Describe("EVPN: VTEP API validations", func() {
	DescribeTable("api-server should reject invalid VTEP CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(vtepSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid VTEP CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid VTEP configurations", vtep.Invalid, SpecTimeout(vtepSpecTimeout)),
	)

	DescribeTable("api-server should accept valid VTEP CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(vtepSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid VTEP CR successfully")
			}
		},
		Entry("Valid VTEP configurations", vtep.Valid, SpecTimeout(vtepSpecTimeout)),
	)

	DescribeTable("api-server should reject invalid VTEP updates",
		func(ctx SpecContext, scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(cleanupUpdateCRScenario, scenarios, NodeTimeout(vtepSpecTimeout))
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
		Entry("Invalid VTEP update configurations", vtep.InvalidUpdates, SpecTimeout(vtepSpecTimeout)),
	)

	DescribeTable("api-server should accept valid VTEP updates",
		func(ctx SpecContext, scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(cleanupUpdateCRScenario, scenarios, NodeTimeout(vtepSpecTimeout))
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
		Entry("Valid VTEP update configurations", vtep.ValidUpdates, SpecTimeout(vtepSpecTimeout)),
	)
})
