#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Summarize the Ginkgo JSON reports of several e2e lanes: which specs no lane
# executed and which specs only passed on retry. Writes Markdown to
# GITHUB_STEP_SUMMARY when set, otherwise to stdout.
#
# Usage: e2e-report-summary.sh [--strict] <reports-dir>
#   --strict  exit 1 when at least one spec was executed by no lane

set -eu

strict=false
if [ "${1:-}" = "--strict" ]; then
	strict=true
	shift
fi
reports_dir=${1:?usage: $0 [--strict] <reports-dir>}
out=${GITHUB_STEP_SUMMARY:-/dev/stdout}

specs=$(mktemp)
trap 'rm -f "${specs}"' EXIT

# One line per spec and lane: lane, state, attempts, spec name. The lane is
# the report file name without the "_report.json" suffix that e2e-cp.sh adds
# to the JOB_NAME.
find "${reports_dir}" -type f -name '*_report.json' | sort | while read -r report; do
	jq -r --arg lane "$(basename "${report}" _report.json)" '
		.[] | .SpecReports[]? | select(.LeafNodeType == "It")
		| [$lane, .State, (.NumAttempts // 0),
		   ((.ContainerHierarchyTexts + [.LeafNodeText]) | join(" "))]
		| @tsv' "${report}"
done > "${specs}"

if [ ! -s "${specs}" ]; then
	echo "No Ginkgo JSON reports found under ${reports_dir}" >> "${out}"
	exit 0
fi

lanes=$(cut -f1 "${specs}" | sort -u | wc -l)
all=$(cut -f4 "${specs}" | sort -u)
executed=$(awk -F'\t' '$2 != "skipped" && $2 != "pending" { print $4 }' "${specs}" | sort -u)
never=$(comm -23 <(printf '%s\n' "${all}") <(printf '%s\n' "${executed}"))
flaky=$(awk -F'\t' '$2 == "passed" && $3 > 1 { print "| " $1 " | " $3 " | " $4 " |" }' "${specs}" | sort)

{
	echo "## e2e spec coverage"
	echo
	echo "| Lanes | Specs | Executed by at least one lane | Executed by no lane |"
	echo "|---|---|---|---|"
	echo "| ${lanes} | $(printf '%s\n' "${all}" | grep -c .) | $(printf '%s\n' "${executed}" | grep -c .) | $(printf '%s\n' "${never}" | grep -c .) |"
	if [ -n "${never}" ]; then
		echo
		echo "### Specs executed by no lane"
		echo
		printf '%s\n' "${never}" | sed 's/^/- /'
	fi
	if [ -n "${flaky}" ]; then
		echo
		echo "### Specs that passed only on retry"
		echo
		echo "| Lane | Attempts | Spec |"
		echo "|---|---|---|"
		printf '%s\n' "${flaky}"
	fi
} >> "${out}"

if [ "${strict}" = true ] && [ -n "${never}" ]; then
	exit 1
fi
