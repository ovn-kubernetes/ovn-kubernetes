#!/usr/bin/env bash
# Export kind cluster logs and collect coredump binaries
# Usage: ./export-kind-logs.sh [logs_dir]
# Default logs_dir: /tmp/kind/logs

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/kind-common.sh"

# Don't create cluster or delete kubeconfig - we're just exporting logs
KIND_CREATE=false
set_common_default_params

export_logs "$@"
