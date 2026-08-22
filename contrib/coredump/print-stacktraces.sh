#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Extract collected coredumps and print their stack traces to the CI job log.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly SCRIPT_DIR
source "${SCRIPT_DIR}/common.sh"
readonly EXTRACTOR="${SCRIPT_DIR}/extract-stacktraces.sh"

escape_github_command_data() {
  local value=$1

  value=${value//%/%25}
  value=${value//$'\r'/%0D}
  value=${value//$'\n'/%0A}
  printf '%s' "$value"
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "Usage: $(basename "$0") KIND_LOGS_OR_COREDUMP_DIRECTORY [OUTPUT_DIRECTORY]" >&2
  exit 2
fi

input_dir=$1
default_stacktrace_dir="${input_dir}/stacktraces"

stacktrace_dir=${2:-$default_stacktrace_dir}
mkdir -p "$stacktrace_dir"
extract_status=0
"$EXTRACTOR" "$input_dir" "$stacktrace_dir" || extract_status=$?

trace_count=0
while IFS= read -r -d '' trace; do
  trace_count=$((trace_count + 1))
  trace_name=$(basename "$trace")
  if [ -n "${TRACE_SOURCE:-}" ]; then
    trace_name="${TRACE_SOURCE}: ${trace_name}"
  fi
  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    trace_name=$(escape_github_command_data "$trace_name")
    printf '::group::Coredump stack trace: %s\n' "$trace_name"
  else
    printf '===== Coredump stack trace: %s =====\n' "$trace_name"
  fi

  # Prevent coredump data from being interpreted as GitHub workflow commands.
  sed 's/^::/ ::/' "$trace"
  echo

  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    echo "::endgroup::"
  fi
done < <(find "$stacktrace_dir" -maxdepth 1 -type f \
  -name "$STACKTRACE_GLOB" -print0)

if [ "$trace_count" -eq 0 ]; then
  echo "No supported coredump stack traces were produced"
fi

exit "$extract_status"
