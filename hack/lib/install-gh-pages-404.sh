# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Install GitHub Pages root 404.html after mike deploy. Sourced by
# hack/build-docs.sh and hack/test-install-gh-pages-404.sh.
#
# Required environment:
#   SITE_404_CACHE  path to the cached template file to install
#   PUSH            "true" to push gh-pages after commit; otherwise local only

install_gh_pages_404() {
  echo "==> Installing root 404.html on gh-pages"
  if ! git show-ref --verify --quiet refs/heads/gh-pages; then
    if git show-ref --verify --quiet refs/remotes/origin/gh-pages; then
      git branch gh-pages origin/gh-pages
    else
      echo "WARNING: no local or remote gh-pages branch; skipping root 404.html"
      return 0
    fi
  fi

  git checkout -f gh-pages
  cp "${SITE_404_CACHE}" 404.html
  git add 404.html
  if git diff --cached --quiet; then
    echo "    404.html already up to date"
    return 0
  fi

  git commit -m "docs: preserve pre-versioning deep links by refreshing root 404"
  if [[ "${PUSH}" == "true" ]]; then
    git push origin gh-pages
  fi
}
