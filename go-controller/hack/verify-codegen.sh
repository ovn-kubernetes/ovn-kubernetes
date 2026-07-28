#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -o errexit # Nonzero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
ROOT=$(readlink --canonicalize "$HERE/..")
GITROOT=$(git -C "$ROOT" rev-parse --show-toplevel)

cd "$ROOT"

echo "Regenerating generated code (codegen + mocks) to verify it matches the committed tree"
./hack/update-codegen.sh

# Generated Go lives under go-controller/pkg; CRD manifests are copied to
# helm/ovn-kubernetes/crds. _output/ is gitignored so it does not affect this check.
CHANGES=$(git -C "$GITROOT" status --porcelain -- go-controller/pkg helm/ovn-kubernetes/crds)
if [ -n "$CHANGES" ]; then
    echo "ERROR: generated code is out of date."
    echo "Run 'make codegen' in go-controller/ and commit the result."
    echo "Offending files:"
    echo "$CHANGES"
    git -C "$GITROOT" --no-pager diff -- go-controller/pkg helm/ovn-kubernetes/crds
    exit 1
fi

echo "Generated code is up to date."
