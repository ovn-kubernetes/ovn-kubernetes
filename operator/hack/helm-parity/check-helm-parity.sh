#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Compare the operator's rendered manifests against the in-tree Helm chart,
# using a throwaway kind cluster's API server as the normalizer (defaulting,
# field canonicalization). Each profile pairs an OVNK8sOperator CR with the
# Helm values that describe the same installation.
#
# Checks:
#   1. Object sets match, modulo the profile's expected-object-delta.txt.
#   2. `kubectl diff --server-side` of the operator render against the applied
#      Helm render matches the profile's expected.diff.
#
# CRDs are excluded; `make verify-crds` checks them against the chart.
#
# Usage:
#   hack/helm-parity/check-helm-parity.sh
#   HELM_PARITY_PROFILE=features ...  # select another profile
#   UPDATE_GOLDEN=1 ...               # rewrite the profile's golden files
#   KEEP_TMP=1 ...                    # retain rendered manifests and diffs
#   KEEP_CLUSTER=1 ...                # retain/reuse the kind cluster

set -euo pipefail

KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.37.0}"
PROFILE="${HELM_PARITY_PROFILE:-standard}"
CLUSTER_NAME="${CLUSTER_NAME:-ovnk8s-helm-parity}"
UPDATE_GOLDEN="${UPDATE_GOLDEN:-}"
KEEP_TMP="${KEEP_TMP:-}"
KEEP_CLUSTER="${KEEP_CLUSTER:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPERATOR_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
CHART_DIR="$(cd "$OPERATOR_DIR/../helm/ovn-kubernetes" && pwd)"
PROFILE_DIR="$SCRIPT_DIR/profiles/$PROFILE"
EXPECTED_DIFF="$PROFILE_DIR/expected.diff"
EXPECTED_OBJECT_DELTA="$PROFILE_DIR/expected-object-delta.txt"

if [ ! -f "$PROFILE_DIR/cr.yaml" ]; then
  echo "ERROR: unknown profile '$PROFILE' (no $PROFILE_DIR/cr.yaml)" >&2
  exit 1
fi

OUT="$(mktemp -d)"
KUBECONFIG_FILE="$OUT/kubeconfig"
CREATED_CLUSTER=""

cleanup() {
  if [ -n "$CREATED_CLUSTER" ] && [ -z "$KEEP_CLUSTER" ]; then
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
  fi
  if [ -n "$KEEP_TMP" ]; then
    echo "==> Kept parity artifacts: $OUT" >&2
  else
    rm -rf "$OUT"
  fi
}
trap cleanup EXIT

for tool in go helm kind kubectl docker; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: required command not found: $tool" >&2
    exit 1
  fi
done

if ! docker info >/dev/null 2>&1; then
  echo "ERROR: Docker daemon is not available" >&2
  exit 1
fi

cd "$OPERATOR_DIR"

echo "==> Rendering operator manifests (profile: $PROFILE)"
go run ./cmd render --cr "$PROFILE_DIR/cr.yaml" --templates ./templates > "$OUT/operator-raw.yaml"

echo "==> Rendering the Helm chart"
HELM_PROFILE_VALUES=()
if [ -f "$PROFILE_DIR/helm-values.yaml" ]; then
  HELM_PROFILE_VALUES=(-f "$PROFILE_DIR/helm-values.yaml")
fi
# OVS is external to the operator, so the chart's ovs-node subchart is left out.
helm template ovn-kubernetes "$CHART_DIR" \
  -f "$CHART_DIR/values-single-node-zone.yaml" \
  ${HELM_PROFILE_VALUES[@]+"${HELM_PROFILE_VALUES[@]}"} \
  --set skipCallToK8s=true \
  --set tags.ovs-node=false \
  > "$OUT/helm-raw.yaml"

echo "==> Normalizing (drop CRDs, webhook cert Secret, caBundle)"
go run ./hack/helm-parity/normalize normalize < "$OUT/operator-raw.yaml" > "$OUT/operator.yaml"
go run ./hack/helm-parity/normalize normalize < "$OUT/helm-raw.yaml" > "$OUT/helm.yaml"

echo "==> Comparing object sets"
go run ./hack/helm-parity/normalize list < "$OUT/operator-raw.yaml" > "$OUT/operator-objects.txt"
go run ./hack/helm-parity/normalize list < "$OUT/helm-raw.yaml" > "$OUT/helm-objects.txt"
{
  comm -23 "$OUT/helm-objects.txt" "$OUT/operator-objects.txt" | sed 's/^/helm-only: /'
  comm -13 "$OUT/helm-objects.txt" "$OUT/operator-objects.txt" | sed 's/^/operator-only: /'
} > "$OUT/object-delta.txt"

if [ -n "$UPDATE_GOLDEN" ]; then
  cp "$OUT/object-delta.txt" "$EXPECTED_OBJECT_DELTA"
  echo "    updated $(basename "$EXPECTED_OBJECT_DELTA")"
elif ! diff -u "$EXPECTED_OBJECT_DELTA" "$OUT/object-delta.txt"; then
  echo "ERROR: object-set delta differs from $(basename "$EXPECTED_OBJECT_DELTA")" >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME"; then
  if [ -n "$KEEP_CLUSTER" ]; then
    echo "==> Reusing kind cluster $CLUSTER_NAME"
    kind get kubeconfig --name "$CLUSTER_NAME" > "$KUBECONFIG_FILE"
  else
    echo "ERROR: kind cluster '$CLUSTER_NAME' already exists" >&2
    echo "       Set KEEP_CLUSTER=1 to reuse it or choose another CLUSTER_NAME." >&2
    exit 1
  fi
else
  echo "==> Creating kind cluster $CLUSTER_NAME ($KIND_IMAGE)"
  CREATED_CLUSTER="true"
  kind create cluster --name "$CLUSTER_NAME" --image "$KIND_IMAGE" \
    --kubeconfig "$KUBECONFIG_FILE" --wait 120s
fi
export KUBECONFIG="$KUBECONFIG_FILE"

echo "==> Applying component CRDs"
kubectl apply --server-side --field-manager=compare -f templates/global/crds/ >/dev/null
kubectl wait --for=condition=Established crd --all --timeout=60s >/dev/null

echo "==> Applying Helm manifests (server-side)"
kubectl apply --server-side --field-manager=compare -f "$OUT/helm.yaml" >/dev/null

echo "==> Diffing operator manifests against the cluster"
set +e
kubectl diff --server-side --field-manager=compare -f "$OUT/operator.yaml" > "$OUT/actual-raw.diff"
rc=$?
set -e
if [ "$rc" -gt 1 ]; then
  echo "ERROR: kubectl diff failed" >&2
  cat "$OUT/actual-raw.diff" >&2
  exit "$rc"
fi

# Strip tmpdir prefixes and volatile server-populated content so the output is
# stable across runs. Removing lines from unified-diff hunks breaks their line
# counts, but the golden is compared as a plain string, never re-applied.
sed -E \
  -e 's|[^ ]*/((LIVE\|MERGED)-[0-9]+)|\1|g' \
  -e 's/(LIVE|MERGED)-[0-9]+/\1/g' \
  -e $'s/^(--- |\\+\\+\\+ |diff -u -N )(.*)\t.*/\\1\\2/' \
  -e '/^[-+ ] *(uid|resourceVersion|creationTimestamp|generation|deprecated\.daemonset\.template\.generation):/d' \
  -e '/^@@ /d' \
  "$OUT/actual-raw.diff" > "$OUT/actual.diff"

if [ -n "$UPDATE_GOLDEN" ]; then
  cp "$OUT/actual.diff" "$EXPECTED_DIFF"
  echo "    updated $(basename "$EXPECTED_DIFF")"
  echo "==> Golden files updated. Review every changed line, then rerun normally."
  exit 0
fi

if ! diff -u "$EXPECTED_DIFF" "$OUT/actual.diff"; then
  echo "ERROR: rendered manifests drifted from the Helm chart (or a documented" >&2
  echo "divergence changed). If intentional, rerun with UPDATE_GOLDEN=1 and" >&2
  echo "commit the updated $(basename "$EXPECTED_DIFF")." >&2
  exit 1
fi

echo "==> OK: operator render matches the Helm render for $PROFILE (modulo documented divergences)"
