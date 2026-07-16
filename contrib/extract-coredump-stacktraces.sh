#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Extract stack traces from coredumps in a tar archive of KIND logs.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly SCRIPT_DIR
source "${SCRIPT_DIR}/coredump/common.sh"
readonly GDB_STACKTRACE_HELPER="${SCRIPT_DIR}/coredump/gdb-stacktrace.sh"
readonly FLAVOR_DIR="${SCRIPT_DIR}/coredump/flavors"
readonly OCI_BIN="${OCI_BIN:-docker}"
readonly GO_DEBUG_IMAGE="${GO_DEBUG_IMAGE:-quay.io/fedora/fedora:latest}"

SUPPORTED_CORES=0
FAILED_CORES=0
COREDUMP_DIRS=0

# Results from inspect_coredump. Keep these as separate variables so paths and
# image references never need to be serialized and parsed again.
CORE_FILENAME=""
CORE_EXECUTABLE=""
CORE_IMAGE_REF=""
CORE_HANDLER=""
CORE_FEDORA_RELEASE=""
CORE_PLATFORM=""
CORE_RPM_ARCH=""
CORE_TRACE_PATH=""

usage() {
  cat <<EOF
Usage: $(basename "$0") KIND_LOGS_TARBALL [OUTPUT_DIRECTORY]

Extract stack traces from coredumps collected with export-kind-logs.sh.
Stack traces are written to OUTPUT_DIRECTORY (default: ./stacktraces).

Currently supported:
  - ovn-northd and ovn-controller from the Fedora ovn-daemonset image
  - Go executables with embedded build and debug information
  - FRR daemons from the Alpine-based quay.io/frrouting/frr image

Set OCI_BIN to use a container runtime other than docker.
Set GO_DEBUG_IMAGE to override the image used to install and run Delve.
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

go_version_for_binary() {
  strings "$1" \
    | sed -n 's/^\(go1\.[0-9][0-9]*\(\.[0-9][0-9]*\)\{0,1\}\)$/\1/p' \
    | sed -n '1p'
}

is_frr_executable() {
  case $1 in
    babeld | bfdd | bgpd | eigrpd | fabricd | isisd | ldpd | mgmtd | nhrpd | \
      ospf6d | ospfd | pathd | pim6d | pimd | ripd | ripngd | staticd | vrrpd | \
      watchfrr | zebra) return 0 ;;
    *) return 1 ;;
  esac
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

run_debugger_container() {
  local description=$1
  local core_count=$2
  local coredump_dir=$3
  local platform=$4
  local debug_image=$5
  local flavor=$6
  local helper
  shift 6

  case $flavor in
    "" | *[!a-z0-9-]*) die "invalid debugger flavor: ${flavor}" ;;
  esac
  helper="${FLAVOR_DIR}/${flavor}.sh"
  [ -r "$helper" ] || die "debugger flavor not found: ${flavor}"

  echo "Extracting ${core_count} ${description} stack trace(s) with ${debug_image}"
  if ! "$OCI_BIN" run --rm -i \
    --platform "$platform" \
    --volume "${coredump_dir}:/artifacts:ro" \
    --volume "${GDB_STACKTRACE_HELPER}:/gdb-stacktrace.sh:ro" \
    --volume "${output_dir}:/output" \
    --env "DEBUG_IMAGE=${debug_image}" \
    --entrypoint bash \
    "$debug_image" \
    -s -- "$@" < "$helper"; then
    echo "error: failed to extract ${description} stack traces from ${coredump_dir}" >&2
    FAILED_CORES=$((FAILED_CORES + core_count))
    return 1
  fi
}

report_trace_paths() {
  local -a args=("$@")
  local i

  for ((i = 2; i < ${#args[@]}; i += 4)); do
    echo "Wrote ${output_dir}/$(basename "${args[$i]}")"
  done
}

process_fedora_ovn_coredumps() {
  local coredump_dir=$1
  local platform=$2
  local rpm_arch=$3
  local fedora_release=$4
  local fedora_image=$5
  shift 5
  local -a debugger_args=("$@")
  local core_count=$((${#debugger_args[@]} / 4))
  local debug_image

  [ "$core_count" -gt 0 ] || return 0
  SUPPORTED_CORES=$((SUPPORTED_CORES + core_count))

  debug_image=$fedora_image
  if ! "$OCI_BIN" image inspect "$debug_image" >/dev/null 2>&1 \
    && ! "$OCI_BIN" pull --platform "$platform" "$debug_image"; then
    debug_image="quay.io/fedora/fedora:${fedora_release}"
    echo "warning: recorded Fedora image is unavailable; falling back to ${debug_image}" >&2
  fi

  if run_debugger_container \
    "Fedora OVN" "$core_count" "$coredump_dir" "$platform" "$debug_image" \
    fedora-ovn "$rpm_arch" "$fedora_image" "${debugger_args[@]}"; then
    report_trace_paths "${debugger_args[@]}"
  fi
}

process_go_coredumps() {
  local coredump_dir=$1
  local platform=$2
  shift 2
  local -a debugger_args=("$@")
  local core_count=$((${#debugger_args[@]} / 4))

  [ "$core_count" -gt 0 ] || return 0
  SUPPORTED_CORES=$((SUPPORTED_CORES + core_count))

  if run_debugger_container \
    "Go" "$core_count" "$coredump_dir" "$platform" "$GO_DEBUG_IMAGE" \
    go "${debugger_args[@]}"; then
    report_trace_paths "${debugger_args[@]}"
  fi
}

process_alpine_frr_coredumps() {
  local coredump_dir=$1
  local platform=$2
  local frr_image=$3
  shift 3
  local -a debugger_args=("$@")
  local core_count=$((${#debugger_args[@]} / 4))

  [ "$core_count" -gt 0 ] || return 0
  SUPPORTED_CORES=$((SUPPORTED_CORES + core_count))

  if run_debugger_container \
    "Alpine FRR" "$core_count" "$coredump_dir" "$platform" "$frr_image" \
    alpine-frr "${debugger_args[@]}"; then
    report_trace_paths "${debugger_args[@]}"
  fi
}

reject_coredump() {
  local filename=$1
  shift

  echo "error: ${filename}: $*" >&2
  FAILED_CORES=$((FAILED_CORES + 1))
}

inspect_coredump() {
  local coredump_dir=$1
  local image_info=$2
  local core=$3
  local remainder binary platform_info

  CORE_FILENAME=$(basename "$core")
  remainder=${CORE_FILENAME#core.}
  remainder=${remainder#*.}
  CORE_EXECUTABLE=${remainder%%.*}
  CORE_IMAGE_REF=""
  CORE_HANDLER=""
  CORE_FEDORA_RELEASE=""
  CORE_PLATFORM=""
  CORE_RPM_ARCH=""
  CORE_TRACE_PATH=""

  binary="${coredump_dir}/binaries/${CORE_EXECUTABLE}"
  if [ ! -s "$binary" ]; then
    case $CORE_EXECUTABLE in
      ovn-northd | ovn-controller)
        reject_coredump \
          "$CORE_FILENAME" "collected binary ${CORE_EXECUTABLE} is missing"
        ;;
      *)
        if is_frr_executable "$CORE_EXECUTABLE"; then
          reject_coredump \
            "$CORE_FILENAME" "collected binary ${CORE_EXECUTABLE} is missing"
        else
          echo "Skipping unsupported coredump: ${CORE_FILENAME}"
        fi
        ;;
    esac
    return 1
  fi

  if [ -f "$image_info" ]; then
    CORE_IMAGE_REF=$(image_for_executable "$CORE_EXECUTABLE" "$image_info")
  fi

  case $CORE_EXECUTABLE in
    ovn-northd | ovn-controller)
      CORE_FEDORA_RELEASE=$(fedora_release_for_binary "$binary")
      if [ -n "$CORE_FEDORA_RELEASE" ]; then
        CORE_HANDLER=fedora-ovn
      else
        reject_coredump \
          "$CORE_FILENAME" \
          "could not determine Fedora release for ${CORE_EXECUTABLE}"
        return 1
      fi
      ;;
  esac
  if [ -z "$CORE_HANDLER" ] \
    && [ -n "$(go_version_for_binary "$binary")" ]; then
    CORE_HANDLER=go
  fi
  if [ -z "$CORE_HANDLER" ] && is_frr_executable "$CORE_EXECUTABLE"; then
    case $CORE_IMAGE_REF in
      quay.io/frrouting/frr@* | quay.io/frrouting/frr:*)
        CORE_HANDLER=alpine-frr
        ;;
    esac
  fi
  if [ -z "$CORE_HANDLER" ]; then
    echo "Skipping unsupported coredump: ${CORE_FILENAME}"
    return 1
  fi

  if [ ! -f "$image_info" ]; then
    reject_coredump "$CORE_FILENAME" "missing binaries/image-info.txt"
    return 1
  fi
  if [ -z "$CORE_IMAGE_REF" ]; then
    reject_coredump \
      "$CORE_FILENAME" "no image recorded for ${CORE_EXECUTABLE}"
    return 1
  fi

  if ! platform_info=$(platform_for_binary "$binary"); then
    reject_coredump "$CORE_FILENAME" "unsupported binary architecture"
    return 1
  fi
  CORE_PLATFORM=${platform_info%% *}
  CORE_RPM_ARCH=${platform_info#* }
  CORE_TRACE_PATH=$(stacktrace_path "$output_dir" "$core")
}

validate_batch_compatibility() {
  local platform=$1
  local fedora_release=$2
  local frr_image=$3
  local fedora_image_file=$4

  if [ -n "$platform" ] && [ "$platform" != "$CORE_PLATFORM" ]; then
    reject_coredump \
      "$CORE_FILENAME" "mixed architectures in one coredump directory"
    return 1
  fi

  case $CORE_HANDLER in
    fedora-ovn)
      if [ -n "$fedora_release" ] \
        && [ "$fedora_release" != "$CORE_FEDORA_RELEASE" ]; then
        reject_coredump \
          "$CORE_FILENAME" "mixed Fedora releases in one coredump directory"
        return 1
      fi
      if [ ! -s "$fedora_image_file" ]; then
        reject_coredump "$CORE_FILENAME" "Fedora base image metadata is missing"
        return 1
      fi
      ;;
    alpine-frr)
      if [ -n "$frr_image" ] && [ "$frr_image" != "$CORE_IMAGE_REF" ]; then
        reject_coredump \
          "$CORE_FILENAME" "mixed FRR images in one coredump directory"
        return 1
      fi
      ;;
  esac
}

process_coredump_dir() {
  local coredump_dir=$1
  local image_info="${coredump_dir}/binaries/image-info.txt"
  local fedora_image_file="${coredump_dir}/binaries/fedora-base-image.txt"
  local fedora_image=""
  local fedora_release=""
  local frr_image=""
  local platform=""
  local rpm_arch=""
  local core
  local -a fedora_ovn_args=()
  local -a go_args=()
  local -a alpine_frr_args=()
  local -a core_args=()

  while IFS= read -r -d '' core; do
    if ! inspect_coredump "$coredump_dir" "$image_info" "$core"; then
      continue
    fi
    if ! validate_batch_compatibility \
      "$platform" "$fedora_release" "$frr_image" "$fedora_image_file"; then
      continue
    fi

    platform=$CORE_PLATFORM
    rpm_arch=$CORE_RPM_ARCH
    case $CORE_HANDLER in
      fedora-ovn)
        fedora_release=$CORE_FEDORA_RELEASE
        if [ -z "$fedora_image" ]; then
          fedora_image=$(tr -d '\r\n' < "$fedora_image_file")
        fi
        ;;
      go) ;;
      alpine-frr)
        frr_image=$CORE_IMAGE_REF
        ;;
    esac

    core_args=(
      "/artifacts/binaries/${CORE_EXECUTABLE}"
      "/artifacts/${CORE_FILENAME}"
      "/output/$(basename "$CORE_TRACE_PATH")"
      "$CORE_IMAGE_REF"
    )
    case $CORE_HANDLER in
      fedora-ovn) fedora_ovn_args+=("${core_args[@]}") ;;
      go) go_args+=("${core_args[@]}") ;;
      alpine-frr) alpine_frr_args+=("${core_args[@]}") ;;
    esac
    rm -f -- "$CORE_TRACE_PATH"
  done < <(find "$coredump_dir" -maxdepth 1 -type f \
    -name "$COREDUMP_GLOB" -print0)

  if [ "${#fedora_ovn_args[@]}" -eq 0 ] \
    && [ "${#go_args[@]}" -eq 0 ] \
    && [ "${#alpine_frr_args[@]}" -eq 0 ]; then
    return
  fi

  command -v "$OCI_BIN" >/dev/null 2>&1 || die "container runtime '${OCI_BIN}' not found"
  process_fedora_ovn_coredumps \
    "$coredump_dir" "$platform" "$rpm_arch" "$fedora_release" "$fedora_image" \
    "${fedora_ovn_args[@]}"
  process_go_coredumps "$coredump_dir" "$platform" "${go_args[@]}"
  process_alpine_frr_coredumps \
    "$coredump_dir" "$platform" "$frr_image" "${alpine_frr_args[@]}"
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
[ -r "$GDB_STACKTRACE_HELPER" ] || die "debugger helper not found: ${GDB_STACKTRACE_HELPER}"
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
