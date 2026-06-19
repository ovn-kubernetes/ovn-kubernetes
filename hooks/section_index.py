# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

"""MkDocs hook – redirect trimmed section URLs to their first child page.

Reads the nav tree at build time and writes docs/js/section-redirects.js
with a redirect map.  Two passes ensure full coverage:

  1. Section pass – each nav section (top-level and nested) maps its
     inferred directory to the section's first child page.
  2. Directory pass – any remaining directory that contains nav pages
     but wasn't covered by pass 1 gets a redirect to its first page.

No index.md files are created.
"""

import json
import logging
import os
from collections import Counter
from pathlib import PurePosixPath

log = logging.getLogger("mkdocs.hooks.section_redirect")


def on_config(config):
    """Build section redirect map and write it as a JS file."""
    docs_dir = config["docs_dir"]
    nav = config.get("nav")
    if not nav:
        return config

    redirects: dict[str, str] = {}

    _walk_nav(nav, redirects, docs_dir)

    first_in_dir: dict[str, str] = {}
    _collect_first_per_dir(nav, first_in_dir)
    for dir_path, page in first_in_dir.items():
        if dir_path == ".":
            continue
        key = "/" + dir_path
        if key in redirects:
            continue
        if os.path.exists(os.path.join(docs_dir, dir_path, "index.md")):
            continue
        redirects[key] = "/" + _md_to_url(page)

    _write_redirect_js(docs_dir, redirects)
    log.info("Section redirect map: %d entries", len(redirects))

    return config

def _walk_nav(items, redirects, docs_dir):
    """Recursively visit nav nodes and collect section redirects."""
    for item in items:
        if isinstance(item, dict):
            for _title, value in item.items():
                if isinstance(value, list):
                    _process_section(value, redirects, docs_dir)
                    _walk_nav(value, redirects, docs_dir)


def _process_section(children, redirects, docs_dir):
    """Add a redirect for a section if it has no index page."""
    section_dir = _infer_directory(children, docs_dir)
    if section_dir is None:
        return

    section_key = "/" + section_dir.replace(os.sep, "/")
    if section_key in redirects:
        return

    if os.path.exists(os.path.join(docs_dir, section_dir, "index.md")):
        return

    first = _first_page(children)
    if first is None:
        return

    redirects[section_key] = "/" + _md_to_url(first)


def _collect_first_per_dir(items, first_in_dir):
    """Walk nav tree, record the first page per directory (nav order)."""
    for item in items:
        if isinstance(item, str):
            if item == "index.md":
                continue
            d = str(PurePosixPath(item).parent)
            first_in_dir.setdefault(d, item)
        elif isinstance(item, dict):
            for v in item.values():
                if isinstance(v, str):
                    if v == "index.md":
                        continue
                    d = str(PurePosixPath(v).parent)
                    first_in_dir.setdefault(d, v)
                elif isinstance(v, list):
                    _collect_first_per_dir(v, first_in_dir)


def _infer_directory(children, docs_dir):
    """Pick the most common directory among child page paths."""
    dirs: Counter[str] = Counter()
    _collect_dirs(children, dirs)
    if not dirs:
        return None
    winner, _ = dirs.most_common(1)[0]
    if winner == ".":
        return None
    return winner


def _collect_dirs(items, dirs):
    """Recursively count the parent directory of every page in the nav."""
    for item in items:
        if isinstance(item, str):
            dirs[str(PurePosixPath(item).parent)] += 1
        elif isinstance(item, dict):
            for v in item.values():
                if isinstance(v, str):
                    dirs[str(PurePosixPath(v).parent)] += 1
                elif isinstance(v, list):
                    _collect_dirs(v, dirs)


def _first_page(items):
    """First concrete page in a nav subtree, skipping the site root index."""
    for item in items:
        if isinstance(item, str):
            if item != "index.md":
                return item
        elif isinstance(item, dict):
            for v in item.values():
                if isinstance(v, str) and v != "index.md":
                    return v
                elif isinstance(v, list):
                    result = _first_page(v)
                    if result:
                        return result
    return None


def _md_to_url(md_path):
    """Convert a docs-relative .md path to its URL form."""
    path = md_path.replace(os.sep, "/")
    if path.endswith("/index.md"):
        return path[: -len("index.md")]
    if path == "index.md":
        return ""
    if path.endswith(".md"):
        return path[:-3] + "/"
    return path


def _write_redirect_js(docs_dir, redirects):
    """Write the redirect map as a self-executing JS file (skip if unchanged)."""
    js_dir = os.path.join(docs_dir, "js")
    os.makedirs(js_dir, exist_ok=True)
    js_path = os.path.join(js_dir, "section-redirects.js")
    content = _build_js(redirects)

    try:
        with open(js_path, "r", encoding="utf-8") as fh:
            if fh.read() == content:
                return
    except FileNotFoundError:
        pass

    with open(js_path, "w", encoding="utf-8") as fh:
        fh.write(content)


def _build_js(redirects):
    """Build the JS source containing the redirect map."""
    entries = []
    for key in sorted(redirects, key=lambda k: -len(k)):
        entries.append(f"    {json.dumps(key)}: {json.dumps(redirects[key])}")

    return (
        "// Auto-generated by hooks/section_index.py — do not edit.\n"
        "(function () {\n"
        "  var map = {\n"
        + ",\n".join(entries) + "\n"
        "  };\n"
        "  var p = window.location.pathname.replace(/\\/$/, '');\n"
        "  var keys = Object.keys(map);\n"
        "  for (var i = 0; i < keys.length; i++) {\n"
        "    var k = keys[i];\n"
        "    if (p === k || (p.endsWith(k)"
        " && !p.slice(0, p.length - k.length).endsWith(k.slice(1)))) {\n"
        "      var prefix = p.slice(0, p.length - k.length);\n"
        "      window.location.replace(window.location.origin + prefix + map[k]);\n"
        "      return;\n"
        "    }\n"
        "  }\n"
        "})();\n"
    )
