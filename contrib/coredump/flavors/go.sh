#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Go flavor: run inside a Fedora image with a recent Delve package.

set -euo pipefail

die() {
  echo "error: $*" >&2
  exit 1
}

[ "$#" -ge 4 ] || die "expected one or more coredumps"
[ $(($# % 4)) -eq 0 ] || die "incomplete coredump arguments"
: "${DEBUG_IMAGE:?DEBUG_IMAGE is required}"

echo "Installing the Delve debugger..."
dnf install -y --setopt=install_weak_deps=False delve

status=0
while [ "$#" -gt 0 ]; do
  binary=$1
  core=$2
  output=$3
  runtime_image=$4
  shift 4

  echo "Extracting $(basename "$core")..."
  {
    echo "Core dump: $(basename "$core")"
    echo "Executable: $(basename "$binary")"
    echo "Runtime image: ${runtime_image}"
    echo "Debug image: ${DEBUG_IMAGE}"
    echo
    if ! printf '%s\n' \
      'goroutine' \
      'stack 100 -full' \
      'goroutines -group userloc -t 20' \
      'exit' \
      | dlv --allow-non-terminal-interactive=true core "$binary" "$core" \
      | sed $'s/\033\\[[0-9;]*[mK]//g'; then
      echo
      echo "Delve failed to extract this stack trace."
      status=1
    fi
  } > "$output" 2>&1
  chmod a+r "$output"
done

exit "$status"
