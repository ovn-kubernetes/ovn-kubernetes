#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Alpine FRR flavor: run inside the exact recorded image.

set -euo pipefail

source /gdb-stacktrace.sh

die() {
  echo "error: $*" >&2
  exit 1
}

[ "$#" -ge 4 ] || die "expected one or more coredumps"
[ $(($# % 4)) -eq 0 ] || die "incomplete coredump arguments"
: "${DEBUG_IMAGE:?DEBUG_IMAGE is required}"

runtime_root=/tmp/frr-runtime-root
mkdir -p "${runtime_root}/usr"
echo "Snapshotting the exact FRR runtime libraries and debug files..."
cp -a /lib "${runtime_root}/"
cp -a /usr/lib "${runtime_root}/usr/"

echo "Installing GDB..."
apk add --no-cache gdb

status=0
while [ "$#" -gt 0 ]; do
  collected_binary=$1
  core=$2
  output=$3
  runtime_image=$4
  shift 4
  executable=$(basename "$collected_binary")
  image_binary="${runtime_root}/usr/lib/frr/${executable}"
  debug_file="${runtime_root}/usr/lib/debug/usr/lib/frr/${executable}.debug"

  echo "Extracting $(basename "$core")..."
  {
    echo "Core dump: $(basename "$core")"
    echo "Executable: ${executable}"
    echo "Runtime image: ${runtime_image}"
    echo "Debug image: ${DEBUG_IMAGE}"
    echo
    if [ ! -x "$image_binary" ]; then
      echo "The matching executable is missing from the recorded FRR image."
      status=1
    elif ! cmp -s "$collected_binary" "$image_binary"; then
      echo "The executable in the recorded FRR image does not match the collected executable."
      status=1
    elif [ ! -s "$debug_file" ]; then
      echo "The recorded FRR image does not contain ${debug_file}."
      status=1
    elif ! gdb_stacktrace "$image_binary" "$core" \
      -iex "set sysroot ${runtime_root}" \
      -iex "set debug-file-directory ${runtime_root}/usr/lib/debug"; then
      status=1
    fi
  } > "$output" 2>&1
  chmod a+r "$output"
done

exit "$status"
