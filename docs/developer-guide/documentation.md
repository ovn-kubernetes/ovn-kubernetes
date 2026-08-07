Documentation is an important step in the SDLC process. Unless there are
proper docs to tell end users about the code you contributed; there will
be less visibility and understanding of your code and feature. It will also
become hard to maintain such code.

## Writing Documentation

All OVN-Kubernetes docs are kept under the `docs/` folder in the main path.
There are specific folders such as `features` and `developer-guide` where
you can add your commit against. These simple `.md` files are referred from
the navigation tile in `mkdocs.yaml` file which is what is used to
build our [website](https://ovn-kubernetes.io/).

As a developer and contributor to this project; it is recommended that you
include a docs commit in your PR whenever possible and relevant. Some examples
where we mandate docs include:

* **Enhancement Proposals**: If you are planning to do a new feature, then start
with an enhancement proposal a.k.a OKEP so that maintainers and other reviewers
can get an understanding of what your goals are. See [here](https://ovn-kubernetes.io/okeps/okep-4368-template/)
for more details and open a commit adding it to our `docs/okeps` folder.

* **Architecture and Topology docs**: Did you change the architecture or topology?
If so please write docs reflecting your design changes and update any existing docs
so that they remain relevant. Open a commit adding it to our `docs/design` folder.

* **Feature Docs**: If your enhancement proposal has merged; next step is to
implement that feature. As part of the main implementation PR we mandate adding a
feature documentation commit. See [here](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/template.md)
for how a feature documentation should be done. Open a commit adding it to our
`docs/features` folder.

* **Contributor Docs**: If your changes include code cleanup or refactoring
that is not user facing but internal; example: "Adding DBIndexes to AddressSets" write up a
developer guide for contributors asking them to follow that new code structure process while
adding new AddressSets. Open a commit adding it to our `docs/developer-guide` folder.

* **Bug Fixes**: If there was a difficult or critical bug fix that warrants a doc; please
feel free to write it. If you are unsure where this should be placed; reach out to the maintainers.

* **Blog Posts**: Are you an end-user of OVN-Kubernetes? Is there something you wish to share with
the community about your awesome use cases and how you used our CNI to solve your problems? We
welcome blog post contributions from all! See [here](https://ovn-kubernetes.io/blog/) for details.
Open a commit adding it to our `docs/blog` folder.

* **Performance Enhancements**: We love performance enhancements! Did you write a cool patch
to reduce the time it takes for iptables to sync up on startup? Think about writing a good blog post
around this! Open a commit adding it to our `docs/blog` folder.

* **API Reference**: Did you introduce a new CRD? OR Did you add a watcher a new CRD? Include API
Reference documentation changes to `docs/api-reference` folder. See [here](https://ovn-kubernetes.io/api-reference/introduction/)
for more details.

## Website Guide

We are utilizing [GitHub Pages](https://docs.github.com/en/pages/quickstart) to host the ovn-kubernetes.io website. The website's
content is composed in Markdown and stored within the `docs/` directory at the root
of our repository. For static site generation, we employ the [MkDocs](https://www.mkdocs.org/user-guide/writing-your-docs/) framework,
managed by the `mkdocs.yml` configuration file located next to the `docs/` folder.
Additionally, we use the [Material](https://squidfunk.github.io/mkdocs-material/setup/) framework on top of MkDocs, which enhances our
site with advanced features like customizable navigation, color schemes, and site
search capabilities.

The published site is **versioned** with [mike](https://github.com/jimporter/mike).
Each `release-*` branch and `master` appears in a version dropdown on the docs site.
Deploy logic lives in
[`hack/build-docs.sh`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/hack/build-docs.sh)
and is invoked via `make -C docs build-docs` (see
[`docs/Makefile`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/Makefile)).
The
[Publish Versioned Docs](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs-versioning.yml)
workflow runs that target with `PUSH=1` and updates the `gh-pages` branch.

The [docs.yml](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs.yml)
workflow keeps a `test-deploy` job so pull requests still validate that docs build
cleanly. It does not publish the live site; publishing is owned by the versioning
workflow so the two do not overwrite each other.

### Versioned docs: how publishing works

The versioning workflow runs on pushes to `master` or any `release-*` branch when
docs-related paths change (`docs/**`, `mkdocs.yml`, the workflow file,
`hack/build-docs.sh`, or `requirements.txt`). By default each run rebuilds **all**
versions so Latest/Stable/Legacy labels and the version dropdown stay consistent.

- The nav structure (`nav:` in `mkdocs.yml`) is **per-branch**. A new page added
  on `master` does not appear in older release versions (and will not break them).
  To ship a page in an older release, backport the markdown **and** update that
  branch's `mkdocs.yml` nav.
- Visual styling is shared: the build copies `docs/stylesheets/extra.css` from
  `master` onto every version at build time. Make CSS changes on `master` only.
  Do not edit `extra.css` on release branches — the file is overwritten, and
  `hack/build-docs.sh` hard-fails if it has local modifications on a `release-*`
  branch.
- When a new `release-X.Y` branch is created, the workflow discovers it
  (via the `create` event and/or a docs push) and the dropdown labels shift:
  - highest `release-*` → `<ver> (Latest)` (also the mike `latest` alias)
  - second-highest → `<ver> (Stable)`
  - older releases → `<ver> (Legacy)`
  - `master` → `Master (Dev)`
  Example today: `1.3 (Latest)`, `1.2 (Stable)`, `1.1 (Legacy)`, `1.0 (Legacy)`.
  After `release-1.4` appears: `1.4 (Latest)`, `1.3 (Stable)`, and older stay
  `(Legacy)`. No workflow edits are required.
- Every branch that is deployed must have a root `mkdocs.yml`. The build injects
  `extra.version.provider: mike` if missing, but the file itself must exist.
- Do not manually push to or develop on `gh-pages`. The versioning workflow owns it.
- Prefer `make -C docs build-docs` over ad-hoc `mike` commands so the same
  validations run locally and in CI.

### Manual rebuild (workflow_dispatch)

Anyone with write access can trigger a rebuild from the **Actions** tab
(Publish Versioned Docs). The `releases` input defaults to `all`. Set it to a
comma-separated list of existing branches (for example `release-1.1` or
`release-1.1,master`) to rebuild only those. Unknown branch names fail the job.

### What triggers a rebuild (and what does not)

| Event | Rebuilds docs site? |
|-------|---------------------|
| Push to `master` / `release-*` changing `docs/**`, `mkdocs.yml`, the versioning workflow, `hack/build-docs.sh`, or `requirements.txt` | Yes (all versions) |
| Creating a new `release-*` branch | Yes (`create` event; labels recompute) |
| Manual **workflow_dispatch** | Yes (`all` or the branches you list) |
| Go-only / non-docs commits on `master` or `release-*` | No (by design) |

### After this lands on master (one-time + first-run checks)

Upstream currently has GitHub Pages enabled but **no `gh-pages` branch** — the
old site was published with the Actions `deploy-pages` flow. Mike needs branch
publishing. A repo admin must do this once after the first successful
**Publish Versioned Docs** run (or right before it):

1. Open
   [Settings → Pages](https://github.com/ovn-kubernetes/ovn-kubernetes/settings/pages)
   on `ovn-kubernetes/ovn-kubernetes`.
2. Under **Build and deployment → Source**, choose **Deploy from a branch**.
3. Branch: **`gh-pages`** / folder: **`/` (root)**. Save.
4. Keep the custom domain (`ovn-kubernetes.io`) as already configured.

Then verify the first merge deploy:

1. Actions → **Publish Versioned Docs** is green for the merge commit.
2. The `gh-pages` branch exists and contains `versions.json` plus `master/`,
   `1.3/`, `1.2/`, …
3. Locally: `git fetch origin gh-pages && mike list` shows titles like
   `Master (Dev)`, `1.3 (Latest)`, `1.2 (Stable)`, `1.1 (Legacy)`, `1.0 (Legacy)`.
4. On the live site, the version dropdown switches and each version’s nav/content
   matches that release branch.

If Pages is still set to **GitHub Actions**, the workflow can succeed while the
public site does not update — fix the Source setting above.

## How to test your documentation changes?

### Option 1) Build and view docs with a PR

Pushing docs changes to the ovn-kubernetes/ovn-kubernetes project as a pull request will
run the job name "[Test Docs Build](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs.yml)" which has a step to save the
docs artifacts. You can download those as a .zip file, extract and view them locally.

### Option 2) Build and serve a single version locally

In order to test changes locally to either mkdocs.yml or to files under docs/ folder,
please follow the instructions below.

## Clone the repository
```text
# git clone https://github.com/ovn-kubernetes/ovn-kubernetes
Cloning into 'ovn-kubernetes'...
remote: Enumerating objects: 84258, done.
remote: Counting objects: 100% (1546/1546), done.
remote: Compressing objects: 100% (686/686), done.
remote: Total 84258 (delta 809), reused 1208 (delta 663), pack-reused 82712
Receiving objects: 100% (84258/84258), 56.39 MiB | 28.74 MiB/s, done.
Resolving deltas: 100% (55993/55993), done.

# cd ovn-kubernetes
```

## Create a Python virtual environment
```python
#  python -m venv venv
```

## Activate the virtual environment
```text
# source venv/bin/activate
```

## Install all the required python packages to render the website
```text
(venv) # pip install -r requirements.txt 
Collecting Click
  Using cached click-8.1.7-py3-none-any.whl (97 kB)
Collecting htmlmin

<output snipped for brevity>
```
## Run the website locally using
```text
(venv) # mkdocs serve
INFO    -  Building documentation...
INFO    -  Cleaning site directory

<output snipped for brevity>

INFO    -  Documentation built in 1.52 seconds
INFO    -  [17:05:21] Watching paths for changes: 'docs', 'mkdocs.yml'
INFO    -  [17:05:21] Serving on http://127.0.0.1:8000/ovn-kubernetes/
```
Now you can browse the website on your browser using the above URL.

As you make changes and save the files, the mkdocs server notices that and re-builds the website.
It also spews out any WARNINGS or ERRORS with respect to the changes that you have just made. If
the changes look good, then go ahead and submit the PR.

### Option 3) Preview versioned docs locally with mike

Install dependencies once:

```bash
pip install -r requirements.txt
```

**Preview unmerged work on your feature branch** (CSS, `mkdocs.yml`, dropdown UI).
`make -C docs build-docs` is the wrong tool for this — it checks out remote
`origin/master` / `origin/release-*` and will not show your local PR changes.

```bash
# On your feature branch:
mike deploy --title "Master (Dev)" master
mike set-default master
mike serve
```

Open `http://localhost:8000`. For a single-version preview without mike, use
`mkdocs serve` as in Option 2.

**Mimic what CI publishes** (content from remote branches only):

```bash
make -C docs build-docs                 # all origin/release-* + origin/master
make -C docs build-docs RELEASES=master # only origin/master
mike serve
```

Push the local `gh-pages` branch only if you intend to update the remote
(CI already does this with `PUSH=1`):

```bash
make -C docs build-docs PUSH=1
```

`hack/build-docs.sh` refuses to run while checked out on `gh-pages`, and
refuses to continue if `extra.css` is dirty on a `release-*` branch. There
is no interactive override.
