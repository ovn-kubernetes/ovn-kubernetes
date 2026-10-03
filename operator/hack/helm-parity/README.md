# helm-parity

Checks that the OVNK8s Operator renders the same manifests as the in-tree Helm chart
(`helm/ovn-kubernetes`) for the same installation. Both sides are rendered
fresh, and a throwaway kind API server applies Kubernetes defaulting and
canonicalization, so the comparison shows semantic differences rather than
YAML formatting differences.

This is a temporary check. It exists only while OVN-Kubernetes ships both the
Helm chart and the operator, to keep the two installs equivalent. Remove it
when helm charts are deprecated.

## Running

Install `go`, `helm`, `kind`, `kubectl` and `docker`, and start the Docker
daemon. From `operator/`:

```sh
make helm-parity                               # standard profile
HELM_PARITY_PROFILE=features make helm-parity  # features profile
```

Each run creates a kind cluster, deletes it on exit, and takes about a minute.

| Variable | Default | Purpose |
|----------|---------|---------|
| `HELM_PARITY_PROFILE` | `standard` | Profile under `profiles/` |
| `KIND_IMAGE` | `kindest/node:v1.37.0` | kind node image (API server version) |
| `CLUSTER_NAME` | `ovnk8s-helm-parity` | kind cluster name |
| `KEEP_TMP` | unset | Keep and print the directory of rendered manifests and diffs |
| `KEEP_CLUSTER` | unset | Keep the kind cluster and reuse it on the next run |
| `UPDATE_GOLDEN` | unset | Rewrite the profile's golden files instead of comparing |

Use `KEEP_CLUSTER=1` only to iterate quickly. Do the final run without it so
it starts from a clean cluster.

## Profiles

Each profile describes one installation twice: as an `OVNK8sOperator` CR and
as Helm values.

| Profile | Helm values | Covers |
|---------|-------------|--------|
| `standard` | `values-single-node-zone.yaml` unmodified | The chart's defaults: single-stack, shared gateway, its default-on features, identity webhook |
| `features` | `values-single-node-zone.yaml` + `helm-values.yaml` | Every feature gate in the API, dual-stack, local gateway, non-default subnets, no identity webhook |

```text
profiles/<profile>/
├── cr.yaml                     # operator input
├── helm-values.yaml            # optional Helm overrides
├── expected-object-delta.txt   # golden: objects only one side renders
└── expected.diff               # golden: allowed server-side differences
```

## How it works

1. Render the operator with `go run ./cmd render --cr <cr.yaml>`. This uses
   the same render path as the controller.
2. Render the chart with `helm template`, using `values-single-node-zone.yaml`
   plus the profile's `helm-values.yaml`. The `ovs-node` subchart is disabled
   because the operator does not deploy OVS.
3. Normalize both renders by dropping CRDs, the chart's self-signed
   `ovnkube-webhook-cert` Secret, and webhook `caBundle` fields. These are
   random per render or checked elsewhere.
4. Compare the object sets with `expected-object-delta.txt`.
5. Apply the CRDs and the Helm render server-side to kind, then run
   `kubectl diff --server-side` with the operator render, and compare the
   result with `expected.diff`.

## Scope

Not covered:

- CRDs. `make verify-crds` checks that the bundled copies match the chart.
- Values the controller adds at runtime: the webhook CA bundle, deployer
  labels, and component patches.
- The DPU and DPU-host modes, and the `ovn-ipsec` subchart.

## Reading the results

`object-delta.txt` lists each object as `helm-only:` or `operator-only:` with
its `Kind namespace/name`.

In the diff, each section compares `LIVE` (the applied Helm render) with
`MERGED` (the operator render): `+` lines are operator-only, `-` lines are
Helm-only. Diffs are compared as plain text, so hunk headers and volatile
server fields are stripped.

On failure, rerun with `KEEP_TMP=1`. The kept directory holds
`operator-raw.yaml`, `helm-raw.yaml`, the normalized `operator.yaml` and
`helm.yaml`, the object lists, and `actual.diff`.

## Goldens

The golden files record reviewed, allowed disagreements; they are not
reference manifests. Both `expected.diff` files are empty, so any diff line is
drift. Both `expected-object-delta.txt` files list the same three Helm-only
objects:

- `Namespace /ovn-kubernetes` and `Namespace /ovn-host-network`: the
  operator's install manifests (`config/default/ovnkube_namespace.yaml`)
  create them, not its templates.
- `RoleBinding ovn-kubernetes/ovnkube-node-ic-configmaps-update`: it binds a
  Role the chart no longer defines, so the operator omits it.

If a chart or template change makes a new difference intentional:

```sh
UPDATE_GOLDEN=1 HELM_PARITY_PROFILE=<profile> make helm-parity
```

Review and justify every changed line, then rerun without `UPDATE_GOLDEN`.
A golden update only writes files and does not verify anything. Never use a
golden to hide a template bug; fix the template instead.

## Adding a profile

Create `profiles/<name>/` with a `cr.yaml`, a `helm-values.yaml` describing
the same installation, and goldens generated with `UPDATE_GOLDEN=1`. Do not
assume another profile covers a mode-specific chart change.
