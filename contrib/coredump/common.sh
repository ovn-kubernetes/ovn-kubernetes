#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Shared coredump and stack trace naming conventions.

readonly COREDUMP_GLOB='core.*'
readonly STACKTRACE_SUFFIX='.stacktrace.txt'
readonly STACKTRACE_GLOB="*${STACKTRACE_SUFFIX}"

has_coredumps() {
  local directory=$1

  [ -d "$directory" ] \
    && find "$directory" -maxdepth 1 -type f -name "$COREDUMP_GLOB" \
      -print -quit | grep -q .
}

stacktrace_path() {
  local output_dir=$1
  local core=$2

  printf '%s/%s%s\n' "$output_dir" "$(basename "$core")" "$STACKTRACE_SUFFIX"
}
