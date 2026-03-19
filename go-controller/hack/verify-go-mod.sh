#!/usr/bin/env bash
set -o errexit # Nozero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
ROOT=$(readlink --canonicalize "$HERE/..")
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Checking that go.mod and go.sum are tidy"
cp "$ROOT/go.mod" "$TMPDIR/go.mod"
cp "$ROOT/go.sum" "$TMPDIR/go.sum"

cd "$ROOT/"
go mod tidy
cd -

if ! cmp -s "$ROOT/go.mod" "$TMPDIR/go.mod" || ! cmp -s "$ROOT/go.sum" "$TMPDIR/go.sum"; then
    echo "ERROR: detected go.mod or go.sum inconsistency after 'go mod tidy':"
    diff -u "$TMPDIR/go.mod" "$ROOT/go.mod" || true
    diff -u "$TMPDIR/go.sum" "$ROOT/go.sum" || true
    exit 1
fi
