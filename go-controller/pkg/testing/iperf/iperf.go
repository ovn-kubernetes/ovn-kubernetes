package iperf

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const TrafficDownIperfLog = "0.00 Bytes  0.00 bits/sec"

// LogDowntime analyzes an iperf3 log (with epoch timestamps) and returns
// the maximum number of consecutive seconds with zero traffic since startTime.
// Lines before startTime are ignored. Lines without a valid epoch timestamp or
// without " sec " are skipped.
func LogDowntime(iperfLog string, startTime time.Time) (int, error) {
	lines := strings.Split(strings.TrimSuffix(iperfLog, "\n"), "\n")

	maxZeroTrafficLines := 0
	zeroTrafficLines := 0
	foundIntervalAfterStart := false
	lastIntervalDown := false
	for _, line := range lines {
		if !strings.Contains(line, " sec ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		epoch, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil {
			continue
		}
		lineTime := time.Unix(epoch, 0)
		if lineTime.Before(startTime) {
			continue
		}
		foundIntervalAfterStart = true
		if strings.Contains(line, TrafficDownIperfLog) {
			zeroTrafficLines++
			if zeroTrafficLines > maxZeroTrafficLines {
				maxZeroTrafficLines = zeroTrafficLines
			}
			lastIntervalDown = true
		} else {
			zeroTrafficLines = 0
			lastIntervalDown = false
		}
	}

	if !foundIntervalAfterStart {
		return 0, fmt.Errorf("no interval after startTime %s yet", startTime)
	}

	if lastIntervalDown {
		return 0, fmt.Errorf("traffic is still down")
	}

	return maxZeroTrafficLines, nil
}
