#!/bin/bash
set -euo pipefail

# Default values
GOLANGCI_LINT_VERSION=${GOLANGCI_LINT_VERSION:-v1.60.3}

# Change to e2e directory
cd "$(dirname "$0")/../e2e"

# Build the command
LINT_CMD="go run github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} run --verbose --print-resources-usage --timeout=15m0s $@"

# Run the linter
echo "Running golangci-lint in $(pwd)..."
eval "$LINT_CMD"
