# Team Guidance: Docs Versioning with Mike

## What Changed

- A new workflow ([`.github/workflows/docs-versioning.yml`](.github/workflows/docs-versioning.yml)) now builds and deploys **versioned documentation** using [mike](https://github.com/jimporter/mike).
- Every `release-*` branch and `master` gets its own entry in a **version dropdown** on the docs site.
- The existing [`docs.yml`](.github/workflows/docs.yml) workflow previously deployed an unversioned site. Its `deploy` job has been removed to avoid conflicts; only the `test-deploy` PR validation job remains.

## What the Team Must Know

### 1. The `docs.yml` deploy job has been removed

The old `docs.yml` deployed an unversioned site via the GitHub Pages API. The new `docs-versioning.yml` deploys a versioned site via the `gh-pages` branch. Running both would cause them to overwrite each other. The `deploy` job has been removed from `docs.yml`, and only the `test-deploy` job remains (so PRs still validate that docs build cleanly).

### 2. Docs rebuild triggers

The versioning workflow triggers only when docs-related files change (`docs/**`, `mkdocs.yml`, or the workflow file itself) on `master` or any `release-*` branch. Each run rebuilds **all** versions to keep labels (Legacy/Stable) and the version dropdown consistent. The team does **not** need to do anything special; it happens automatically.

### 3. Each branch keeps its own `mkdocs.yml`

The nav structure (`nav:` section in `mkdocs.yml`) is **per-branch**. This means:
- If a new docs page is added on `master`, it does **not** appear in older release versions (and won't break them either).
- If the team wants a new page to appear in an older release, they must backport the markdown file **and** update that branch's `mkdocs.yml` nav.

### 4. Only `docs/stylesheets/extra.css` is shared from master

Visual styling (colors, fonts, spacing) is consistent across all versions because the workflow copies `extra.css` from master onto every branch at build time. So:
- **Do**: make CSS/styling changes on `master` only -- they propagate everywhere on next rebuild.
- **Don't**: edit `docs/stylesheets/extra.css` on release branches -- it gets overwritten.

### 5. Creating a new release branch

When the team creates a new `release-X.Y` branch and pushes it:
- The workflow auto-discovers it and adds it to the dropdown.
- The previous highest release is automatically relabeled from "Stable" to "Legacy".
- The new release becomes "Stable" with the `latest` alias.
- **No manual steps required.**

### 6. The branch must have a valid `mkdocs.yml`

If a release branch does not have a `mkdocs.yml` at its root, `mike deploy` will fail for that version. The workflow injects the `version.provider: mike` setting if missing, but the file itself must exist. Older branches that predate mkdocs will need a `mkdocs.yml` added manually (one-time).

### 7. Local preview with mike

Developers can preview the versioned site locally:
```bash
pip install mkdocs-material mike mkdocs-awesome-pages-plugin mkdocs-mermaid2-plugin mkdocs-glightbox mkdocs-meta-descriptions-plugin
mike deploy master "Master (Dev)"
mike serve
```
This serves the site at `http://localhost:8000` with the version dropdown.

### 8. Manual rebuild

The workflow supports `workflow_dispatch`, so anyone with write access can trigger a full rebuild from the **Actions** tab without pushing code. Useful after fixing a broken release branch or when the `gh-pages` branch needs a fresh start.

## Summary of Team Rules

- **Do not** edit `extra.css` on release branches (it gets overwritten).
- **Do** ensure every release branch has a `mkdocs.yml` with a valid `nav`.
- **Do not** manually push to the `gh-pages` branch -- the workflow owns it.
- New release branches are auto-detected; no workflow changes needed.
- CSS/theme changes go to `master` only.
