#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

"""Lint Markdown docs for incorrect ovn-kubernetes.io link conventions.

Exits 0 when every link follows the conventions, 1 otherwise.

Conventions (documented in docs/developer-guide/documentation.md,
"Linking between docs pages"):

  docs/ files (except governance symlinks to the repo root)
      Cross-reference other docs pages with relative Markdown paths.
      Do NOT use https://ovn-kubernetes.io/… URLs for in-docs links.

  Governance symlinks (repo-root files served via docs/governance/)
  and other repository Markdown files
      Deep-links into the published site MUST include a version prefix
      (master/, latest/, or a release number like 1.3/).

  A bare homepage link — https://ovn-kubernetes.io or
  https://ovn-kubernetes.io/ with nothing after the slash — is always
  fine everywhere.

Links inside fenced code blocks and inline code spans are ignored
(they are documentation examples, and MagicLink does not auto-link
inline code).
"""

import re
import sys
from pathlib import Path

# Directory name components skipped when collecting repository Markdown files.
_SKIP_DIR_NAMES = frozenset(
    {
        ".git",
        ".venv",
        "vendor",
        "node_modules",
        "__pycache__",
    }
)

# Inline Markdown link: [text](https://ovn-kubernetes.io/…)
# Also matches angle-bracket form: [text](<https://ovn-kubernetes.io/…>)
_INLINE_LINK_RE = re.compile(
    r"\]\(\s*<?(https?://(?:www\.)?ovn-kubernetes\.io(?:/[^)\s>]*)?)>?"
)

# Reference-style link definition: [label]: https://ovn-kubernetes.io/…
# Also matches angle-bracket form: [label]: <https://ovn-kubernetes.io/…>
# Space after the colon is optional (Python-Markdown accepts both).
_REF_DEF_RE = re.compile(
    r"^\s{0,3}\[.*\]:\s*<?(https?://(?:www\.)?ovn-kubernetes\.io(?:/[^\s>]*)?)>?",
    re.MULTILINE,
)

# Autolink: <https://ovn-kubernetes.io/…>
_AUTOLINK_RE = re.compile(
    r"<(https?://(?:www\.)?ovn-kubernetes\.io(?:/[^>\s]*)?)>"
)

# Bare URL turned into a link by pymdownx.magiclink. Stop at whitespace and
# Markdown delimiters so we do not run past ]( in [text](url) forms.
_BARE_URL_RE = re.compile(
    r"https?://(?:www\.)?ovn-kubernetes\.io(?:/[^\s<>\[\]()\"'`]*)?"
)

# Inline code spans: `…`, ``…``, etc. (CommonMark-style fence matching).
_INLINE_CODE_RE = re.compile(r"(?<!`)(`+)(?!`)(?:.+?)(?<!`)\1(?!`)")

# Trailing punctuation MagicLink typically leaves outside the link.
_BARE_URL_TRAIL_PUNCT = ".,;:!?)]}'\""

# Versioned path prefix: /master/…, /latest/…, /1.3/… etc
_VERSION_PREFIX_RE = re.compile(r"^/(master|latest|\d+\.\d+)(/|$)")

# Fenced code block delimiter
_FENCE_OPEN_RE = re.compile(r"^(\s*(`{3,}|~{3,}))")


def _inline_code_spans(line):
    """Return ``(start, end)`` spans of inline code on a single line."""
    return [m.span() for m in _INLINE_CODE_RE.finditer(line)]


def _span_inside(span, covering):
    """True when *span* lies entirely inside any span in *covering*."""
    start, end = span
    for cs, ce in covering:
        if start >= cs and end <= ce:
            return True
    return False


def _spans_overlap(a, b):
    """True when half-open spans *a* and *b* overlap."""
    return a[0] < b[1] and b[0] < a[1]


def _fenced_line_set(lines):
    """Return the set of 0-based line indices that are inside fenced code blocks."""
    inside = set()
    in_fence = False
    fence_char = None
    fence_indent = 0
    fence_len = 0
    for idx, line in enumerate(lines):
        stripped = line.lstrip()
        m = _FENCE_OPEN_RE.match(line)
        if m:
            char = m.group(2)[0]
            length = len(m.group(2))
            indent = len(line) - len(stripped)
            if not in_fence:
                in_fence = True
                fence_char = char
                fence_indent = indent
                fence_len = length
                continue
            if char == fence_char and length >= fence_len and indent <= fence_indent:
                in_fence = False
                continue
        if in_fence:
            inside.add(idx)
    return inside


def _extract_site_path(url):
    """Return the path portion after the hostname, or None for a bare homepage"""
    remainder = re.sub(r"^https?://(?:www\.)?ovn-kubernetes\.io", "", url)
    if not remainder or remainder == "/":
        return None
    return remainder


def _is_governance_symlink(filepath, docs_dir):
    """True when filepath is a symlink under docs/governance/ pointing outside docs/."""
    governance_dir = docs_dir / "governance"
    try:
        filepath.relative_to(governance_dir)
    except ValueError:
        return False
    if not filepath.is_symlink():
        return False
    resolved = filepath.resolve()
    try:
        resolved.relative_to(docs_dir.resolve())
        return False
    except ValueError:
        return True


def _should_skip_path(filepath, repo_root):
    """True when *filepath* should not be linted (vendored / generated trees)."""
    try:
        rel_parts = filepath.resolve().relative_to(repo_root.resolve()).parts
    except ValueError:
        return True
    if any(part in _SKIP_DIR_NAMES for part in rel_parts):
        return True
    # Third-party license copies under LICENSES/packages/
    if len(rel_parts) >= 2 and rel_parts[0] == "LICENSES" and rel_parts[1] == "packages":
        return True
    return False


def _collect_targets(repo_root):
    """Return Markdown paths to lint, one entry per resolved file.

    Prefer a non-symlink path when the same file is also reached via a
    docs/governance/ symlink so each shared file is reported once.
    """
    chosen = {}
    for md in sorted(repo_root.rglob("*.md")):
        if _should_skip_path(md, repo_root):
            continue
        resolved = md.resolve()
        existing = chosen.get(resolved)
        if existing is None:
            chosen[resolved] = md
            continue
        # Prefer the real file over a symlink (e.g. root CONTRIBUTING.md
        # over docs/governance/CONTRIBUTING.md).
        if existing.is_symlink() and not md.is_symlink():
            chosen[resolved] = md
    return sorted(chosen.values(), key=lambda p: str(p.relative_to(repo_root)))


def _link_matches(line):
    """Yield ``(span, url)`` for site URLs on *line*.

    Skips inline code spans. Structured Markdown links (inline, reference,
    autolink) take priority; bare/MagicLink matches that overlap them are
    suppressed so a complete autolink destination is not also reported as a
    truncated bare URL.
    """
    code_spans = _inline_code_spans(line)
    matches = []
    seen_spans = set()

    for m in (
        list(_INLINE_LINK_RE.finditer(line))
        + list(_REF_DEF_RE.finditer(line))
        + list(_AUTOLINK_RE.finditer(line))
    ):
        span = m.span(1)
        if span in seen_spans or _span_inside(span, code_spans):
            continue
        seen_spans.add(span)
        matches.append((span, m.group(1)))

    for m in _BARE_URL_RE.finditer(line):
        raw = m.group(0)
        url = raw.rstrip(_BARE_URL_TRAIL_PUNCT)
        if not url:
            continue
        span = (m.start(), m.start() + len(url))
        if span in seen_spans or _span_inside(span, code_spans):
            continue
        if any(_spans_overlap(span, existing) for existing, _ in matches):
            continue
        seen_spans.add(span)
        matches.append((span, url))

    return matches


def lint_file(filepath, repo_root):
    """Return a list of ``(line_number, message)`` tuples for violations."""
    docs_dir = repo_root / "docs"
    errors = []

    try:
        text = filepath.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        return [(0, f"cannot read file: {exc}")]

    lines = text.splitlines()
    fenced = _fenced_line_set(lines)

    # Determine which rule set applies.
    resolved = filepath.resolve()
    try:
        resolved.relative_to(docs_dir.resolve())
        is_docs_file = True
    except ValueError:
        is_docs_file = False

    is_gov_symlink = _is_governance_symlink(filepath, docs_dir)

    require_relative = is_docs_file and not is_gov_symlink

    for idx, line in enumerate(lines):
        if idx in fenced:
            continue
        for span, url in _link_matches(line):
            site_path = _extract_site_path(url)
            if site_path is None:
                continue

            if require_relative:
                errors.append(
                    (idx + 1, f"use a relative Markdown path instead of {url}")
                )
            else:
                if not _VERSION_PREFIX_RE.match(site_path):
                    errors.append(
                        (
                            idx + 1,
                            f"use a versioned URL (e.g. /master/…) instead of {url}",
                        )
                    )

    return errors


def main(argv=None):
    repo_root = Path(__file__).resolve().parent.parent
    targets = _collect_targets(repo_root)

    total_errors = 0
    for filepath in targets:
        errors = lint_file(filepath, repo_root)
        for lineno, msg in errors:
            relpath = filepath.relative_to(repo_root)
            print(f"{relpath}:{lineno}: {msg}")
            total_errors += 1

    if total_errors:
        print(f"\n{total_errors} link convention violation(s) found.")
        print(
            'See docs/developer-guide/documentation.md, "Linking between docs pages".'
        )
        return 1

    print("All docs links follow conventions. ✓")
    return 0


if __name__ == "__main__":
    sys.exit(main())
