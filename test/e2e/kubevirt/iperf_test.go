// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"strings"
	"testing"
)

const iperfLogWithStall = `1752499420 [  5]  50.00-51.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
1752499421 [  5]  51.00-52.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
1752499422 [  5]  52.00-53.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
1752499423 [  5]  53.00-54.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
1752499424 [  5]  54.00-55.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
`

const iperfLogWithRecoveredStall = `[  5]  50.00-51.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
[  5]  51.00-52.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
[  5]  52.00-53.00  sec  0.00 Bytes  0.00 bits/sec    0   3.93 MBytes
[  5]  53.00-54.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
`

const iperfLogHealthy = `1752499420 [  5]  50.00-51.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
1752499421 [  5]  51.00-52.00  sec  1.25 MBytes  10.5 Mbits/sec    0   3.93 MBytes
`

func TestIperfDowntimeSummaryOngoingStall(t *testing.T) {
	summary := IperfDowntimeSummary(iperfLogWithStall)
	for _, expected := range []string{
		"5 intervals parsed, 1 stall(s)",
		"from interval 52.00s to 55.00s (3.00s)",
		"started at 2025-07-14T13:23:42Z",
		"still ongoing at the end of the log",
	} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("expected summary to contain %q, got:\n%s", expected, summary)
		}
	}
}

func TestIperfDowntimeSummaryRecoveredStallWithoutTimestamps(t *testing.T) {
	summary := IperfDowntimeSummary(iperfLogWithRecoveredStall)
	for _, expected := range []string{
		"4 intervals parsed, 1 stall(s)",
		"from interval 51.00s to 53.00s (2.00s)",
	} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("expected summary to contain %q, got:\n%s", expected, summary)
		}
	}
	for _, unexpected := range []string{"started at", "ongoing"} {
		if strings.Contains(summary, unexpected) {
			t.Fatalf("expected summary to not contain %q, got:\n%s", unexpected, summary)
		}
	}
}

func TestIperfDowntimeSummaryHealthyLog(t *testing.T) {
	if summary := IperfDowntimeSummary(iperfLogHealthy); summary != "" {
		t.Fatalf("expected empty summary for healthy log, got:\n%s", summary)
	}
	if summary := IperfDowntimeSummary("iperf3: error - unable to connect"); summary != "" {
		t.Fatalf("expected empty summary for non interval log, got:\n%s", summary)
	}
}

func TestTailLines(t *testing.T) {
	if tail := TailLines("a\nb\nc\n", 2); tail != "b\nc" {
		t.Fatalf("expected tail to be 'b\\nc', got %q", tail)
	}
	if tail := TailLines("a\nb", 5); tail != "a\nb" {
		t.Fatalf("expected tail to be 'a\\nb', got %q", tail)
	}
}
