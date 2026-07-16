#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

gdb_stacktrace() {
  local binary=$1
  local core=$2
  shift 2

  if gdb --batch --quiet \
    -iex 'set debuginfod enabled off' \
    "$@" \
    -ex 'set pagination off' \
    -ex 'info threads' \
    -ex 'thread apply all bt full' \
    "$binary" "$core"; then
    return 0
  fi

  echo
  echo "GDB failed to extract this stack trace."
  return 1
}
