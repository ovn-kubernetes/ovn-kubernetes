#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -o errexit
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
GO_CONTROLLER_ROOT=$(readlink --canonicalize "${HERE}/..")
REPO_ROOT=$(readlink --canonicalize "${GO_CONTROLLER_ROOT}/..")
UPDATE_SCRIPT="${HERE}/update-third-party-licenses.sh"

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ovn-verify-third-party-licenses.XXXXXX")
trap 'rm -rf "${TMP_DIR}"' EXIT

GENERATED_LICENSES_DIR=${TMP_DIR}/LICENSES
DIFF_FOUND=0

check_symlinks_absent() {
    local path=$1
    local label=$2
    local found=0
    local symlink_path

    while IFS= read -r symlink_path; do
        [[ -n "${symlink_path}" ]] || continue
        if [[ ${found} -eq 0 ]]; then
            echo "${label} must not contain symlinks:" >&2
            found=1
        fi
        echo "  - ${symlink_path}" >&2
    done < <(find "${path}" -type l -print | LC_ALL=C sort)

    [[ ${found} -eq 0 ]]
}

write_relative_tree_paths() {
    local root=$1
    local path

    while IFS= read -r path; do
        printf '%s\n' "${path#"${root}/"}"
    done < <(find "${root}" -type f -print | LC_ALL=C sort)
}

LICENSES_DIR="${GENERATED_LICENSES_DIR}" "${UPDATE_SCRIPT}"

if ! check_symlinks_absent "${GENERATED_LICENSES_DIR}" "generated LICENSES tree"; then
    DIFF_FOUND=1
fi

if [[ -d "${REPO_ROOT}/LICENSES" ]]; then
    if ! check_symlinks_absent "${REPO_ROOT}/LICENSES" "checked-in LICENSES tree"; then
        DIFF_FOUND=1
    fi
    if ! diff -u <(write_relative_tree_paths "${REPO_ROOT}/LICENSES") <(write_relative_tree_paths "${GENERATED_LICENSES_DIR}"); then
        DIFF_FOUND=1
    fi
    if ! diff -ruN "${REPO_ROOT}/LICENSES" "${GENERATED_LICENSES_DIR}"; then
        DIFF_FOUND=1
    fi
else
    if ! diff -ruN /dev/null "${GENERATED_LICENSES_DIR}"; then
        DIFF_FOUND=1
    fi
fi

if [[ -L "${REPO_ROOT}/LICENSE" ]]; then
    echo "repo root LICENSE must be a regular file" >&2
    DIFF_FOUND=1
fi

if [[ ! -f "${REPO_ROOT}/LICENSE" ]]; then
    echo "repo root LICENSE is missing" >&2
    DIFF_FOUND=1
elif ! cmp -s "${REPO_ROOT}/LICENSE" "${GENERATED_LICENSES_DIR}/LICENSE"; then
    echo "repo root LICENSE must match LICENSES/LICENSE" >&2
    DIFF_FOUND=1
fi

if [[ ${DIFF_FOUND} -ne 0 ]]; then
    echo "third-party license files are out of date; run 'make -C go-controller third-party-licenses && git add LICENSES'" >&2
    exit 1
fi

echo "third-party license files are up to date"
