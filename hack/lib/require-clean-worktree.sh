# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Refuse to run when tracked files are modified. Force-checkouts in the docs
# publish path (mike deploy, install_gh_pages_404) would discard those changes.
# Sourced by hack/build-docs.sh and hack/test-install-gh-pages-404.sh.
#
# Optional: die() may already be defined by the caller. If not, a minimal one
# is provided.

if ! declare -F die >/dev/null 2>&1; then
  die() {
    echo "ERROR: $*" >&2
    exit 1
  }
fi

require_clean_worktree() {
  # Only tracked changes matter: git checkout -f discards them. Untracked files
  # are left alone and are not a reason to block the build.
  if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
    die "refusing to run with a dirty worktree (tracked files have local changes).

git checkout -f during versioned docs builds would discard those changes.
Commit, stash, or discard local modifications, then retry.
See docs/developer-guide/documentation.md for details.

$(git status --porcelain --untracked-files=no)"
  fi
}
