#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

"""Format coredump stack traces as one bounded, injection-safe PR comment."""

import argparse
import html
import os
import sys
import unicodedata
from pathlib import Path
from string import Template


COMMENT_MARKER = "<!-- ovn-coredump-stacktraces -->"
MAX_COMMENT_SIZE = 60_000
MAX_TRACES = 50
TRUNCATION_NOTICE = "\n\n… truncated; the full trace is in the workflow artifact."
TEMPLATE_DIR = Path(__file__).resolve().parent
COMMENT_TEMPLATE_PATH = TEMPLATE_DIR / "stacktrace-comment.md.tmpl"
SECTION_TEMPLATE_PATH = TEMPLATE_DIR / "stacktrace-section.html.tmpl"


class NoTracesError(ValueError):
    """Raised when the input contains no coredump stack traces."""


def find_traces(input_dir: Path) -> tuple[list[Path], int]:
    """Find regular trace files without following artifact-provided symlinks."""
    root = input_dir.resolve()
    traces = []
    total = 0

    for path in sorted(input_dir.rglob("*.stacktrace.txt")):
        if path.is_symlink() or not path.is_file():
            continue
        try:
            if os.path.commonpath((root, path.resolve())) != str(root):
                continue
        except (OSError, ValueError):
            continue

        total += 1
        if len(traces) < MAX_TRACES:
            traces.append(path)

    return traces, total


def sanitize_text(text: str) -> str:
    """Remove control characters that are invalid or misleading in comments."""
    return "".join(
        char
        if char in "\n\t" or unicodedata.category(char) not in {"Cc", "Cf", "Cs"}
        else "�"
        for char in text
    )


def escaped_prefix(text: str, budget: int) -> tuple[str, bool]:
    """HTML-escape the longest prefix of text that fits in budget."""
    escaped = html.escape(text)
    if len(escaped) <= budget:
        return escaped, False

    low, high = 0, len(text)
    while low < high:
        middle = (low + high + 1) // 2
        if len(html.escape(text[:middle])) <= budget:
            low = middle
        else:
            high = middle - 1
    return html.escape(text[:low]), True


def load_template(path: Path) -> Template:
    """Load a trusted comment template from the script directory."""
    return Template(path.read_text(encoding="utf-8"))


def format_trace_section(
    trace: Path, input_dir: Path, budget: int, template: Template
) -> str:
    """Format one trace in a collapsible section no larger than budget."""
    relative_name = sanitize_text(str(trace.relative_to(input_dir))[-300:])
    display_name = html.escape(relative_name)
    size = trace.stat().st_size
    values = {
        "display_name": display_name,
        "size": size,
        "section_spacing": "\n",
    }
    empty_section = template.substitute(**values, content="")
    content_budget = max(0, budget - len(empty_section))

    with trace.open("r", encoding="utf-8", errors="replace") as stream:
        text = sanitize_text(stream.read(content_budget + 1))

    escaped, escaped_was_trimmed = escaped_prefix(text, content_budget)
    if escaped_was_trimmed:
        escaped, _ = escaped_prefix(
            text, max(0, content_budget - len(html.escape(TRUNCATION_NOTICE)))
        )
        notice, _ = escaped_prefix(
            TRUNCATION_NOTICE, max(0, content_budget - len(escaped))
        )
        escaped += notice

    return template.substitute(**values, content=escaped)


def build_comment(input_dir: Path, run_id: str, run_url: str) -> str:
    traces, total_traces = find_traces(input_dir)
    if not traces:
        raise NoTracesError("no coredump stack traces found")

    comment_template = load_template(COMMENT_TEMPLATE_PATH)
    section_template = load_template(SECTION_TEMPLATE_PATH)
    omitted = total_traces - len(traces)
    omitted_notice = ""
    if omitted:
        omitted_notice = (
            f"Only the first {len(traces)} of {total_traces} traces are shown.\n\n"
        )
    values = {
        "comment_marker": COMMENT_MARKER,
        "run_id": run_id,
        "run_url": run_url,
        "omitted_notice": omitted_notice,
    }

    comment_without_traces = comment_template.substitute(
        **values, trace_sections=""
    )
    available = MAX_COMMENT_SIZE - len(comment_without_traces)
    section_budget = available // len(traces)
    sections = [
        format_trace_section(trace, input_dir, section_budget, section_template)
        for trace in traces
    ]
    comment = comment_template.substitute(
        **values, trace_sections="".join(sections)
    )
    if len(comment) > MAX_COMMENT_SIZE:
        raise ValueError("formatted comment exceeds GitHub's size limit")
    return comment


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input-dir", required=True, type=Path)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--run-url", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument(
        "--allow-empty",
        action="store_true",
        help="succeed without writing output when no stack traces are found",
    )
    args = parser.parse_args()

    try:
        comment = build_comment(args.input_dir, args.run_id, args.run_url)
    except NoTracesError as error:
        if not args.allow_empty:
            print(f"error: {error}", file=sys.stderr)
            return 1
        print(error)
        return 0
    except (KeyError, OSError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1

    args.output.write_text(comment, encoding="utf-8")
    print(f"Wrote {args.output} ({len(comment)} characters)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
