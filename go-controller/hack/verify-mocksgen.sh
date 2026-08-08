#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -o errexit # Nonzero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
ROOT=$(cd "$HERE/.." && pwd -P)
GITROOT=$(git -C "$ROOT" rev-parse --show-toplevel)

cd "$ROOT"

echo "Regenerating mockery mocks to verify they match the committed tree"
make mocksgen

# _output/ is gitignored so the downloaded mockery binary does not affect this check.
CHANGES=$(git -C "$GITROOT" status --porcelain -- go-controller/pkg)
if [ -n "$CHANGES" ]; then
    echo "ERROR: generated mocks are out of date."
    echo "Run 'make mocksgen' in go-controller/ and commit the result."
    echo "Offending files:"
    echo "$CHANGES"
    git -C "$GITROOT" --no-pager diff -- go-controller/pkg
    exit 1
fi

echo "Generated mocks are up to date."
