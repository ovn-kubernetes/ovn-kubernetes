#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Extract and print stack traces from coredump artifacts downloaded from CI lanes.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly SCRIPT_DIR
source "${SCRIPT_DIR}/common.sh"
readonly PRINTER="${SCRIPT_DIR}/print-stacktraces.sh"

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "Usage: $(basename "$0") COREDUMP_ARTIFACTS_DIRECTORY [OUTPUT_DIRECTORY]" >&2
  exit 2
fi

artifact_root=$1
output_root=${2:-"$(pwd)/stacktraces"}

[ -d "$artifact_root" ] || {
  echo "No coredump artifacts found in ${artifact_root}"
  exit 0
}

mkdir -p "$output_root"
artifact_root=$(cd -- "$artifact_root" >/dev/null 2>&1 && pwd -P)
output_root=$(cd -- "$output_root" >/dev/null 2>&1 && pwd -P)

artifact_count=0
failed_artifacts=0

process_artifact() {
  local artifact_dir=$1
  local artifact_name

  if ! has_coredumps "$artifact_dir"; then
    return
  fi

  artifact_count=$((artifact_count + 1))
  artifact_name=$(basename "$artifact_dir")
  echo "Processing coredumps from ${artifact_name}"
  if ! TRACE_SOURCE="$artifact_name" \
    "$PRINTER" "$artifact_dir" "${output_root}/${artifact_name}"; then
    failed_artifacts=$((failed_artifacts + 1))
  fi
}

while IFS= read -r -d '' artifact_dir; do
  process_artifact "$artifact_dir"
done < <(find "$artifact_root" -maxdepth 1 -type d -print0)

if [ "$artifact_count" -eq 0 ]; then
  echo "No coredump artifacts found in ${artifact_root}"
elif [ "$failed_artifacts" -ne 0 ]; then
  echo "error: failed to process ${failed_artifacts} coredump artifact(s)" >&2
  exit 1
fi
