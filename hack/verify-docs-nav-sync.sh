#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that docs/, mkdocs.yml nav:, and section index.md card grids stay
# in sync. Intended to run in CI (docs.yml) so contributors get fast feedback
# when a new page is added without updating all three.
#
# Checks performed:
#   1. Every .md path in mkdocs.yml nav: must exist on disk.
#   2. Every .md file under docs/ must appear in nav:, unless it matches an
#      intentional exclusion (blog/, templates/stubs, cross-section landing
#      pages not listed as nav roots, or mkdocs not_in_nav patterns).
#   3. Every nav entry must be referenced in its navigation section's
#      card-grid landing page.  The section is determined by the nav
#      hierarchy, not the filesystem directory.  Section landing pages
#      with nav children must contain a landing-grid.
#   4. Nested nav pages (under a sub-directory of their section) must be
#      reachable via a markdown link from a sub-section overview, parent
#      page, or the section landing page.
#
# Entries under nav sections that use the root index.md (e.g. Overview)
# have no nav-based section.  For these, the script falls back to the
# filesystem directory's landing page (e.g. design/index.md) and fails
# if that landing page is missing or has no card grid.
#
# Exits 0 when everything is in sync, 1 on mismatches.
#
# Requires Bash 4.0+ (associative arrays, mapfile) and Python 3
# (portable relative-path computation and HTML/code-fence stripping).
# On macOS, install a recent Bash via Homebrew: brew install bash
#
# Usage (from repo root):
#   ./hack/verify-docs-nav-sync.sh

set -euo pipefail

if [[ "${BASH_VERSINFO[0]}" -lt 4 ]]; then
  echo "ERROR: This script requires Bash 4.0 or later (for associative arrays and mapfile)." >&2
  echo "On macOS, install a recent Bash via Homebrew: brew install bash" >&2
  exit 1
fi

# True if the file contains a class attribute with the given whitespace-
# separated token (e.g. landing-grid in class="landing-grid wide").
# Ignores HTML comments and fenced code blocks.
has_class_token() {
  python3 - "$1" "$2" <<'PY'
import re, sys

def strip_noise(text):
    text = re.sub(r"<!--.*?-->", "", text, flags=re.S)
    text = re.sub(r"^```[^\n]*\n.*?^```[ \t]*$", "", text, flags=re.M | re.S)
    text = re.sub(r"^~~~[^\n]*\n.*?^~~~[ \t]*$", "", text, flags=re.M | re.S)
    return text

path, token = sys.argv[1], sys.argv[2]
text = strip_noise(open(path, encoding="utf-8").read())
# Require a real class= attribute, not data-class= / ng-class= / etc.
pat = re.compile(r'(?<![\w-])class\s*=\s*["\']([^"\']+)["\']', flags=re.I)
for m in pat.finditer(text):
    if token in m.group(1).split():
        sys.exit(0)
sys.exit(1)
PY
}

# Extract markdown link targets from landing-card blocks inside the first
# landing-grid only.  Ignores HTML comments and fenced code.  Prints one
# absolute normalized path per line (resolved against the index file dir).
landing_card_targets() {
  python3 - "$1" <<'PY'
import os, re, sys

def strip_noise(text):
    text = re.sub(r"<!--.*?-->", "", text, flags=re.S)
    text = re.sub(r"^```[^\n]*\n.*?^```[ \t]*$", "", text, flags=re.M | re.S)
    text = re.sub(r"^~~~[^\n]*\n.*?^~~~[ \t]*$", "", text, flags=re.M | re.S)
    return text

def class_has_token(class_value, token):
    return token in class_value.split()

path = sys.argv[1]
page_dir = os.path.dirname(path)
text = strip_noise(open(path, encoding="utf-8").read())

div_open = re.compile(r"<div\b[^>]*>", flags=re.I | re.S)
div_close = re.compile(r"</div>", flags=re.I)
# Require a real class= attribute, not data-class= / ng-class= / etc.
class_attr = re.compile(r'(?<![\w-])class\s*=\s*["\']([^"\']+)["\']', flags=re.I)

# Find the opening <div> whose class list contains landing-grid.
grid_open = None
for m in div_open.finditer(text):
    cm = class_attr.search(m.group(0))
    if cm and class_has_token(cm.group(1), "landing-grid"):
        grid_open = m
        break
if grid_open is None:
    sys.exit(0)

# Slice only the interior of that grid by tracking <div>/</div> depth.
pos = grid_open.end()
depth = 1
grid_end = len(text)
while depth > 0 and pos < len(text):
    no = div_open.search(text, pos)
    nc = div_close.search(text, pos)
    if nc is None:
        break
    if no is not None and no.start() < nc.start():
        depth += 1
        pos = no.end()
    else:
        depth -= 1
        if depth == 0:
            grid_end = nc.start()
            break
        pos = nc.end()

grid_text = text[grid_open.end():grid_end]

# Collect markdown links inside landing-card blocks within the grid.
pos = 0
while True:
    m = div_open.search(grid_text, pos)
    if not m:
        break
    cm = class_attr.search(m.group(0))
    if not cm or not class_has_token(cm.group(1), "landing-card"):
        pos = m.end()
        continue
    # Match this card's closing </div> with depth tracking.
    cpos = m.end()
    cdepth = 1
    card_end = len(grid_text)
    while cdepth > 0 and cpos < len(grid_text):
        cno = div_open.search(grid_text, cpos)
        cnc = div_close.search(grid_text, cpos)
        if cnc is None:
            break
        if cno is not None and cno.start() < cnc.start():
            cdepth += 1
            cpos = cno.end()
        else:
            cdepth -= 1
            if cdepth == 0:
                card_end = cnc.start()
                break
            cpos = cnc.end()
    card = grid_text[m.end():card_end]
    for link in re.finditer(r"(?<!!)\[([^\]]*)\]\(([^)]+)\)", card):
        target = link.group(2).strip().split()[0]
        target = target.split("#", 1)[0]
        if not target or target.startswith(("http://", "https://", "mailto:")):
            continue
        resolved = os.path.normpath(os.path.join(page_dir, target))
        print(resolved)
    pos = card_end + len("</div>")
PY
}

# Return 0 if page_file contains a navigable markdown link that resolves to
# target_file.  Ignores HTML comments, fenced code, and image syntax.
# Both arguments are absolute filesystem paths.
page_links_to() {
  python3 - "$1" "$2" <<'PY'
import os, re, sys

def strip_noise(text):
    text = re.sub(r"<!--.*?-->", "", text, flags=re.S)
    text = re.sub(r"^```[^\n]*\n.*?^```[ \t]*$", "", text, flags=re.M | re.S)
    text = re.sub(r"^~~~[^\n]*\n.*?^~~~[ \t]*$", "", text, flags=re.M | re.S)
    # Drop image syntax so ![alt](page.md) is not treated as a link.
    text = re.sub(r"!\[[^\]]*\]\([^)]*\)", "", text)
    # Drop inline code spans so `[text](page.md)` examples are not links.
    text = re.sub(r"`[^`\n]+`", "", text)
    return text

page, target = sys.argv[1], sys.argv[2]
page_dir = os.path.dirname(page)
target_norm = os.path.normpath(target)

text = strip_noise(open(page, encoding="utf-8").read())

found = False
for m in re.finditer(r"(?<!!)\[([^\]]*)\]\(([^)]+)\)", text):
    href = m.group(2).strip().split()[0]
    href_path = href.split("#", 1)[0]
    if not href_path or href_path.startswith(("http://", "https://", "mailto:", "#")):
        continue
    resolved = os.path.normpath(os.path.join(page_dir, href_path))
    if resolved == target_norm:
        found = True
        break
sys.exit(0 if found else 1)
PY
}

# Extract a .md path from a nav YAML line.  Supports "Title: path.md" and
# bare "- path.md", with optional quotes and trailing inline comments.
# Returns 0 on success, 1 if no path, 2 if the path contains whitespace
# (caller must fail the overall check — do not call via bare $(...) alone).
extract_nav_md_path() {
  local line="$1"
  local md_path=""

  # Drop trailing inline comments (space/tab then # ...).
  if [[ "$line" =~ ^(.*)[[:blank:]]+#.*$ ]]; then
    line="${BASH_REMATCH[1]}"
  fi
  # Trim trailing whitespace.
  line="${line%"${line##*[![:space:]]}"}"

  if [[ "$line" =~ :[[:space:]]+[\"\']?([^\"\']+\.md)[\"\']?[[:space:]]*$ ]]; then
    md_path="${BASH_REMATCH[1]}"
  elif [[ "$line" =~ ^[[:space:]]*-[[:space:]]+[\"\']?([^\"\']+\.md)[\"\']?[[:space:]]*$ ]]; then
    md_path="${BASH_REMATCH[1]}"
  else
    return 1
  fi

  if [[ "$md_path" =~ [[:space:]] ]]; then
    echo "ERROR: mkdocs.yml nav: path contains whitespace: '${md_path}'" >&2
    echo "Rename the file or quote/encode the path without spaces." >&2
    return 2
  fi

  printf '%s' "$md_path"
  return 0
}

# True if a docs-relative path is intentionally excluded from the
# "must appear in nav" check.
is_nav_exclusion() {
  local rel="$1"

  # Material blog plugin manages these pages.
  [[ "$rel" == blog/* ]] && return 0

  # Cross-section card-grid landing pages used as fallbacks / mirrors
  # but not listed as their own nav section roots.
  case "$rel" in
    design/index.md|installation/index.md|governance/index.md|ci/index.md)
      return 0
      ;;
  esac

  # Templates and empty/legacy stubs kept on disk but not published in nav.
  case "$rel" in
    features/template.md|\
    okeps/okep-4368-template.md|\
    developer-guide/testing.md|\
    getting-started/building-ovn-kubernetes.md|\
    getting-started/example-pod-creation.md|\
    getting-started/example-service-creation.md|\
    getting-started/launching-ovn-kubernetes.md|\
    getting-started/overview.md|\
    getting-started/running-release.md|\
    getting-started/running-rpm.md)
      return 0
      ;;
  esac

  # Honour mkdocs not_in_nav entries when present (one path/glob per line).
  if [[ -n "${NOT_IN_NAV_PATTERNS:-}" ]]; then
    local pat
    while IFS= read -r pat; do
      [[ -z "$pat" ]] && continue
      # shellcheck disable=SC2254
      case "$rel" in
        $pat) return 0 ;;
      esac
    done <<< "${NOT_IN_NAV_PATTERNS}"
  fi

  return 1
}

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MKDOCS_YML="${ROOT_DIR}/mkdocs.yml"
DOCS_DIR="${ROOT_DIR}/docs"

errors=0

# Optional not_in_nav block from mkdocs.yml (paths relative to docs/).
NOT_IN_NAV_PATTERNS="$(
  awk '
    /^not_in_nav:[[:space:]]*$/ {inblock=1; next}
    inblock && /^[^[:space:]#]/ {exit}
    inblock && /^[[:space:]]+/ {
      line=$0
      sub(/^[[:space:]]+/, "", line)
      sub(/#.*/, "", line)
      gsub(/[[:space:]]+$/, "", line)
      gsub(/^["'\'']|["'\'']$/, "", line)
      if (length(line)) print line
    }
  ' "${MKDOCS_YML}"
)" || true

# ── 1. Extract every .md path from non-commented nav: lines ─────────────────
# The range /^nav:/,/^[a-zA-Z]/ captures from nav: up to (and including) the
# next top-level YAML key, which contains no .md paths so the result is safe.
# This avoids breaking when nav items start at column zero (e.g. "- Home:").
nav_block=$(sed -n '/^nav:/,/^[a-zA-Z]/p' "${MKDOCS_YML}" | grep -v '^\s*#' || true)

declare -A in_nav=()
nav_files=()
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  md_path=""
  extract_rc=0
  md_path="$(extract_nav_md_path "$line")" || extract_rc=$?
  if [[ "${extract_rc}" -eq 2 ]]; then
    errors=$((errors + 1))
    continue
  elif [[ "${extract_rc}" -ne 0 ]]; then
    continue
  fi
  if [[ -z "${in_nav[$md_path]+x}" ]]; then
    nav_files+=("$md_path")
    in_nav["$md_path"]=1
  fi
done <<< "$nav_block"

# ── 2. Every nav .md must exist on disk ──────────────────────────────────────
for nav_entry in "${nav_files[@]}"; do
  if [[ ! -f "${DOCS_DIR}/${nav_entry}" ]]; then
    echo "ERROR: mkdocs.yml nav: references '${nav_entry}' but docs/${nav_entry} does not exist" >&2
    errors=$((errors + 1))
  fi
done

# ── 2b. Every docs .md must be in nav: (unless intentionally excluded) ───────
while IFS= read -r abs; do
  rel="${abs#"${DOCS_DIR}/"}"
  [[ -n "${in_nav[$rel]+x}" ]] && continue
  if is_nav_exclusion "$rel"; then
    continue
  fi
  echo "ERROR: 'docs/${rel}' exists on disk but is not listed in mkdocs.yml nav:" >&2
  echo "  Add it to nav: and the section card grid, or add an intentional exclusion." >&2
  errors=$((errors + 1))
done < <(find "${DOCS_DIR}" \( -type f -o -type l \) -name '*.md' | sort)

# ── 3. Every nav entry must appear in its navigation section's card-grid ─────
#
# Determine which section each entry belongs to based on the navigation
# hierarchy rather than the filesystem directory.  A section is identified
# by its index.md landing page.  Cross-directory entries (e.g.
# installation/*.md under "Getting Started") are checked against the nav
# section's landing page.

# Track the active section (index.md) at each indentation level.
declare -A level_section
# Collect children for each section's index.md.
declare -A section_children
# Nested pages that skip the section card check but must still be reachable.
declare -a nested_pages=()
declare -a nested_page_sections=()

nav_body=$(echo "${nav_block}" | tail -n +2)

while IFS= read -r line; do
  [[ -z "$line" ]] && continue

  # Calculate indentation.
  stripped="${line#"${line%%[![:space:]]*}"}"
  indent=$(( ${#line} - ${#stripped} ))

  md_path=""
  extract_rc=0
  md_path="$(extract_nav_md_path "$line")" || extract_rc=$?
  if [[ "${extract_rc}" -eq 2 ]]; then
    errors=$((errors + 1))
    continue
  elif [[ "${extract_rc}" -ne 0 ]]; then
    # Section header (no .md path) — clear deeper sections so that
    # sibling sections with different child indentation levels don't
    # inherit each other's level_section entries.
    for key in "${!level_section[@]}"; do
      [[ $key -gt $indent ]] && unset "level_section[$key]"
    done
    continue
  fi

  # Skip root index and blog posts.
  [[ "${md_path}" == "index.md" ]] && continue
  [[ "${md_path}" == blog/* ]] && continue

  if [[ "$(basename "$md_path")" == "index.md" ]]; then
    # New section at this level — clear this level and deeper.
    for key in "${!level_section[@]}"; do
      [[ $key -ge $indent ]] && unset "level_section[$key]"
    done

    # Sub-section index.md is a child of its parent section.
    best_key=-1
    parent_section=""
    for key in "${!level_section[@]}"; do
      if [[ $key -lt $indent && $key -gt $best_key ]]; then
        parent_section="${level_section[$key]}"
        best_key=$key
      fi
    done
    if [[ -n "$parent_section" ]]; then
      section_children["$parent_section"]+=" ${md_path}"
    fi

    level_section[$indent]="$md_path"
  else
    # Find the section at this or the closest shallower indent level,
    # handling entries under sub-headings without their own index.md.
    best_key=-1
    section=""
    for key in "${!level_section[@]}"; do
      if [[ $key -le $indent && $key -gt $best_key ]]; then
        section="${level_section[$key]}"
        best_key=$key
      fi
    done

    if [[ -n "$section" ]]; then
      section_dir="$(dirname "$section")"
      child_dir="$(dirname "$md_path")"

      # Nested under the section directory: require reachability instead
      # of an individual section card.  Still register the section so the
      # landing-grid check runs even when every child is nested.
      if [[ "$child_dir" != "$section_dir" && "$child_dir" == "$section_dir"/* ]]; then
        section_children["$section"]="${section_children[$section]-}"
        nested_pages+=("$md_path")
        nested_page_sections+=("$section")
        continue
      fi

      section_children["$section"]+=" ${md_path}"
    else
      # No nav section found (e.g. entries under "Overview" which uses
      # the root index.md).  Fall back to the filesystem directory's
      # landing page; require that page to exist and have a card grid.
      child_dir="$(dirname "$md_path")"
      fallback_index="${child_dir}/index.md"
      if [[ ! -f "${DOCS_DIR}/${fallback_index}" ]]; then
        echo "ERROR: '${md_path}' is under Overview/no-section nav but docs/${fallback_index} does not exist" >&2
        errors=$((errors + 1))
      elif ! has_class_token "${DOCS_DIR}/${fallback_index}" "landing-grid"; then
        echo "ERROR: '${md_path}' is under Overview/no-section nav but docs/${fallback_index} has no landing-grid" >&2
        errors=$((errors + 1))
      else
        section_children["$fallback_index"]+=" ${md_path}"
      fi
    fi
  fi
done <<< "$nav_body"

# Validate each section's card grid contains all its children.
for section_index in "${!section_children[@]}"; do
  index_file="${DOCS_DIR}/${section_index}"
  [[ -f "$index_file" ]] || continue

  if ! has_class_token "$index_file" "landing-grid"; then
    echo "ERROR: section landing page 'docs/${section_index}' has nav children but no landing-grid markup" >&2
    errors=$((errors + 1))
    continue
  fi

  mapfile -t card_targets < <(landing_card_targets "$index_file")

  for child in ${section_children[$section_index]}; do
    child_abs="$(python3 -c "import os.path,sys; print(os.path.normpath(sys.argv[1]))" "${DOCS_DIR}/${child}")"

    found=0
    for t in "${card_targets[@]:-}"; do
      if [[ "$t" == "$child_abs" ]]; then
        found=1
        break
      fi
    done

    if [[ "$found" -eq 0 ]]; then
      echo "ERROR: '${child}' is in mkdocs.yml nav: but not referenced in a landing-card of docs/${section_index}" >&2
      errors=$((errors + 1))
    fi
  done
done

# ── 4. Nested nav pages must be reachable from a parent overview ─────────────
for i in "${!nested_pages[@]}"; do
  nested="${nested_pages[$i]}"
  section="${nested_page_sections[$i]}"
  section_dir="$(dirname "$section")"
  child_dir="$(dirname "$nested")"
  nested_abs="${DOCS_DIR}/${nested}"

  reachable=0

  # Walk ancestor directories from the nested dir up to the section dir
  # and check nav pages / landing pages for a markdown link.
  check_dirs=("${child_dir}")
  d="${child_dir}"
  while [[ "$d" != "$section_dir" && "$d" != "." && "$d" != "/" ]]; do
    d="$(dirname "$d")"
    check_dirs+=("$d")
  done

  # Only accept links from a sub-section overview or the section landing
  # page — not from arbitrary sibling nested pages.  Overview names:
  #   ${d}/index.md, ${d}.md, or ${d}/$(basename ${d}).md
  # (e.g. observability/metrics.md or okep-dir/okep-dir.md).
  for d in "${check_dirs[@]}"; do
    base="$(basename "$d")"
    for cand in "${d}/index.md" "${d}.md" "${d}/${base}.md"; do
      [[ "$cand" == "$nested" ]] && continue
      [[ -f "${DOCS_DIR}/${cand}" ]] || continue
      if page_links_to "${DOCS_DIR}/${cand}" "${nested_abs}"; then
        reachable=1
        break 2
      fi
    done
  done

  # Section landing page is always a candidate.
  if [[ "$reachable" -eq 0 ]] && [[ -f "${DOCS_DIR}/${section}" ]]; then
    if page_links_to "${DOCS_DIR}/${section}" "${nested_abs}"; then
      reachable=1
    fi
  fi

  if [[ "$reachable" -eq 0 ]]; then
    echo "ERROR: nested nav page '${nested}' is not reachable from a sub-section index.md, parent/overview page, or section landing page link" >&2
    errors=$((errors + 1))
  fi
done

if [[ "${errors}" -gt 0 ]]; then
  echo >&2
  echo "Found ${errors} docs navigation sync error(s)." >&2
  echo "When adding a new page, update both mkdocs.yml nav: AND the section index.md card grid." >&2
  echo "See docs/developer-guide/documentation.md for details." >&2
  exit 1
fi

echo "docs nav sync check passed."
