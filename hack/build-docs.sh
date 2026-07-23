#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Build and deploy versioned documentation with mike.
#
# Usage:
#   hack/build-docs.sh [--push] [--releases all|branch[,branch...]]
#
# Prefer the docs/ Makefile wrapper:
#   make -C docs build-docs
#   make -C docs build-docs PUSH=1
#   make -C docs build-docs RELEASES=release-1.1
#   make -C docs build-docs RELEASES=release-1.1,master PUSH=1
#
# RELEASES defaults to "all" (every origin/release-* branch plus master).
# --push pushes the gh-pages branch to origin (used by CI).
# See docs/developer-guide/documentation.md for team guidance.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

PUSH=false
RELEASES="${RELEASES:-all}"

usage() {
  cat <<EOF
Usage: $(basename "$0") [--push] [--releases all|branch[,branch...]]

Options:
  --push              Push the gh-pages branch to origin after deploy
  --releases VALUE    Branches to deploy: "all" (default) or a comma-separated
                      list of existing branches (e.g. release-1.1,master)
  -h, --help          Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push)
      PUSH=true
      shift
      ;;
    --releases)
      RELEASES="${2:?--releases requires a value}"
      shift 2
      ;;
    --releases=*)
      RELEASES="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

die() {
  echo "ERROR: $*" >&2
  exit 1
}

# --- Safety validations (hard-fail; no interactive override) ---
#
# Temporary / manual edits of gh-pages or of extra.css on release branches are
# not allowed. The automated versioning process owns gh-pages, and release
# branches always receive master's extra.css at build time.

current_branch="$(git symbolic-ref --short HEAD 2>/dev/null || true)"
if [[ "${current_branch}" == "gh-pages" ]]; then
  die "refusing to run from the 'gh-pages' branch.

The docs-versioning process owns gh-pages. Check out master (or a release
branch) and use 'make -C docs build-docs' instead.
See docs/developer-guide/documentation.md for details."
fi

if [[ "${current_branch}" =~ ^release- ]]; then
  if ! git diff --quiet -- docs/stylesheets/extra.css 2>/dev/null || \
     ! git diff --cached --quiet -- docs/stylesheets/extra.css 2>/dev/null; then
    die "docs/stylesheets/extra.css has local modifications on '${current_branch}'.

This file is overwritten from master during versioned docs builds.
Make CSS/styling changes on 'master' instead.
See docs/developer-guide/documentation.md for details."
  fi
fi

command -v mike >/dev/null 2>&1 || die "mike is not installed. Run: pip install -r requirements.txt"
command -v mkdocs >/dev/null 2>&1 || die "mkdocs is not installed. Run: pip install -r requirements.txt"

# mike deploy checks out origin/* refs and would otherwise leave the repo in
# detached HEAD. Restore the caller's starting branch/commit on exit.
START_REF="$(git symbolic-ref -q --short HEAD 2>/dev/null || git rev-parse HEAD)"
restore_start_ref() {
  local ec=$?
  if [[ -n "${START_REF}" ]] && ! git checkout -f "${START_REF}" >/dev/null 2>&1; then
    echo "WARNING: could not restore git checkout to '${START_REF}'" >&2
  fi
  exit "${ec}"
}
trap restore_start_ref EXIT

git fetch origin gh-pages --depth=1 2>/dev/null || true

# Cache master's stylesheet so every version gets the same look.
# DOCS_EXTRA_CSS=/path/to/extra.css overrides this for local feature-branch
# previews (so unmerged CSS is applied to every deployed version).
mkdir -p /tmp/master-stylesheets
if [[ -n "${DOCS_EXTRA_CSS:-}" ]]; then
  [[ -f "${DOCS_EXTRA_CSS}" ]] || die "DOCS_EXTRA_CSS is set but not a file: ${DOCS_EXTRA_CSS}"
  cp "${DOCS_EXTRA_CSS}" /tmp/master-stylesheets/extra.css
elif git show origin/master:docs/stylesheets/extra.css > /tmp/master-stylesheets/extra.css 2>/dev/null; then
  :
elif git show master:docs/stylesheets/extra.css > /tmp/master-stylesheets/extra.css 2>/dev/null; then
  :
else
  die "could not read docs/stylesheets/extra.css from origin/master (or master)"
fi

# Collect all release-* branches, sorted by version (ascending)
mapfile -t ALL_RELEASES < <(
  git branch -r --list 'origin/release-*' \
    | sed 's|^[[:space:]]*origin/||' \
    | sed '/^$/d' \
    | sort -V
)

# Labels (recomputed every full deploy when a new release-* appears):
#   highest release-*  → "<ver> (Latest)"  (+ mike alias "latest")
#   second-highest     → "<ver> (Stable)"
#   older releases     → "<ver> (Legacy)"
#   master             → "Master (Dev)"
LATEST_RELEASE=""
STABLE_RELEASE=""
if [[ ${#ALL_RELEASES[@]} -gt 0 ]]; then
  LATEST_RELEASE="${ALL_RELEASES[-1]}"
fi
if [[ ${#ALL_RELEASES[@]} -gt 1 ]]; then
  STABLE_RELEASE="${ALL_RELEASES[-2]}"
fi

deploy_master=false
branches_to_deploy=()

if [[ "${RELEASES}" == "all" ]]; then
  branches_to_deploy=("${ALL_RELEASES[@]+"${ALL_RELEASES[@]}"}")
  deploy_master=true
else
  IFS=',' read -r -a requested <<< "${RELEASES}"
  for raw in "${requested[@]}"; do
    branch="$(echo "${raw}" | xargs)"
    [[ -n "${branch}" ]] || continue
    if [[ "${branch}" == "master" ]]; then
      deploy_master=true
      continue
    fi
    if [[ ! "${branch}" =~ ^release- ]]; then
      die "invalid RELEASES entry '${branch}': expected 'master', 'release-*', or 'all'"
    fi
    if ! git show-ref --verify --quiet "refs/remotes/origin/${branch}"; then
      die "branch 'origin/${branch}' does not exist.
Available release branches: ${ALL_RELEASES[*]:-none}"
    fi
    branches_to_deploy+=("${branch}")
  done
  if [[ ${#branches_to_deploy[@]} -eq 0 && "${deploy_master}" != "true" ]]; then
    die "no branches selected. Use --releases all or a comma-separated list."
  fi
fi

# Optional --push flag for mike (empty when not pushing).
MIKE_PUSH=()
if [[ "${PUSH}" == "true" ]]; then
  MIKE_PUSH=(--push)
fi

ensure_mike_provider() {
  # Material only renders the version dropdown when extra.version.provider
  # is set. Older release branches often have the mike plugin but no extra:.
  python3 - <<'PY'
from pathlib import Path
import re
import sys

path = Path("mkdocs.yml")
text = path.read_text()
if re.search(r"provider:\s*mike", text):
    sys.exit(0)

version_under_extra = "  version:\n    provider: mike\n"
full_extra = "extra:\n  version:\n    provider: mike\n"

if re.search(r"^extra:\s*$", text, re.M):
    text, n = re.subn(r"(^extra:\s*\n)", r"\1" + version_under_extra, text, count=1, flags=re.M)
    if n != 1:
        sys.exit("failed to inject version under existing extra:")
elif re.search(r"^extra_css:", text, re.M):
    text, n = re.subn(r"^extra_css:", full_extra + "extra_css:", text, count=1, flags=re.M)
    if n != 1:
        sys.exit("failed to inject extra: before extra_css:")
else:
    text = full_extra + text

path.write_text(text)
if not re.search(r"provider:\s*mike", path.read_text()):
    sys.exit("failed to inject extra.version.provider: mike into mkdocs.yml")
PY
}

deploy_release_branch() {
  local branch="$1"
  local version
  version="$(echo "${branch}" | sed 's/release-//')"

  echo "==> Deploying ${branch} as version ${version}"
  git checkout -f "origin/${branch}"

  # Each branch keeps its own mkdocs.yml (and nav), but gets master's
  # stylesheet for a consistent visual appearance.
  mkdir -p docs/stylesheets
  cp /tmp/master-stylesheets/extra.css docs/stylesheets/extra.css

  ensure_mike_provider

  if [[ -n "${LATEST_RELEASE}" && "${branch}" == "${LATEST_RELEASE}" ]]; then
    mike deploy ${MIKE_PUSH[@]+"${MIKE_PUSH[@]}"} --update-aliases \
      --title "${version} (Latest)" "${version}" latest
  elif [[ -n "${STABLE_RELEASE}" && "${branch}" == "${STABLE_RELEASE}" ]]; then
    mike deploy ${MIKE_PUSH[@]+"${MIKE_PUSH[@]}"} \
      --title "${version} (Stable)" "${version}"
  else
    mike deploy ${MIKE_PUSH[@]+"${MIKE_PUSH[@]}"} \
      --title "${version} (Legacy)" "${version}"
  fi
}

for branch in "${branches_to_deploy[@]+"${branches_to_deploy[@]}"}"; do
  [[ -n "${branch}" ]] || continue
  deploy_release_branch "${branch}"
done

if [[ "${deploy_master}" == "true" ]]; then
  echo "==> Deploying master as Master (Dev)"
  git checkout -f origin/master
  # Same stylesheet as release versions (supports DOCS_EXTRA_CSS previews).
  mkdir -p docs/stylesheets
  cp /tmp/master-stylesheets/extra.css docs/stylesheets/extra.css
  ensure_mike_provider
  mike deploy ${MIKE_PUSH[@]+"${MIKE_PUSH[@]}"} --title "Master (Dev)" master

  # Master is the default landing page (only when deploying master / all)
  mike set-default ${MIKE_PUSH[@]+"${MIKE_PUSH[@]}"} master
fi

echo "Docs versioning deploy complete (releases=${RELEASES}, push=${PUSH})."
