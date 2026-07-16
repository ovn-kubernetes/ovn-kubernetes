#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Extract collected coredumps and print their stack traces to the CI job log.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly SCRIPT_DIR
source "${SCRIPT_DIR}/coredump/common.sh"
readonly EXTRACTOR="${SCRIPT_DIR}/extract-coredump-stacktraces.sh"

if [ "$#" -ne 1 ]; then
  echo "Usage: $(basename "$0") KIND_LOGS_DIRECTORY" >&2
  exit 2
fi

logs_dir=$1
coredump_dir="${logs_dir}/coredumps"

if [ ! -d "$coredump_dir" ] \
  || ! find "$coredump_dir" -maxdepth 1 -type f -name 'core.*' -print -quit \
    | grep -q .; then
  echo "No coredumps found in ${logs_dir}"
  exit 0
fi

archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
tar -C "$(dirname "$logs_dir")" -cf "$archive" \
  "$(basename "$logs_dir")/coredumps"

stacktrace_dir="${logs_dir}/stacktraces"
extract_status=0
"$EXTRACTOR" "$archive" "$stacktrace_dir" || extract_status=$?

trace_count=0
while IFS= read -r -d '' trace; do
  trace_count=$((trace_count + 1))
  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    echo "::group::Coredump stack trace: $(basename "$trace")"
  else
    echo "===== Coredump stack trace: $(basename "$trace") ====="
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
