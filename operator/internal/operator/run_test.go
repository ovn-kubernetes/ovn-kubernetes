// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
)

func TestBuildCacheOptions_RestrictsOVNK8sOperatorToOperatorNamespace(t *testing.T) {
	opts := buildCacheOptions()

	// Map keys are client.Object (an interface). A fresh pointer literal won't
	// equal the pointer stored in the map, so iterate and type-assert instead.
	var found bool
	for key, entry := range opts.ByObject {
		if _, ok := key.(*k8sv1alpha1.OVNK8sOperator); !ok {
			continue
		}
		found = true
		if len(entry.Namespaces) != 1 {
			t.Fatalf("expected exactly 1 namespace scope for OVNK8sOperator cache, got %d: %v", len(entry.Namespaces), entry.Namespaces)
		}
		if _, ok := entry.Namespaces[k8sv1alpha1.OperatorNamespace]; !ok {
			t.Fatalf("expected cache scoped to %q, got %v", k8sv1alpha1.OperatorNamespace, entry.Namespaces)
		}
	}
	if !found {
		t.Fatalf("no ByObject entry for *OVNK8sOperator; map has %d entries", len(opts.ByObject))
	}
}

func TestBuildCacheOptions_DoesNotRestrictOtherKinds(t *testing.T) {
	opts := buildCacheOptions()

	// Only the OVNK8sOperator kind should have a ByObject entry.
	// Any other entry would silently restrict a kind we expect cluster-wide.
	if len(opts.ByObject) != 1 {
		t.Fatalf("expected exactly 1 ByObject entry, got %d: %v", len(opts.ByObject), opts.ByObject)
	}
}

func TestUncachedClientOptionsDisablesCache(t *testing.T) {
	options := client.Options{Cache: &client.CacheOptions{}}
	if got := uncachedClientOptions(options); got.Cache != nil {
		t.Fatal("expected cache to be disabled")
	}
}
