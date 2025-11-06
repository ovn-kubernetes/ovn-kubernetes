#!/usr/bin/env bash
set -euo pipefail

# Default values
GOLANGCI_LINT_VERSION=${GOLANGCI_LINT_VERSION:-v1.64.8}

# Change to e2e directory
cd "$(dirname "$0")/../e2e"

# Instead of building a single string with $@, collect the sub-command and flags in an array:
LINT_ARGS=(run \
  --verbose \
  --print-resources-usage \
  --timeout=15m0s \
  --path-prefix=e2e/ \
  "$@"
)

echo "Running golangci-lint in $(pwd)..."
go run "github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
  "${LINT_ARGS[@]}"
