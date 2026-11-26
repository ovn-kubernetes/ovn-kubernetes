// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"context"

	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

func cleanupValidateCRsTest(ctx context.Context, scenarios []testscenario.ValidateCRScenario) {
	objs, err := testscenario.ValidateScenariosToObjects(scenarios)
	Expect(err).NotTo(HaveOccurred(), "must convert manifest to object")
	for _, o := range objs {
		err = k8sClient.Delete(ctx, o)
		err = controllerruntimeclient.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), "expected the object to be deleted")
	}
	// Verify each named resource is gone individually — a global "no resources found"
	// check is not parallel-safe since other concurrent tests may have live resources.
	for _, o := range objs {
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, o)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected the object to be deleted")
	}
}

func cleanupUpdateCRScenario(ctx context.Context, updateScenarios []testscenario.UpdateCRScenario) {
	scenarios := make([]testscenario.ValidateCRScenario, 0, len(updateScenarios))
	for _, updateScenario := range updateScenarios {
		scenarios = append(scenarios, updateScenario.ValidateCRScenario)
	}
	cleanupValidateCRsTest(ctx, scenarios)
}
