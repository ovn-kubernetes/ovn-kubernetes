package crdintegration

import (
	"context"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
	controllerruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

func cleanupValidateCRsTest(ctx context.Context, k8sClient controllerruntimeclient.Client, scenarios []testscenario.ValidateCRScenario) {
	objs, err := testscenario.ValidateScenariosToObjects(scenarios)
	Expect(err).NotTo(HaveOccurred(), "must convert manifest to object")
	for _, o := range objs {
		err = k8sClient.Delete(ctx, o)
		err = controllerruntimeclient.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), "expected the object to be deleted")
	}
	// Verify each named resource is gone individually — a global "no resources found"
	// check is not parallel-safe since other concurrent tests may have live CUDNs.
	for _, o := range objs {
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, o)
		// ignore the scenario where the object isn't found. We expect the object to not be present
		err = controllerruntimeclient.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), "expected the object to be deleted")
	}
}

func cleanupUpdateCRScenario(ctx context.Context, k8sClient controllerruntimeclient.Client, updateScenarios []testscenario.UpdateCRScenario) {
	scenarios := make([]testscenario.ValidateCRScenario, 0, len(updateScenarios))
	for _, updateScenario := range updateScenarios {
		scenarios = append(scenarios, updateScenario.ValidateCRScenario)
	}
	cleanupValidateCRsTest(ctx, k8sClient, scenarios)
}
