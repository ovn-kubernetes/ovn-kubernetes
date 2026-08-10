# OVN-Kubernetes E2E Test Contributor Tutorial

A step-by-step guide for adding, modifying, and removing E2E tests and their CI
configuration. Covers all scenarios: new features, new coverage for existing
features, new configuration flags, and feature removal.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Architecture Overview](#architecture-overview)
3. [Scenario A: Adding a New E2E Test for a New Feature](#scenario-a-adding-a-new-e2e-test-for-a-new-feature)
4. [Scenario B: Adding Coverage to an Existing Feature](#scenario-b-adding-coverage-to-an-existing-feature)
5. [Scenario C: Adding a New Configuration Flag](#scenario-c-adding-a-new-configuration-flag)
6. [Scenario D: Adding a New CI Matrix Lane](#scenario-d-adding-a-new-ci-matrix-lane)
7. [Scenario E: Removing a Feature, Flag, or CI Lane](#scenario-e-removing-a-feature-flag-or-ci-lane)
8. [Scenario F: Modifying Test Selection (Skip/Focus)](#scenario-f-modifying-test-selection-skipfocus)
9. [Running and Validating Locally](#running-and-validating-locally)
10. [Common Patterns and Recipes](#common-patterns-and-recipes)
11. [Troubleshooting](#troubleshooting)
12. [Checklist Reference](#checklist-reference)

---

## Prerequisites

Before making changes, ensure you have:

- Go 1.25+ installed
- Docker (or Podman) running
- `jinjanate` Python package (`pip install jinjanate`)
- ~20 GB free disk space
- Familiarity with [Ginkgo v2](https://onsi.github.io/ginkgo/) testing framework
- Access to the repository (`git clone`)

Build the project first to verify your environment:

```bash
cd go-controller && make && cd ..
```

---

## Architecture Overview

Understanding where each piece lives is essential before making changes. The test
infrastructure has six layers, and depending on your scenario, you may touch some
or all of them:

```
Layer 1: Test Code         test/e2e/*.go              Ginkgo Describe/It blocks
Layer 2: Feature Labels    test/e2e/feature/           Feature:* label definitions
Layer 3: Test Scripts      test/scripts/e2e-cp.sh      Skip/focus/label filter logic
Layer 4: Cluster Setup     contrib/kind-helm.sh        Kind cluster + Helm deploy
                           contrib/kind-common.sh      Env var defaults + validation
Layer 5: Helm Charts       helm/ovn-kubernetes/        Feature flag -> container config
Layer 6: CI Workflow        .github/workflows/test.yml  Matrix lanes + env var mapping
```

**Data flow** for a feature flag:

```
CI matrix entry (test.yml)
  -> environment variable (e.g., ENABLE_MY_FEATURE=true)
    -> kind-helm.sh CLI flag (e.g., --my-feature-enable)
      -> Helm --set value (e.g., global.enableMyFeature=true)
        -> container env var (e.g., OVN_MY_FEATURE_ENABLE=true)
          -> Go controller reads env var and enables feature
            -> e2e-cp.sh reads env var and includes/excludes tests
              -> Ginkgo label filter selects tests with Feature:MyFeature
```

---

## Scenario A: Adding a New E2E Test for a New Feature

This is the most comprehensive scenario. You are adding a brand-new feature to
OVN-Kubernetes and need end-to-end tests for it. This touches all six layers.

### Step 1: Define the Feature Label

**File**: `test/e2e/feature/features.go`

Add your feature to the `var` block. The label name should match the feature
concept, not a Go package name.

```go
var (
	// ... existing features ...
	NetworkConnect        = New("NetworkConnect")
	// Add your feature here, alphabetical order preferred:
	MyFeature             = New("MyFeature")
)
```

This creates a Ginkgo label `Feature:MyFeature` that you will use in your test
`Describe` blocks and that the CI scripts use for filtering.

**How it works**: `New("MyFeature")` calls `label.New("Feature", "MyFeature").GinkgoLabel()`
which produces `ginkgo.Label("Feature:MyFeature")`.

### Step 2: Write the Test File

**File**: `test/e2e/myfeature.go` (new file)

Create a new Go file in the `test/e2e/` package. Use this template:

```go
package e2e

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	// Import feature labels
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	// Import infra provider for cluster operations
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	// Import deployment config for environment queries
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
)

var _ = ginkgo.Describe("My Feature", feature.MyFeature, func() {
	f := wrappedTestFramework("myfeature")

	// Use Context blocks to group related tests
	ginkgo.Context("basic functionality", func() {

		ginkgo.It("should do the expected thing", func() {
			// Get cluster nodes
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(
				context.TODO(), f.ClientSet, 2,
			)
			framework.ExpectNoError(err)
			gomega.Expect(nodes.Items).To(gomega.HaveLen(2),
				"need at least 2 schedulable nodes")

			// Create a test pod
			pod := e2epod.NewAgnhostPod(
				f.Namespace.Name, "test-pod", nil, nil, nil,
			)
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(
				context.TODO(), pod, metav1.CreateOptions{},
			)
			framework.ExpectNoError(err)

			// Wait for it to be running
			err = e2epod.WaitForPodRunningInNamespace(
				context.TODO(), f.ClientSet, pod,
			)
			framework.ExpectNoError(err)

			// Your test assertions here
			// ...
		})
	})

	ginkgo.Context("edge cases", func() {

		ginkgo.It("should handle the error scenario", func() {
			// Negative test
			// ...
		})
	})
})
```

**Key patterns**:

| Pattern | Usage |
|---------|-------|
| `wrappedTestFramework("name")` | Creates the test framework with coredump detection and OVN DB collection on failure. Always use this, not `framework.NewDefaultFramework`. |
| `feature.MyFeature` | Attaches the feature label to the Describe block. Place it as the second argument. |
| `f.Namespace.Name` | Each test gets an auto-created, auto-cleaned namespace. |
| `f.ClientSet` | Pre-configured Kubernetes client. |
| `infraprovider.Get()` | Access cluster operations (exec on nodes, external containers, etc.). |
| `deploymentconfig.Get()` | Query deployment configuration (namespace names, bridge names, etc.). |

**Running commands on cluster nodes**:

```go
output, err := infraprovider.Get().ExecK8NodeCommand(
	nodeName,
	[]string{"ovs-vsctl", "show"},
)
```

**Creating external containers** (for testing external connectivity):

```go
ctx := infraprovider.Get().NewTestContext()
defer ctx.CleanUp()

container, err := ctx.CreateExternalContainer(
	"external-server",
	images.AgnHost(),
	[]string{"netexec", "--http-port=80"},
	nil, // networks
)
```

**Conditional skips** (when the test requires specific cluster state):

```go
ginkgo.It("requires shared gateway mode", func() {
	if isLocalGWModeEnabled() {
		ginkgo.Skip("this test requires shared gateway mode")
	}
	// ... test logic ...
})
```

### Step 3: Register Test Images (If Needed)

**File**: `test/e2e/images/images.go`

If your test uses container images beyond the standard `agnhost`, register them
so they are preloaded into the Kind cluster.

Add a package-level variable and env-var override:

```go
// At the top with other image vars:
var myTestImage = "ghcr.io/ovn-kubernetes/ovn-kubernetes/test-images/myimage:v1.0"

// In init():
func init() {
	if img := os.Getenv("MY_TEST_IMAGE"); img != "" {
		myTestImage = img
	}
}

// Add a getter:
func MyTestImage() string {
	return myTestImage
}
```

Then register it for preloading -- add it to the `Required()` function or call
`images.Add()`:

```go
func init() {
	Add(myTestImage)
}
```

### Step 4: Add the Feature Flag to the Cluster Setup

This step is needed only if your feature requires a configuration flag to be
enabled in OVN-Kubernetes. If your tests work with the default configuration,
skip to Step 6.

#### 4a: Add the Helm Value

**File**: `helm/ovn-kubernetes/values-single-node-zone.yaml`

Add your feature flag under the `global:` section:

```yaml
global:
  # ... existing flags ...
  enableMyFeature: false
```

#### 4b: Wire the Helm Value to a Container Environment Variable

**File**: `helm/ovn-kubernetes/charts/ovnkube-single-node-zone/templates/ovnkube-single-node-zone.yaml`

In the container environment section, add:

```yaml
- name: OVN_MY_FEATURE_ENABLE
  value: {{ hasKey .Values.global "enableMyFeature" | ternary .Values.global.enableMyFeature false | quote }}
```

If the feature also affects the control-plane, add the same entry in:
`helm/ovn-kubernetes/charts/ovnkube-control-plane/templates/ovnkube-control-plane.yaml`

#### 4c: Add the Environment Variable Default

**File**: `contrib/kind-common.sh`

In the `set_common_default_params()` function, add your variable with its default
and any validation:

```bash
ENABLE_MY_FEATURE=${ENABLE_MY_FEATURE:-false}
# Add validation if your feature depends on another:
if [ "$ENABLE_MY_FEATURE" == true ] && [ "$SOME_OTHER_FEATURE" != true ]; then
    echo "My feature requires some-other-feature to be enabled"
    exit 1
fi
```

#### 4d: Add the CLI Flag to kind-helm.sh

**File**: `contrib/kind-helm.sh`

Add flag parsing in the argument processing loop:

```bash
# In the case statement (around line 110-350):
-mfe | --my-feature-enable)
    ENABLE_MY_FEATURE=true
    ;;
```

Add the Helm `--set` in the `create_ovn_kubernetes()` function (around line 500-700):

```bash
--set global.enableMyFeature=$(if [ "${ENABLE_MY_FEATURE}" == "true" ]; then echo "true"; else echo "false"; fi) \
```

### Step 5: Add Test Script Filtering

**File**: `test/scripts/e2e-cp.sh`

Add filtering logic so tests with `Feature:MyFeature` are only run when the
feature is enabled. Insert this block alongside the other feature blocks
(around lines 130-200):

```bash
# My Feature tests
if [ "$ENABLE_MY_FEATURE" != "true" ]; then
  skip_label "Feature:MyFeature"
fi
```

This ensures that when `ENABLE_MY_FEATURE` is not set to `true`, all tests
labeled `Feature:MyFeature` are excluded.

If your feature tests should only run when explicitly targeted (like External
Gateway or KubeVirt tests), use the `WHAT`-based gating pattern instead:

```bash
MY_FEATURE_TESTS="My Feature"
if [[ "${WHAT}" == "${MY_FEATURE_TESTS}"* ]]; then
  require_label "Feature:MyFeature"
elif [[ "${WHAT}" != "" ]]; then
  skip "My Feature"
fi
```

### Step 6: Add the CI Matrix Lane

**File**: `.github/workflows/test.yml`

#### 6a: Add a Matrix Entry

In the `e2e` job's `strategy.matrix.include` array, add your lane:

```yaml
- {"target": "my-feature", "ha": "noHA", "gateway-mode": "shared",
   "ipfamily": "dualstack", "disable-snat-multiple-gws": "noSnatGW",
   "second-bridge": "1br", "ic": "ic-single-node-zones",
   "my-feature": "enable-my-feature"}
```

#### 6b: Map the Matrix Value to an Environment Variable

In the `env:` section of the `e2e` job, add the mapping:

```yaml
ENABLE_MY_FEATURE: "${{ matrix.my-feature == 'enable-my-feature' }}"
```

If your feature should also be enabled in other existing lanes (e.g., the target
is another test category that happens to need your feature), you can add
conditions:

```yaml
ENABLE_MY_FEATURE: "${{ matrix.target == 'my-feature' || matrix.my-feature == 'enable-my-feature' }}"
```

#### 6c: Add the Test Dispatch Logic

In the "Run Tests" step's `if/elif` chain, add your target:

```bash
elif [ "${{ matrix.target }}" == "my-feature" ]; then
  make -C test control-plane WHAT="My Feature"
```

The `WHAT` value must match the beginning of your `ginkgo.Describe` string
in Step 2. The `make control-plane WHAT="My Feature"` command passes this
to `e2e-cp.sh` which uses it as a `--focus` pattern.

#### 6d: Set a Timeout (If Needed)

If your tests need more than the default 130 minutes, add your target to the
timeout expression:

```yaml
timeout-minutes: ${{ startsWith(matrix.target, 'bgp') && 190 || matrix.target == 'my-feature' && 190 || 130 }}
```

### Step 7: Verify Locally

Follow the instructions in [Running and Validating Locally](#running-and-validating-locally)
to verify your tests work before submitting a PR.

### Summary: Files Changed for a New Feature

| File | Change |
|------|--------|
| `test/e2e/feature/features.go` | Add `MyFeature = New("MyFeature")` |
| `test/e2e/myfeature.go` | New test file |
| `test/e2e/images/images.go` | Register custom images (if needed) |
| `test/scripts/e2e-cp.sh` | Add `skip_label "Feature:MyFeature"` filter |
| `contrib/kind-common.sh` | Add `ENABLE_MY_FEATURE` default + validation |
| `contrib/kind-helm.sh` | Add CLI flag + `--set` mapping |
| `helm/ovn-kubernetes/values-single-node-zone.yaml` | Add `enableMyFeature: false` |
| `helm/ovn-kubernetes/charts/*/templates/*.yaml` | Add container env var |
| `.github/workflows/test.yml` | Add matrix entry + env var mapping + dispatch |

---

## Scenario B: Adding Coverage to an Existing Feature

You want to add more test cases to an already-tested feature (e.g., adding more
EgressIP scenarios). This is the simplest scenario.

### Step 1: Identify the Existing Test File and Label

Find the test file for your feature:

| Feature | Test File | Label |
|---------|----------|-------|
| EgressIP | `test/e2e/egressip.go` | `feature.EgressIP` |
| Services | `test/e2e/service.go` | `feature.Service` |
| Network Segmentation | `test/e2e/network_segmentation*.go` | `feature.NetworkSegmentation` |
| BGP | `test/e2e/route_advertisements.go` | `feature.RouteAdvertisements` |
| External Gateway | `test/e2e/external_gateways.go` | `feature.ExternalGateway` |
| Multicast | `test/e2e/multicast.go` | `feature.Multicast` |
| Multi-Homing | `test/e2e/multihoming.go` | `feature.MultiHoming` |
| KubeVirt | `test/e2e/kubevirt.go` | `feature.VirtualMachineSupport` |
| Egress Firewall | `test/e2e/egress_firewall.go` | `feature.EgressFirewall` |
| Egress Services | `test/e2e/egress_services.go` | `feature.EgressService` |

See `test/README.md` for the complete list.

### Step 2: Add Your Test Cases

Add new `ginkgo.It` or `ginkgo.Context` blocks inside the existing `Describe`:

```go
// In an existing file, e.g., egressip.go:

// Inside the existing Describe block:
ginkgo.Context("my new scenario", func() {
    ginkgo.It("should handle the new case", func() {
        // Use the existing framework variable 'f'
        // ...
    })
})
```

If the existing `Describe` block is getting too large, you can create a
new top-level `Describe` in the same file or in a new file, as long as you
use the same feature label:

```go
// In a new file: test/e2e/egressip_new_scenarios.go
package e2e

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

var _ = ginkgo.Describe("EgressIP new scenarios", feature.EgressIP, func() {
	f := wrappedTestFramework("egressip-new")
	// ...
})
```

Because you use the same `feature.EgressIP` label, the test will automatically
run in all CI lanes that already include EgressIP tests.

### Step 3: Handle Configuration-Specific Tests

If your new test only works in certain configurations, add a skip inside the test:

```go
ginkgo.It("requires local gateway mode", func() {
	if !isLocalGWModeEnabled() {
		ginkgo.Skip("this test requires local gateway mode")
	}
	// ...
})
```

Common skip helpers available in `test/e2e/util.go`:
- `isLocalGWModeEnabled()` -- checks `OVN_GATEWAY_MODE` env var
- `IsGatewayModeLocal(cs)` -- checks the node annotation at runtime

### Step 4: Verify

No CI changes needed. Your tests will automatically run in the lanes that already
enable the feature. Test locally with:

```bash
export ENABLE_MY_FEATURE=true  # whatever feature flag
make -C test install-kind
make -C test control-plane WHAT="My new test name"
```

### Summary: Files Changed

| File | Change |
|------|--------|
| `test/e2e/<existing_feature>.go` | Add new `It`/`Context` blocks |
| (or) `test/e2e/<feature>_new.go` | New file with same feature label |

No infrastructure changes needed.

---

## Scenario C: Adding a New Configuration Flag

You are adding a new configuration option (not a full feature, just a flag)
that changes OVN-Kubernetes behavior, and you need to test it.

### Step 1: Add the Helm Value

**File**: `helm/ovn-kubernetes/values-single-node-zone.yaml`

```yaml
global:
  myNewOption: "default-value"
```

### Step 2: Wire to Container Config

**File**: `helm/ovn-kubernetes/charts/ovnkube-single-node-zone/templates/ovnkube-single-node-zone.yaml`

```yaml
- name: OVN_MY_NEW_OPTION
  value: {{ default "" .Values.global.myNewOption | quote }}
```

### Step 3: Add to kind-helm.sh

**File**: `contrib/kind-helm.sh`

Add the CLI flag:

```bash
-mno | --my-new-option)
    OVN_MY_NEW_OPTION="${2}"
    shift
    ;;
```

Add the Helm `--set`:

```bash
--set global.myNewOption="${OVN_MY_NEW_OPTION:-default-value}" \
```

### Step 4: Add the Default in kind-common.sh

**File**: `contrib/kind-common.sh`

```bash
OVN_MY_NEW_OPTION=${OVN_MY_NEW_OPTION:-"default-value"}
```

### Step 5: Test the Flag

You have two options:

**Option A: Add tests within an existing feature** (if the flag modifies an
existing feature's behavior):

```go
ginkgo.It("behaves differently with my-new-option", func() {
	// Check the option value or skip
	if os.Getenv("OVN_MY_NEW_OPTION") != "special-value" {
		ginkgo.Skip("requires OVN_MY_NEW_OPTION=special-value")
	}
	// ...
})
```

**Option B: Add a CI lane** that sets the flag to a non-default value:

```yaml
# In test.yml matrix.include:
- {"target": "control-plane", "ha": "noHA", "gateway-mode": "shared",
   "ipfamily": "ipv4", "disable-snat-multiple-gws": "snatGW",
   "second-bridge": "1br", "ic": "ic-single-node-zones",
   "my-new-option": "special-value"}
```

Add the env var mapping:

```yaml
OVN_MY_NEW_OPTION: "${{ matrix.my-new-option || 'default-value' }}"
```

### Summary: Files Changed

| File | Change |
|------|--------|
| `helm/ovn-kubernetes/values-single-node-zone.yaml` | Add value |
| `helm/ovn-kubernetes/charts/*/templates/*.yaml` | Add container env var |
| `contrib/kind-helm.sh` | Add CLI flag + `--set` |
| `contrib/kind-common.sh` | Add default |
| `test/e2e/<feature>.go` | Add test cases |
| `.github/workflows/test.yml` | Add matrix entry (optional) |

---

## Scenario D: Adding a New CI Matrix Lane

You want to test an existing feature in a new configuration combination
(e.g., adding a BGP lane with IPv6-only, or adding HA mode to network-segmentation).

### Step 1: Define the Matrix Entry

**File**: `.github/workflows/test.yml`

Add a new entry to `strategy.matrix.include` in the `e2e` job. Use existing
entries as templates. Every entry must have at minimum:

```yaml
- {"target": "<make-target>",
   "ha": "noHA|HA",
   "gateway-mode": "local|shared",
   "ipfamily": "ipv4|ipv6|dualstack",
   "disable-snat-multiple-gws": "snatGW|noSnatGW",
   "second-bridge": "1br|2br",
   "ic": "ic-single-node-zones"}
```

Add feature-specific keys as needed:

```yaml
- {"target": "network-segmentation", "ha": "HA", "gateway-mode": "shared",
   "ipfamily": "dualstack", "disable-snat-multiple-gws": "noSnatGW",
   "second-bridge": "1br", "ic": "ic-single-node-zones",
   "network-segmentation": "enable-network-segmentation"}
```

### Step 2: Verify Environment Variable Mapping

Check that the `env:` section already maps your matrix keys to the right
environment variables. For example, `network-segmentation` is mapped by:

```yaml
ENABLE_NETWORK_SEGMENTATION: "${{ startsWith(matrix.target, 'network-segmentation') || matrix.network-segmentation == 'enable-network-segmentation' }}"
```

If your new lane uses an existing target name (e.g., `network-segmentation`) with
only different parameters (e.g., `ha: HA`), no env var changes are needed.

### Step 3: Verify Test Dispatch

Check the "Run Tests" step to ensure your target has a dispatch entry. If you
are using an existing target name, it is already there. If you created a new
target name, add a dispatch block.

### Step 4: Update the Job Name (Automatic)

The job name is auto-generated:

```yaml
JOB_NAME: "${{ matrix.target }}-${{ matrix.ha }}-${{ matrix.gateway-mode }}-${{ matrix.ipfamily }}-..."
```

Your new lane will get a unique name automatically.

### Step 5: Consider the Impact

Before adding a lane, consider:

- **Does it provide meaningful new coverage?** Each lane takes 30+ minutes of
  CI time. Adding a lane that duplicates existing coverage wastes resources.
- **Can the same coverage be achieved with a runtime skip/conditional?** If the
  only difference is a single flag, consider adding a conditional test instead.
- **Is this a primary or secondary configuration?** If secondary, consider making
  it run only on scheduled/merge-queue runs:

```yaml
# Run only on schedule or merge queue, not on every PR:
- {"target": "my-niche-config", ...,
   "schedule-only": "true"}
```

Then add a condition:

```yaml
if: ${{ github.event_name == 'schedule' || matrix.schedule-only != 'true' }}
```

### Summary: Files Changed

| File | Change |
|------|--------|
| `.github/workflows/test.yml` | Add matrix entry (+ env mapping and dispatch if new target) |

---

## Scenario E: Removing a Feature, Flag, or CI Lane

### Removing a CI Matrix Lane

**File**: `.github/workflows/test.yml`

1. Remove the matrix entry from `strategy.matrix.include`.
2. If this was the only lane using a specific `target` name, remove the dispatch
   block from the "Run Tests" step.
3. If this was the only lane using a specific matrix key (e.g., `my-feature`),
   remove the env var mapping from the `env:` section.

### Removing a Feature Entirely

Work backwards through the layers:

| Step | File | Action |
|------|------|--------|
| 1 | `.github/workflows/test.yml` | Remove matrix entries with this feature |
| 2 | `.github/workflows/test.yml` | Remove env var mapping for the feature |
| 3 | `test/scripts/e2e-cp.sh` | Remove `skip_label` / `require_label` blocks |
| 4 | `test/e2e/<feature>.go` | Delete the test file |
| 5 | `test/e2e/feature/features.go` | Remove the feature label variable |
| 6 | `contrib/kind-helm.sh` | Remove CLI flag and `--set` mapping |
| 7 | `contrib/kind-common.sh` | Remove env var default and validation |
| 8 | `helm/ovn-kubernetes/values-single-node-zone.yaml` | Remove Helm value |
| 9 | `helm/ovn-kubernetes/charts/*/templates/*.yaml` | Remove container env var |

### Removing a Configuration Flag

Same as removing a feature, but you may keep the test file if the tests still
apply without the flag. Only remove the infrastructure pieces (Helm values,
kind-helm.sh flag, kind-common.sh default, CI env var mapping).

### Removing Skip Patterns

If a previously skipped test is now fixed and should run:

**File**: `test/scripts/e2e-cp.sh`

Remove the specific `skip "test name pattern"` call. Be careful to verify the
test actually passes in the configurations where it was being skipped.

**File**: `test/scripts/e2e-kind.sh`

For upstream K8s tests, remove the test name from the `SKIPPED_TESTS` variable.

### Deprecation Pattern

For a gradual removal, rather than deleting everything at once:

1. First PR: Add a `ginkgo.Skip("deprecated: <reason>")` to the test's
   `BeforeEach` to disable it without deleting code.
2. Second PR: Remove the CI lane (if the feature is the only thing in that lane).
3. Third PR: Remove all infrastructure code (flag, Helm value, etc.).
4. Final PR: Delete the test file and feature label.

---

## Scenario F: Modifying Test Selection (Skip/Focus)

### Adding a Skip for a Specific Configuration

If a test doesn't work in a specific configuration (e.g., IPv6-only), add the
skip inside the test code, not in the shell script:

```go
ginkgo.It("my test that needs IPv4", func() {
	if !isIPv4Supported() {
		ginkgo.Skip("requires IPv4")
	}
	// ...
})
```

**Prefer runtime skips over shell script skips** because they are:
- Self-documenting (the reader sees why the test is skipped)
- Closer to the test logic
- Harder to accidentally orphan

### When Shell Script Skips Are Appropriate

Use shell script skips (`e2e-cp.sh`) when:
- An entire feature group must be excluded (use `skip_label`)
- A test only runs when explicitly requested (use `WHAT`-based gating)
- An upstream K8s test (in `e2e-kind.sh`) needs to be excluded

### Adding a Focus Pattern

To run only a subset of tests in a CI lane, modify the dispatch in `test.yml`:

```bash
make -C test control-plane WHAT="My Specific Test Context"
```

The `WHAT` value is passed to `--ginkgo.focus` as a regex. It matches against
the full test name (Describe + Context + It text concatenated).

### Adding IPv6-Specific Skips

**File**: `test/scripts/e2e-cp.sh`

The script has a dedicated section for IPv6 skips. Add your test name to the
existing list:

```bash
if [ "$PLATFORM_IPV6_SUPPORT" == true ] && [ "$PLATFORM_IPV4_SUPPORT" != true ]; then
  # IPv6-only skips
  skip "my test that does not work on IPv6-only"
  # ...
fi
```

### Adding Gateway Mode Skips

```bash
if [ "$OVN_GATEWAY_MODE" == "local" ]; then
  skip "my test that requires shared gateway"
fi
```

---

## Running and Validating Locally

### Quick Test Run (Existing Cluster)

If you already have a Kind cluster with OVN-K running:

```bash
# Run your specific test
make -C test control-plane WHAT="My Feature"

# Run a single test case
make -C test control-plane WHAT="should do the expected thing"
```

### Full Run (From Scratch)

```bash
# 1. Set environment variables for your configuration
export OVN_GATEWAY_MODE=shared
export OVN_HA=false
export PLATFORM_IPV4_SUPPORT=true
export PLATFORM_IPV6_SUPPORT=true
export KIND_INSTALL_INGRESS=true
export KIND_ALLOW_SYSTEM_WRITES=true
export ENABLE_MY_FEATURE=true

# 2. Build the image and create the cluster
make -C test install-kind

# 3. Run your tests
make -C test control-plane WHAT="My Feature"
```

### Iterating on Tests

After your cluster is running, you can modify tests and re-run without
recreating the cluster:

```bash
# Edit your test file
vim test/e2e/myfeature.go

# Re-run (Go recompiles automatically)
make -C test control-plane WHAT="My Feature"
```

### Running with Verbose Output

```bash
# The control-plane target passes -v to go test
# For more verbose ginkgo output, set:
export GINKGO_VERBOSE=true
make -C test control-plane WHAT="My Feature"
```

### Running in Parallel vs Serial

```bash
# Parallel (default in CI)
export PARALLEL=true
make -C test control-plane

# Serial (to isolate failures)
export PARALLEL=false
make -C test control-plane WHAT="My Feature"
```

### Running Upstream K8s Tests

```bash
# Conformance tests
make -C test shard-conformance

# Specific upstream test
make -C test shard-test WHAT="should provide DNS for services"
```

### Checking Which Tests Would Run

To preview which tests would be selected without actually running them:

```bash
cd test/e2e
go test -v -list ".*" -run "^$" . 2>&1 | head -50
```

To check ginkgo label filtering:

```bash
cd test/e2e
go test -v -run "^$" -ginkgo.dry-run -ginkgo.label-filter "Feature:MyFeature" .
```

### Cleanup

```bash
# Delete the Kind cluster
kind delete cluster --name ovn

# Clean everything
contrib/kind-helm.sh --delete
```

---

## Common Patterns and Recipes

### Recipe: Test That Needs an External Container

Some tests require a container outside the Kubernetes cluster (e.g., an external
BGP peer or HTTP server):

```go
var _ = ginkgo.Describe("My Feature", feature.MyFeature, func() {
	f := wrappedTestFramework("myfeature")

	ginkgo.It("connects to an external server", func() {
		ctx := infraprovider.Get().NewTestContext()
		defer ctx.CleanUp()

		// Create an external container attached to the Kind network
		container, err := ctx.CreateExternalContainer(
			"ext-server",
			images.AgnHost(),
			[]string{"netexec", "--http-port=8080"},
			nil,
		)
		framework.ExpectNoError(err)

		serverIP := container.GetIPv4()
		// ... create a pod and verify connectivity to serverIP:8080 ...
	})
})
```

### Recipe: Test That Needs Multiple Nodes

```go
ginkgo.It("validates inter-node traffic", func() {
	nodes, err := e2enode.GetBoundedReadySchedulableNodes(
		context.TODO(), f.ClientSet, 2,
	)
	framework.ExpectNoError(err)
	if len(nodes.Items) < 2 {
		ginkgo.Skip("requires at least 2 nodes")
	}

	node1 := nodes.Items[0].Name
	node2 := nodes.Items[1].Name

	// Create pods pinned to specific nodes using NodeSelector
	pod1 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod1", nil, nil, nil)
	pod1.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node1}
	// ...
})
```

### Recipe: Test That Checks OVN Database State

```go
ginkgo.It("creates the expected OVN logical switch", func() {
	// Run ovn-nbctl on a cluster node
	output, err := infraprovider.Get().ExecK8NodeCommand(
		nodeName,
		[]string{"ovn-nbctl", "ls-list"},
	)
	framework.ExpectNoError(err)
	gomega.Expect(output).To(gomega.ContainSubstring("expected-switch-name"))
})
```

### Recipe: Test That Verifies Traffic Flow

```go
ginkgo.It("routes traffic through the correct path", func() {
	// Create server pod
	serverPod := e2epod.NewAgnhostPod(
		f.Namespace.Name, "server", nil, nil, nil,
		"netexec", "--http-port=8080",
	)
	// ... create and wait for running ...

	// Create client pod and exec curl
	clientPod := e2epod.NewAgnhostPod(
		f.Namespace.Name, "client", nil, nil, nil,
	)
	// ... create and wait for running ...

	cmd := fmt.Sprintf("curl -s http://%s:8080/hostname", serverPod.Status.PodIP)
	output, err := e2epod.ExecShellInPod(
		context.TODO(), f, clientPod.Namespace, clientPod.Name, cmd,
	)
	framework.ExpectNoError(err)
	gomega.Expect(output).To(gomega.Equal("server"))
})
```

### Recipe: Test That Uses CRDs

```go
import (
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
)

ginkgo.It("creates an EgressIP", func() {
	// Build a CRD client from kubeconfig
	config, err := framework.LoadConfig()
	framework.ExpectNoError(err)

	client, err := egressipclient.NewForConfig(config)
	framework.ExpectNoError(err)

	eip := &egressipv1.EgressIP{
		ObjectMeta: metav1.ObjectMeta{Name: "test-eip"},
		Spec: egressipv1.EgressIPSpec{
			EgressIPs: []string{"10.0.0.100"},
			// ...
		},
	}

	_, err = client.K8sV1().EgressIPs().Create(
		context.TODO(), eip, metav1.CreateOptions{},
	)
	framework.ExpectNoError(err)
	defer client.K8sV1().EgressIPs().Delete(
		context.TODO(), eip.Name, metav1.DeleteOptions{},
	)
	// ... verify behavior ...
})
```

### Recipe: Serial Test

For tests that cannot run in parallel with others (e.g., they modify cluster-wide
state):

```go
var _ = ginkgo.Describe("My disruptive test",
	ginkgo.Serial,
	feature.MyFeature,
	func() {
		// This test will never run in parallel with other tests
	},
)
```

Serial tests are excluded from parallel CI lanes (which set `PARALLEL=true`)
and run in the `serial` CI lane.

### Recipe: Ordered Tests

For tests that must run in a specific sequence:

```go
ginkgo.Context("ordered lifecycle", ginkgo.Ordered, func() {
	var resource *MyResource

	ginkgo.It("creates the resource", func() {
		resource = createResource()
		gomega.Expect(resource).NotTo(gomega.BeNil())
	})

	ginkgo.It("modifies the resource", func() {
		// This runs after the create test
		err := modifyResource(resource)
		framework.ExpectNoError(err)
	})

	ginkgo.It("deletes the resource", func() {
		err := deleteResource(resource)
		framework.ExpectNoError(err)
	})
})
```

### Recipe: Table-Driven Tests

For testing the same scenario with multiple inputs:

```go
ginkgo.DescribeTable("validates CRD",
	func(name string, spec MySpec, expectError bool) {
		obj := &MyCRD{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}
		_, err := client.Create(context.TODO(), obj, metav1.CreateOptions{})
		if expectError {
			gomega.Expect(err).To(gomega.HaveOccurred())
		} else {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	},
	ginkgo.Entry("valid config", "valid", MySpec{Field: "good"}, false),
	ginkgo.Entry("invalid config", "invalid", MySpec{Field: ""}, true),
	ginkgo.Entry("edge case", "edge", MySpec{Field: "x"}, false),
)
```

### Recipe: Using Diagnostics

For collecting debug data during test execution:

```go
var _ = ginkgo.Describe("My Feature", feature.MyFeature, func() {
	f := wrappedTestFramework("myfeature")

	ginkgo.It("does something with diagnostics", func() {
		d := diagnostics.New(f)

		// Start collecting conntrack entries on all nodes
		d.ConntrackDumpingDaemonSet()

		// Start collecting OVS flows on breth0
		d.OVSFlowsDumpingDaemonSet("breth0")

		// Run the actual test
		// ...

		// Diagnostics are collected as pod logs which are preserved
		// in Kind log export on failure
	})
})
```

---

## Troubleshooting

### "My test is not running in CI"

1. **Check the label**: Verify your `Describe` block has the correct feature label
   (`feature.MyFeature`).
2. **Check e2e-cp.sh**: Verify the label is not being excluded by a `skip_label`
   block. Search for `Feature:MyFeature` in the script.
3. **Check the CI matrix**: Verify a lane exists with the right `target` and that
   the env var mapping sets `ENABLE_MY_FEATURE=true`.
4. **Check the dispatch**: Verify the "Run Tests" step dispatches your target
   to the correct `make` command.

### "My test runs locally but fails in CI"

Common causes:

- **Different IP family**: CI runs IPv4, IPv6, and dualstack. Your test may assume
  IPv4. Use `framework.TestContext.ClusterIsIPv6()` or check
  `PLATFORM_IPV4_SUPPORT` / `PLATFORM_IPV6_SUPPORT`.
- **Different gateway mode**: CI runs both `shared` and `local`. Use
  `isLocalGWModeEnabled()` to check.
- **Parallel execution**: CI sets `PARALLEL=true` by default. Your test may have
  resource conflicts with other tests. Add `ginkgo.Serial` if needed.
- **Different node count**: CI uses 1 master + 2 workers (non-HA) or 3 masters
  (HA). Don't assume a specific number of nodes.
- **Disk space**: CI runners have limited disk. Large images or logs can cause
  failures. The `free-disk-space` action mitigates this.

### "My test is flaky"

1. Add `FLAKE_ATTEMPTS` retries if the flakiness is inherent (e.g., timing):
   The `e2e-cp.sh` script already sets `--flake-attempts 2`.
2. Add `gomega.Eventually` with retries instead of `gomega.Expect` for
   asynchronous operations:

```go
gomega.Eventually(func() string {
	output, _ := exec(cmd)
	return output
}, 30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("expected"))
```

3. File an issue with the `kind/ci-flake` label and document the failure pattern.

### "My Kind cluster fails to start"

- Check disk space: `df -h /mnt/docker-data` (CI) or `df -h` (local).
- Check Docker is running: `docker info`.
- Check for port conflicts: `ss -tlnp | grep 6443`.
- Delete existing cluster first: `kind delete cluster --name ovn`.
- Check `jinjanate` is installed: `pip install jinjanate`.

### "My test can't find the OVN image"

- Locally: Build the image first with `cd go-controller && make && cd .. && make -C dist/images fedora-image`.
- Or set `OVN_IMAGE=<your-image>` to use a pre-built image.
- In CI: The image is built by `build-pr` or `build-pr-ubuntu` and loaded
  automatically. Check the "Verify selected image build" step.

---

## Checklist Reference

### New Feature Test Checklist

- [ ] Feature label added to `test/e2e/feature/features.go`
- [ ] Test file created in `test/e2e/` with `wrappedTestFramework`
- [ ] Tests use `feature.MyFeature` label on `Describe` block
- [ ] Custom images registered in `test/e2e/images/images.go` (if needed)
- [ ] Helm value added to `values-single-node-zone.yaml` (if feature flag needed)
- [ ] Helm template wires value to container env var (if feature flag needed)
- [ ] `contrib/kind-common.sh` has env var default and validation
- [ ] `contrib/kind-helm.sh` has CLI flag and `--set` mapping
- [ ] `test/scripts/e2e-cp.sh` has `skip_label` / `require_label` block
- [ ] `.github/workflows/test.yml` has matrix entry
- [ ] `.github/workflows/test.yml` has env var mapping
- [ ] `.github/workflows/test.yml` has dispatch in "Run Tests" step
- [ ] Tests pass locally with the feature enabled
- [ ] Tests are properly skipped locally with the feature disabled

### Existing Feature Coverage Checklist

- [ ] New tests use the existing feature label
- [ ] New tests are inside the existing `Describe` or use the same label
- [ ] Configuration-specific tests have appropriate `ginkgo.Skip` guards
- [ ] Tests pass in all CI lanes that enable the feature

### CI Lane Addition Checklist

- [ ] Matrix entry has all required keys (target, ha, gateway-mode, ipfamily, etc.)
- [ ] Env var mappings in the `env:` section cover all custom keys
- [ ] "Run Tests" step has dispatch for the target name
- [ ] Timeout is appropriate (130 min default, 190 for complex features)
- [ ] The new lane provides meaningful coverage not covered by existing lanes
- [ ] Job name is unique (auto-generated from matrix values)

### Feature Removal Checklist

- [ ] CI matrix entries removed
- [ ] CI env var mapping removed
- [ ] CI dispatch entry removed (if unique target)
- [ ] `e2e-cp.sh` skip/require blocks removed
- [ ] Test file deleted
- [ ] Feature label removed from `features.go`
- [ ] `kind-helm.sh` CLI flag and `--set` removed
- [ ] `kind-common.sh` default and validation removed
- [ ] Helm value removed from `values-*.yaml`
- [ ] Helm template env var removed
- [ ] No orphaned references in other test files (grep for the feature name)
