#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Fedora OVN flavor: run inside the recorded Fedora base image.

set -euo pipefail

source /gdb-stacktrace.sh

die() {
  echo "error: $*" >&2
  exit 1
}

extract_ovn_version() {
  strings "$1" \
    | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"ovn".*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | sed -n '1p'
}

[ "$#" -ge 6 ] || die "expected Fedora metadata and one or more coredumps"
[ $((($# - 2) % 4)) -eq 0 ] || die "incomplete coredump arguments"
: "${DEBUG_IMAGE:?DEBUG_IMAGE is required}"

rpm_arch=$1
recorded_fedora_base_image=$2
shift 2

declare -a coredump_args=("$@")

echo "Installing Fedora debugging tools..."
dnf install -y --setopt=install_weak_deps=False binutils gdb koji

ovn_build=""
for ((i = 0; i < ${#coredump_args[@]}; i += 4)); do
  binary=${coredump_args[$i]}
  version=$(extract_ovn_version "$binary")
  [ -n "$version" ] || die "could not determine the OVN RPM version from ${binary}"
  current_build="ovn-${version}"
  if [ -n "$ovn_build" ] && [ "$ovn_build" != "$current_build" ]; then
    die "multiple OVN builds found: ${ovn_build} and ${current_build}"
  fi
  ovn_build=$current_build
done

echo "Downloading runtime and debuginfo packages for ${ovn_build}..."
rpm_dir=/tmp/ovn-debug-rpms
mkdir -p "$rpm_dir"
cd "$rpm_dir"
koji download-build "$ovn_build" --arch="$rpm_arch"
koji download-build "$ovn_build" --debuginfo --arch="$rpm_arch"

declare -a rpms=()
while IFS= read -r -d '' rpm; do
  rpms+=("$rpm")
done < <(find "$rpm_dir" -maxdepth 1 -type f -name '*.rpm' -print0)
[ "${#rpms[@]}" -gt 0 ] || die "no RPMs downloaded for ${ovn_build}"
dnf install -y "${rpms[@]}"

status=0
for ((i = 0; i < ${#coredump_args[@]}; i += 4)); do
  binary=${coredump_args[$i]}
  core=${coredump_args[$((i + 1))]}
  output=${coredump_args[$((i + 2))]}
  runtime_image=${coredump_args[$((i + 3))]}

  echo "Extracting $(basename "$core")..."
  {
    echo "Core dump: $(basename "$core")"
    echo "Executable: $(basename "$binary")"
    echo "Runtime image: ${runtime_image}"
    echo "Recorded Fedora base image: ${recorded_fedora_base_image}"
    echo "Debug image: ${DEBUG_IMAGE}"
    echo "OVN build: ${ovn_build}"
    echo
    if ! gdb_stacktrace "$binary" "$core"; then
      status=1
    fi
  } > "$output" 2>&1
  chmod a+r "$output"
done

exit "$status"
