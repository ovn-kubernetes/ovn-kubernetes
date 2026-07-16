#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Extract stack traces from coredumps in a tar archive of KIND logs.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly SCRIPT_DIR
readonly FEDORA_OVN_DEBUGGER="${SCRIPT_DIR}/coredump/fedora-ovn-stacktrace.sh"
readonly OCI_BIN="${OCI_BIN:-docker}"

SUPPORTED_CORES=0
FAILED_CORES=0
COREDUMP_DIRS=0

usage() {
  cat <<EOF
Usage: $(basename "$0") KIND_LOGS_TARBALL [OUTPUT_DIRECTORY]

Extract stack traces from coredumps collected with export-kind-logs.sh.
Stack traces are written to OUTPUT_DIRECTORY (default: ./stacktraces).

Currently supported:
  - ovn-northd and ovn-controller from the Fedora ovn-daemonset image

Set OCI_BIN to use a container runtime other than docker.
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

fedora_release_for_binary() {
  strings "$1" \
    | sed -n 's/.*"type":"rpm".*"name":"ovn".*"osCpe":"cpe:\/o:fedoraproject:fedora:\([0-9][0-9]*\)".*/\1/p' \
    | sed -n '1p'
}

image_for_executable() {
  local executable=$1
  local image_info=$2

  awk -v executable="$executable" '
    index($0, executable ": ") == 1 {
      sub(/^[^:]+: /, "")
      print
      exit
    }
  ' "$image_info"
}

platform_for_binary() {
  local description
  description=$(file -b "$1")

  case $description in
    *x86-64*) echo "linux/amd64 x86_64" ;;
    *aarch64* | *ARM64*) echo "linux/arm64 aarch64" ;;
    *) return 1 ;;
  esac
}

process_coredump_dir() {
  local coredump_dir=$1
  local image_info="${coredump_dir}/binaries/image-info.txt"
  local fedora_image_file="${coredump_dir}/binaries/fedora-base-image.txt"
  local fedora_image=""
  local fedora_release=""
  local platform=""
  local rpm_arch=""
  local supported_in_dir=0
  local -a debugger_args=()
  local -a trace_paths=()

  while IFS= read -r -d '' core; do
    local filename remainder executable image_ref binary platform_info
    local current_fedora_release current_platform current_rpm_arch trace_path

    filename=$(basename "$core")
    remainder=${filename#core.}
    remainder=${remainder#*.}
    executable=${remainder%%.*}

    case $executable in
      ovn-northd | ovn-controller) ;;
      *)
        echo "Skipping unsupported coredump: ${filename}"
        continue
        ;;
    esac

    if [ ! -f "$image_info" ]; then
      echo "error: ${filename}: missing binaries/image-info.txt" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi

    image_ref=$(image_for_executable "$executable" "$image_info")
    if [ -z "$image_ref" ]; then
      echo "error: ${filename}: no image recorded for ${executable}" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi
    binary="${coredump_dir}/binaries/${executable}"
    if [ ! -s "$binary" ]; then
      echo "error: ${filename}: collected binary ${executable} is missing" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi
    current_fedora_release=$(fedora_release_for_binary "$binary")
    if [ -z "$current_fedora_release" ]; then
      echo "Skipping coredump from unsupported image ${image_ref}: ${filename}"
      continue
    fi
    if [ -n "$fedora_release" ] && [ "$fedora_release" != "$current_fedora_release" ]; then
      echo "error: ${filename}: mixed Fedora releases in one coredump directory" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi
    fedora_release=$current_fedora_release
    if [ ! -s "$fedora_image_file" ]; then
      echo "error: ${filename}: Fedora base image metadata is missing" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi

    if ! platform_info=$(platform_for_binary "$binary"); then
      echo "error: ${filename}: unsupported binary architecture" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi
    current_platform=${platform_info%% *}
    current_rpm_arch=${platform_info#* }
    if [ -n "$platform" ] && [ "$platform" != "$current_platform" ]; then
      echo "error: ${filename}: mixed architectures in one coredump directory" >&2
      FAILED_CORES=$((FAILED_CORES + 1))
      continue
    fi
    platform=$current_platform
    rpm_arch=$current_rpm_arch

    if [ -z "$fedora_image" ]; then
      fedora_image=$(tr -d '\r\n' < "$fedora_image_file")
    fi

    trace_path="${output_dir}/${filename}.stacktrace.txt"
    : > "$trace_path"
    trace_paths+=("$trace_path")
    debugger_args+=(
      "/artifacts/binaries/${executable}"
      "/artifacts/${filename}"
      "/output/$(basename "$trace_path")"
      "$image_ref"
    )
    supported_in_dir=$((supported_in_dir + 1))
  done < <(find "$coredump_dir" -maxdepth 1 -type f -name 'core.*' -print0)

  if [ "$supported_in_dir" -eq 0 ]; then
    return
  fi
  SUPPORTED_CORES=$((SUPPORTED_CORES + supported_in_dir))

  command -v "$OCI_BIN" >/dev/null 2>&1 || die "container runtime '${OCI_BIN}' not found"
  local debug_image="$fedora_image"
  if ! "$OCI_BIN" image inspect "$debug_image" >/dev/null 2>&1 \
    && ! "$OCI_BIN" pull --platform "$platform" "$debug_image"; then
    debug_image="quay.io/fedora/fedora:${fedora_release}"
    echo "warning: recorded Fedora image is unavailable; falling back to ${debug_image}" >&2
  fi

  echo "Extracting ${supported_in_dir} Fedora OVN stack trace(s) with ${debug_image}"
  if ! "$OCI_BIN" run --rm -i \
    --platform "$platform" \
    --volume "${coredump_dir}:/artifacts:ro" \
    --volume "${output_dir}:/output" \
    --env "RECORDED_FEDORA_BASE_IMAGE=${fedora_image}" \
    --env "DEBUG_IMAGE=${debug_image}" \
    "$debug_image" \
    bash -s -- "$rpm_arch" "${debugger_args[@]}" < "$FEDORA_OVN_DEBUGGER"; then
    echo "error: failed to extract stack traces from ${coredump_dir}" >&2
    FAILED_CORES=$((FAILED_CORES + supported_in_dir))
    return
  fi

  local trace_path
  for trace_path in "${trace_paths[@]}"; do
    echo "Wrote ${trace_path}"
  done
}

if [ "$#" -eq 1 ] && { [ "$1" = "-h" ] || [ "$1" = "--help" ]; }; then
  usage
  exit 0
fi
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  usage >&2
  exit 2
fi

archive=$1
output_dir=${2:-"$(pwd)/stacktraces"}

[ -f "$archive" ] || die "archive not found: ${archive}"
[ -r "$FEDORA_OVN_DEBUGGER" ] || die "debugger helper not found: ${FEDORA_OVN_DEBUGGER}"
command -v tar >/dev/null 2>&1 || die "tar not found"
command -v file >/dev/null 2>&1 || die "file not found"
command -v strings >/dev/null 2>&1 || die "strings not found"

archive_dir=$(cd -- "$(dirname "$archive")" >/dev/null 2>&1 && pwd -P)
archive="${archive_dir}/$(basename "$archive")"
mkdir -p "$output_dir"
output_dir=$(cd -- "$output_dir" >/dev/null 2>&1 && pwd -P)

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
work_dir=$(cd -- "$work_dir" >/dev/null 2>&1 && pwd -P)
unpack_dir="${work_dir}/kind-logs"
mkdir -p "$unpack_dir"

if ! tar -tf "$archive" >/dev/null; then
  die "not a readable tar archive: ${archive}"
fi
if ! tar -tf "$archive" | awk '
  /^\// { exit 1 }
  {
    count = split($0, parts, "/")
    for (i = 1; i <= count; i++) {
      if (parts[i] == "..") exit 1
    }
  }
'; then
  die "archive contains an unsafe path"
fi
tar -xf "$archive" -C "$unpack_dir"

while IFS= read -r -d '' coredump_dir; do
  COREDUMP_DIRS=$((COREDUMP_DIRS + 1))
  process_coredump_dir "$coredump_dir"
done < <(find "$unpack_dir" -type d -name coredumps -print0)

if [ "$COREDUMP_DIRS" -eq 0 ]; then
  echo "No coredump directory found in ${archive}"
elif [ "$SUPPORTED_CORES" -eq 0 ] && [ "$FAILED_CORES" -eq 0 ]; then
  echo "No supported coredumps found in ${archive}"
fi

if [ "$FAILED_CORES" -ne 0 ]; then
  die "failed to process ${FAILED_CORES} coredump(s)"
fi
