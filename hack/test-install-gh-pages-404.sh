#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Shell-level coverage for install_gh_pages_404 (hack/lib/install-gh-pages-404.sh).
# Uses disposable git repos — no network, no mike.
#
# Usage (from repo root):
#   ./hack/test-install-gh-pages-404.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/install-gh-pages-404.sh
source "${ROOT_DIR}/hack/lib/install-gh-pages-404.sh"
# shellcheck source=hack/lib/require-clean-worktree.sh
source "${ROOT_DIR}/hack/lib/require-clean-worktree.sh"

PASS=0
FAIL=0
TMP=""

cleanup() {
  if [[ -n "${TMP}" && -d "${TMP}" ]]; then
    rm -rf "${TMP}"
  fi
}
trap cleanup EXIT

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "${got}" == "${want}" ]]; then
    echo "  PASS: ${name}"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ${name} (got='${got}' want='${want}')" >&2
    FAIL=$((FAIL + 1))
  fi
}

assert_true() {
  local name="$1"
  shift
  if "$@"; then
    echo "  PASS: ${name}"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ${name}" >&2
    FAIL=$((FAIL + 1))
  fi
}

assert_false() {
  local name="$1"
  shift
  if ! "$@"; then
    echo "  PASS: ${name}"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ${name} (expected failure)" >&2
    FAIL=$((FAIL + 1))
  fi
}

git_init_identity() {
  git config user.email "docs-404-test@example.com"
  git config user.name "docs-404-test"
}

make_bare_remote() {
  local bare="$1"
  local branch="${2:-master}"
  local seed
  seed="$(mktemp -d)"
  git init -q -b "${branch}" "${seed}"
  (
    cd "${seed}"
    git_init_identity
    echo seed > README
    git add README
    git commit -q -m "seed"
  )
  git clone -q --bare "${seed}" "${bare}"
  rm -rf "${seed}"
}

clone_worktree() {
  local bare="$1"
  local work="$2"
  git clone -q "${bare}" "${work}"
  (
    cd "${work}"
    git_init_identity
  )
}

ensure_local_gh_pages() {
  git checkout -q -b gh-pages
  echo pages > index.html
  git add index.html
  git commit -q -m "gh-pages seed"
  git checkout -q master
}

begin_fixture() {
  cleanup
  TMP="$(mktemp -d)"
  make_bare_remote "${TMP}/remote.git"
  clone_worktree "${TMP}/remote.git" "${TMP}/work"
  cd "${TMP}/work"
}

echo "==> test: skip when no local or remote gh-pages"
begin_fixture
SITE_404_CACHE="$(mktemp)"
echo 'TEMPLATE-A' > "${SITE_404_CACHE}"
PUSH=false
out="$(install_gh_pages_404 2>&1 || true)"
assert_true "warns and skips" grep -q "skipping root 404.html" <<<"${out}"
assert_false "did not create gh-pages" git show-ref --verify --quiet refs/heads/gh-pages
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: existing local gh-pages, changed template, PUSH=false"
begin_fixture
ensure_local_gh_pages
SITE_404_CACHE="$(mktemp)"
echo 'TEMPLATE-NEW' > "${SITE_404_CACHE}"
PUSH=false
install_gh_pages_404 >/dev/null
assert_eq "404 contents" "$(cat 404.html)" "TEMPLATE-NEW"
msg="$(git log -1 --pretty=%s)"
assert_eq "commit message" "${msg}" "docs: preserve pre-versioning deep links by refreshing root 404"
assert_false "origin has no gh-pages" git -C "${TMP}/remote.git" show-ref --verify --quiet refs/heads/gh-pages
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: unchanged template is a no-op"
begin_fixture
ensure_local_gh_pages
SITE_404_CACHE="$(mktemp)"
echo 'SAME' > "${SITE_404_CACHE}"
PUSH=false
install_gh_pages_404 >/dev/null
commits_after_first="$(git rev-list --count gh-pages)"
out="$(install_gh_pages_404 2>&1)"
assert_true "reports up to date" grep -q "already up to date" <<<"${out}"
assert_eq "no extra commit" "$(git rev-list --count gh-pages)" "${commits_after_first}"
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: remote-only gh-pages (create local tracking branch)"
begin_fixture
ensure_local_gh_pages
git push -q origin gh-pages
cd "${ROOT_DIR}"
# Fresh clone: origin/gh-pages exists, local gh-pages does not.
rm -rf "${TMP}/work"
clone_worktree "${TMP}/remote.git" "${TMP}/work"
cd "${TMP}/work"
assert_false "no local gh-pages before install" git show-ref --verify --quiet refs/heads/gh-pages
assert_true "remote gh-pages present" git show-ref --verify --quiet refs/remotes/origin/gh-pages
SITE_404_CACHE="$(mktemp)"
echo 'FROM-REMOTE-ONLY' > "${SITE_404_CACHE}"
PUSH=false
install_gh_pages_404 >/dev/null
assert_true "local gh-pages created" git show-ref --verify --quiet refs/heads/gh-pages
assert_eq "404 installed" "$(git show gh-pages:404.html)" "FROM-REMOTE-ONLY"
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: PUSH=true pushes commit to origin"
begin_fixture
ensure_local_gh_pages
git push -q origin gh-pages
SITE_404_CACHE="$(mktemp)"
echo 'PUSHED-TEMPLATE' > "${SITE_404_CACHE}"
PUSH=true
install_gh_pages_404 >/dev/null
assert_eq "remote 404" "$(git --git-dir="${TMP}/remote.git" show gh-pages:404.html)" "PUSHED-TEMPLATE"
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: PUSH=true surfaces push failure"
begin_fixture
ensure_local_gh_pages
git push -q origin gh-pages
cat > "${TMP}/remote.git/hooks/pre-receive" <<'EOF'
#!/bin/sh
echo "push rejected by test hook" >&2
exit 1
EOF
chmod +x "${TMP}/remote.git/hooks/pre-receive"
SITE_404_CACHE="$(mktemp)"
echo 'WILL-FAIL-PUSH' > "${SITE_404_CACHE}"
PUSH=true
set +e
install_gh_pages_404 >/dev/null 2>"${TMP}/err"
ec=$?
set -e
assert_false "install fails when push fails" test "${ec}" -eq 0
assert_true "error mentions rejection" grep -Eqi 'rejected|hook|denied|error' "${TMP}/err"
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo "==> test: require_clean_worktree allows a clean repo"
begin_fixture
set +e
( require_clean_worktree ) >/dev/null 2>"${TMP}/err"
ec=$?
set -e
assert_eq "clean worktree exit code" "${ec}" "0"
cd "${ROOT_DIR}"

echo "==> test: require_clean_worktree rejects dirty tracked files"
begin_fixture
echo "local-edit" >> README
set +e
( require_clean_worktree ) >/dev/null 2>"${TMP}/err"
ec=$?
set -e
assert_false "dirty worktree is rejected" test "${ec}" -eq 0
assert_true "error mentions dirty worktree" grep -q "dirty worktree" "${TMP}/err"
# Confirm the local edit was not discarded by the preflight itself.
assert_true "local edit still present" grep -q "local-edit" README
cd "${ROOT_DIR}"

echo "==> test: dirty worktree survives install_gh_pages_404 only if caller gates first"
# Regression: without require_clean_worktree, checkout -f would drop tracked edits.
# With the gate, we refuse before switching branches so the edit remains.
begin_fixture
ensure_local_gh_pages
echo "keep-me" >> README
SITE_404_CACHE="$(mktemp)"
echo 'TEMPLATE' > "${SITE_404_CACHE}"
PUSH=false
set +e
( require_clean_worktree ) >/dev/null 2>"${TMP}/err"
gate_ec=$?
set -e
assert_false "gate blocks before install" test "${gate_ec}" -eq 0
assert_true "README edit preserved" grep -q "keep-me" README
assert_eq "still on master" "$(git symbolic-ref --short HEAD)" "master"
rm -f "${SITE_404_CACHE}"
cd "${ROOT_DIR}"

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
if [[ "${FAIL}" -ne 0 ]]; then
  exit 1
fi
