#!/usr/bin/env bash
set -o errexit # Nozero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
ROOT=$(readlink --canonicalize "$HERE/..")
VERIFY_TMP_DIR=$(mktemp -d)
trap 'rm -rf "$VERIFY_TMP_DIR"' EXIT

echo "Checking that go.mod and go.sum are tidy"
cp "$ROOT/go.mod" "$VERIFY_TMP_DIR/go.mod"
cp "$ROOT/go.sum" "$VERIFY_TMP_DIR/go.sum"

cd "$ROOT/"
go mod tidy -modfile="$VERIFY_TMP_DIR/go.mod"
cd -

if ! cmp -s "$ROOT/go.mod" "$VERIFY_TMP_DIR/go.mod" || ! cmp -s "$ROOT/go.sum" "$VERIFY_TMP_DIR/go.sum"; then
    echo "ERROR: detected go.mod or go.sum inconsistency after 'go mod tidy':"
    diff -u "$ROOT/go.mod" "$VERIFY_TMP_DIR/go.mod" || true
    diff -u "$ROOT/go.sum" "$VERIFY_TMP_DIR/go.sum" || true
    exit 1
fi
