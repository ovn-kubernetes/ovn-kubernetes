// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package observabilityconfig holds the ObservabilityConfig CRD admission scenarios
// exercised by the crd-integration suite.
package observabilityconfig

import (
	"fmt"
	"strings"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

// namespacesFilterTemplate is an otherwise valid config whose name and filter.namespaces
// list are substituted in. It is used for the namespace list/name bound cases, whose data
// is too bulky to spell out inline.
const namespacesFilterTemplate = `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: %s
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
  filter:
    namespaces:%s
`

// namespaceItems renders the given namespaces as YAML list items of filter.namespaces.
func namespaceItems(namespaces ...string) string {
	var sb strings.Builder
	for _, ns := range namespaces {
		fmt.Fprintf(&sb, "\n      - %s", ns)
	}
	return sb.String()
}

// nNamespaces renders n distinct namespace list items.
func nNamespaces(n int) string {
	namespaces := make([]string, 0, n)
	for i := range n {
		namespaces = append(namespaces, fmt.Sprintf("ns-%d", i))
	}
	return namespaceItems(namespaces...)
}

// RequiredFieldsInvalid covers the fields the CRD marks as required.
var RequiredFieldsInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "spec is required",
		ExpectedErr: `spec: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-spec
`,
	},
	{
		Description: "spec.collectorID is required",
		ExpectedErr: `spec.collectorID: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-collector-id
spec:
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
	{
		Description: "spec.features is required",
		ExpectedErr: `spec.features: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-features
spec:
  collectorID: 1
`,
	},
	{
		Description: "spec.features[].feature is required",
		ExpectedErr: `spec.features[0].feature: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-feature-name
spec:
  collectorID: 1
  features:
    - probability: 100
`,
	},
	{
		Description: "spec.features[].probability is required",
		ExpectedErr: `spec.features[0].probability: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-probability
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
`,
	},
}

// CollectorIDInvalid covers the collectorID bounds (1..uint32 max). Zero is rejected both as
// an out-of-range value and because it is the Go zero value, so it cannot be left unset.
var CollectorIDInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "collectorID 0 is below the minimum",
		ExpectedErr: `should be greater than or equal to 1`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: collector-id-zero
spec:
  collectorID: 0
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
	{
		Description: "negative collectorID is rejected",
		ExpectedErr: `should be greater than or equal to 1`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: collector-id-negative
spec:
  collectorID: -1
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
	{
		Description: "collectorID above the uint32 maximum is rejected",
		ExpectedErr: `should be less than or equal to 4294967295`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: collector-id-too-big
spec:
  collectorID: 4294967296
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
}

// FeaturesInvalid covers the feature enum, the minimum list size and the listType=map
// uniqueness on the feature key.
var FeaturesInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "unknown feature is rejected",
		ExpectedErr: `Unsupported value: "EgressIP"`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: unknown-feature
spec:
  collectorID: 1
  features:
    - feature: EgressIP
      probability: 100
`,
	},
	{
		Description: "feature enum is case sensitive",
		ExpectedErr: `Unsupported value: "networkpolicy"`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: lowercase-feature
spec:
  collectorID: 1
  features:
    - feature: networkpolicy
      probability: 100
`,
	},
	{
		Description: "empty feature is rejected",
		ExpectedErr: `Unsupported value: ""`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: empty-feature
spec:
  collectorID: 1
  features:
    - feature: ""
      probability: 100
`,
	},
	{
		Description: "empty feature list is rejected",
		ExpectedErr: `should have at least 1 items`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: no-feature-items
spec:
  collectorID: 1
  features: []
`,
	},
	{
		Description: "the same feature cannot be listed twice",
		ExpectedErr: `Duplicate value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: duplicate-feature
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
    - feature: NetworkPolicy
      probability: 10
`,
	},
}

// ProbabilityInvalid covers the per-feature probability bounds (0..100 percent).
var ProbabilityInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "negative probability is rejected",
		ExpectedErr: `should be greater than or equal to 0`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: probability-negative
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: -1
`,
	},
	{
		Description: "probability above 100 percent is rejected",
		ExpectedErr: `should be less than or equal to 100`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: probability-too-big
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 101
`,
	},
}

// FilterInvalid covers the filter: it must set at least one of its fields, the namespaces
// list is bounded and holds unique valid namespace names, and it may only be combined with
// namespaced features.
var FilterInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "an empty filter is rejected",
		ExpectedErr: `should have at least 1 properties`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: empty-filter
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
  filter: {}
`,
	},
	{
		Description: "an empty namespaces list is rejected",
		ExpectedErr: `should have at least 1 items`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: empty-namespaces
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
  filter:
    namespaces: []
`,
	},
	{
		Description: "more than 100 namespaces is rejected",
		ExpectedErr: `must have at most 100 items`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "too-many-namespaces", nNamespaces(101)),
	},
	{
		Description: "duplicate namespaces are rejected",
		ExpectedErr: `Duplicate value: "frontend"`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "duplicate-namespaces", namespaceItems("frontend", "frontend")),
	},
	{
		Description: "a namespace name longer than 63 characters is rejected",
		ExpectedErr: `Too long: may not be more than 63 bytes`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-too-long", namespaceItems(strings.Repeat("a", 64))),
	},
	{
		Description: "an uppercase namespace name is rejected",
		ExpectedErr: `in body should match`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-uppercase", namespaceItems("Frontend")),
	},
	{
		Description: "a namespace name with an underscore is rejected",
		ExpectedErr: `in body should match`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-underscore", namespaceItems("front_end")),
	},
	{
		Description: "a namespace name with a leading dash is rejected",
		ExpectedErr: `in body should match`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-leading-dash", namespaceItems("-frontend")),
	},
	{
		Description: "a namespace name with a trailing dash is rejected",
		ExpectedErr: `in body should match`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-trailing-dash", namespaceItems("frontend-")),
	},
	{
		Description: "an empty namespace name is rejected",
		ExpectedErr: `in body should match`,
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "namespace-empty", namespaceItems(`""`)),
	},
	{
		Description: "namespaces cannot be combined with AdminNetworkPolicy",
		ExpectedErr: `filter.namespaces can only be used with namespaced features`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: namespaces-with-anp
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
    - feature: AdminNetworkPolicy
      probability: 100
  filter:
    namespaces:
      - frontend
`,
	},
	{
		Description: "namespaces cannot be combined with UDNIsolation",
		ExpectedErr: `filter.namespaces can only be used with namespaced features`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: namespaces-with-udn-isolation
spec:
  collectorID: 1
  features:
    - feature: UDNIsolation
      probability: 100
  filter:
    namespaces:
      - frontend
`,
	},
	{
		Description: "namespaces cannot be combined with MulticastIsolation",
		ExpectedErr: `filter.namespaces can only be used with namespaced features`,
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: namespaces-with-multicast-isolation
spec:
  collectorID: 1
  features:
    - feature: MulticastIsolation
      probability: 100
  filter:
    namespaces:
      - frontend
`,
	},
}

// Valid covers the accepted configurations: the minimal config, both ends of every bound,
// each value of the feature enum, and the filter combinations the CEL rule allows.
var Valid = []testscenario.ValidateCRScenario{
	{
		Description: "minimal config: one feature, no filter",
		Name:        "minimal",
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: minimal
spec:
  collectorID: 1
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
	{
		Description: "collectorID at the uint32 maximum",
		Name:        "collector-id-max",
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: collector-id-max
spec:
  collectorID: 4294967295
  features:
    - feature: NetworkPolicy
      probability: 100
`,
	},
	{
		Description: "every feature of the enum, with probability 0 accepted (declared but not sampled)",
		Name:        "all-features",
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: all-features
spec:
  collectorID: 2
  features:
    - feature: NetworkPolicy
      probability: 0
    - feature: AdminNetworkPolicy
      probability: 1
    - feature: EgressFirewall
      probability: 50
    - feature: UDNIsolation
      probability: 99
    - feature: MulticastIsolation
      probability: 100
`,
	},
	{
		Description: "nodeSelector-only filter with a cluster-scoped feature",
		Name:        "node-selector-only",
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: node-selector-only
spec:
  collectorID: 3
  features:
    - feature: UDNIsolation
      probability: 100
  filter:
    nodeSelector:
      matchLabels:
        observability: enabled
`,
	},
	{
		Description: "nodeSelector and namespaces together with namespaced features only",
		Name:        "node-selector-and-namespaces",
		Manifest: `
apiVersion: k8s.ovn.org/v1alpha1
kind: ObservabilityConfig
metadata:
  name: node-selector-and-namespaces
spec:
  collectorID: 4
  features:
    - feature: NetworkPolicy
      probability: 100
    - feature: EgressFirewall
      probability: 10
  filter:
    nodeSelector:
      matchExpressions:
        - key: observability
          operator: Exists
    namespaces:
      - frontend
      - backend
`,
	},
	{
		Description: "100 namespaces is the maximum accepted",
		Name:        "max-namespaces",
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "max-namespaces", nNamespaces(100)),
	},
	{
		Description: "a 63 character namespace name is accepted",
		Name:        "max-length-namespace",
		Manifest:    fmt.Sprintf(namespacesFilterTemplate, "max-length-namespace", namespaceItems(strings.Repeat("a", 63))),
	},
}
