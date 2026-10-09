// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// iperfIntervalRegexp matches iperf3 client interval lines, with an optional
// leading Unix epoch prefix produced by `--timestamps='%s '`, for example:
//
//	1752499477 [  5]  54.00-55.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
//	[  5]  54.00-55.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
var iperfIntervalRegexp = regexp.MustCompile(`^(?:(\d+(?:\.\d+)?)\s+)?\[\s*\d+\]\s+(\d+(?:\.\d+)?)-(\d+(?:\.\d+)?)\s+sec\s+(.*)$`)

const iperfZeroThroughput = "0.00 Bytes  0.00 bits/sec"

type iperfStall struct {
	// begin and end are the interval offsets in seconds since the iperf3
	// client started.
	begin, end float64
	// beginEpoch is the Unix epoch of the first zero-throughput line of the
	// stall, zero if the log has no `--timestamps` prefix.
	beginEpoch float64
	// ongoing is true when the log ends while the stall is still in
	// progress.
	ongoing bool
}

// IperfDowntimeSummary parses iperf3 client interval output and returns a
// human readable summary of the zero-throughput stalls found in it, so
// failures report the actual outage rather than just the last log line. It
// returns an empty string when the log contains no interval lines or no
// stalls. Lines with or without the Unix epoch prefix from
// `--timestamps='%s '` are supported; the wall clock stall start is only
// reported when the prefix is present.
func IperfDowntimeSummary(iperfLog string) string {
	stalls := []iperfStall{}
	intervals := 0
	var current *iperfStall
	for _, line := range strings.Split(iperfLog, "\n") {
		match := iperfIntervalRegexp.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		intervals++
		begin, _ := strconv.ParseFloat(match[2], 64)
		end, _ := strconv.ParseFloat(match[3], 64)
		if strings.Contains(match[4], iperfZeroThroughput) {
			if current == nil {
				epoch := float64(0)
				if match[1] != "" {
					epoch, _ = strconv.ParseFloat(match[1], 64)
				}
				current = &iperfStall{begin: begin, beginEpoch: epoch}
			}
			current.end = end
			continue
		}
		if current != nil {
			stalls = append(stalls, *current)
			current = nil
		}
	}
	if current != nil {
		current.ongoing = true
		stalls = append(stalls, *current)
	}
	if intervals == 0 || len(stalls) == 0 {
		return ""
	}
	summary := []string{fmt.Sprintf("iperf3 downtime summary: %d intervals parsed, %d stall(s):", intervals, len(stalls))}
	for _, stall := range stalls {
		line := fmt.Sprintf("- zero throughput from interval %.2fs to %.2fs (%.2fs)", stall.begin, stall.end, stall.end-stall.begin)
		if stall.beginEpoch > 0 {
			line += fmt.Sprintf(", started at %s", time.Unix(int64(stall.beginEpoch), 0).UTC().Format(time.RFC3339))
		}
		if stall.ongoing {
			line += ", still ongoing at the end of the log"
		}
		summary = append(summary, line)
	}
	return strings.Join(summary, "\n")
}

// TailLines returns at most n trailing lines of s.
func TailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
