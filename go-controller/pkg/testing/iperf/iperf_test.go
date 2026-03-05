package iperf

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func iperfLine(epoch int64, interval, transfer, bitrate string) string {
	return fmt.Sprintf("%d [  6] %s sec  %s  %s    0   2.00 MBytes", epoch, interval, transfer, bitrate)
}

func iperfZeroLine(epoch int64, interval string) string {
	return fmt.Sprintf("%d [  6] %s sec  0.00 Bytes  0.00 bits/sec    0   1.30 KBytes", epoch, interval)
}

func TestLogDowntime(t *testing.T) {
	baseEpoch := int64(1772800000)
	startTime := time.Unix(baseEpoch, 0)

	tests := []struct {
		name             string
		log              string
		startTime        time.Time
		expectedDowntime int
		expectErr        bool
		expectedErr      string
	}{
		{
			name: "no downtime",
			log: strings.Join([]string{
				fmt.Sprintf("%d Connecting to host 10.0.0.1, port 5201", baseEpoch),
				fmt.Sprintf("%d [ ID] Interval           Transfer     Bitrate         Retr  Cwnd", baseEpoch),
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfLine(baseEpoch+1, "1.00-2.00", "195 MBytes", "1.64 Gbits/sec"),
				iperfLine(baseEpoch+2, "2.00-3.00", "210 MBytes", "1.76 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 0,
		},
		{
			name: "1 second downtime within limit",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfLine(baseEpoch+1, "1.00-2.00", "195 MBytes", "1.64 Gbits/sec"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfLine(baseEpoch+3, "3.00-4.00", "180 MBytes", "1.51 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 1,
		},
		{
			name: "2 seconds downtime at limit",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfLine(baseEpoch+3, "3.00-4.00", "180 MBytes", "1.51 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 2,
		},
		{
			name: "5 seconds downtime exceeds limit",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfZeroLine(baseEpoch+3, "3.00-4.00"),
				iperfZeroLine(baseEpoch+4, "4.00-5.00"),
				iperfZeroLine(baseEpoch+5, "5.00-6.00"),
				iperfLine(baseEpoch+6, "6.00-7.00", "180 MBytes", "1.51 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 5,
		},
		{
			name: "downtime before startTime is ignored",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfZeroLine(baseEpoch+3, "3.00-4.00"),
				iperfZeroLine(baseEpoch+4, "4.00-5.00"),
				iperfZeroLine(baseEpoch+5, "5.00-6.00"),
				iperfLine(baseEpoch+6, "6.00-7.00", "180 MBytes", "1.51 Gbits/sec"),
				iperfLine(baseEpoch+7, "7.00-8.00", "190 MBytes", "1.59 Gbits/sec"),
			}, "\n"),
			startTime:        time.Unix(baseEpoch+6, 0),
			expectedDowntime: 0,
		},
		{
			name: "two downtime windows returns the max",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfLine(baseEpoch+3, "3.00-4.00", "180 MBytes", "1.51 Gbits/sec"),
				iperfZeroLine(baseEpoch+4, "4.00-5.00"),
				iperfZeroLine(baseEpoch+5, "5.00-6.00"),
				iperfZeroLine(baseEpoch+6, "6.00-7.00"),
				iperfZeroLine(baseEpoch+7, "7.00-8.00"),
				iperfLine(baseEpoch+8, "8.00-9.00", "190 MBytes", "1.59 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 4,
		},
		{
			name: "traffic still down returns error",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
			}, "\n"),
			startTime:   startTime,
			expectErr:   true,
			expectedErr: "traffic is still down",
		},
		{
			name: "lines without timestamps are skipped",
			log: strings.Join([]string{
				"Connecting to host 10.0.0.1, port 5201",
				"[ ID] Interval           Transfer     Bitrate         Retr  Cwnd",
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfLine(baseEpoch+1, "1.00-2.00", "195 MBytes", "1.64 Gbits/sec"),
			}, "\n"),
			startTime:        startTime,
			expectedDowntime: 0,
		},
		{
			name: "no interval after startTime yet",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfLine(baseEpoch+1, "1.00-2.00", "195 MBytes", "1.64 Gbits/sec"),
			}, "\n"),
			startTime:   time.Unix(baseEpoch+10, 0),
			expectErr:   true,
			expectedErr: "no interval after startTime",
		},
		{
			name: "only banners no intervals",
			log: strings.Join([]string{
				fmt.Sprintf("%d Connecting to host 10.0.0.1, port 5201", baseEpoch),
				fmt.Sprintf("%d [ ID] Interval           Transfer     Bitrate         Retr  Cwnd", baseEpoch),
			}, "\n"),
			startTime:   startTime,
			expectErr:   true,
			expectedErr: "no interval after startTime",
		},
		{
			name: "epoch zero startTime checks all lines",
			log: strings.Join([]string{
				iperfLine(baseEpoch, "0.00-1.00", "200 MBytes", "1.68 Gbits/sec"),
				iperfZeroLine(baseEpoch+1, "1.00-2.00"),
				iperfZeroLine(baseEpoch+2, "2.00-3.00"),
				iperfZeroLine(baseEpoch+3, "3.00-4.00"),
				iperfLine(baseEpoch+4, "4.00-5.00", "180 MBytes", "1.51 Gbits/sec"),
			}, "\n"),
			startTime:        time.Unix(0, 0).UTC(),
			expectedDowntime: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downtime, err := LogDowntime(tt.log, tt.startTime)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if tt.expectedErr != "" && !strings.Contains(err.Error(), tt.expectedErr) {
					t.Fatalf("expected error containing %q, got %v", tt.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if downtime != tt.expectedDowntime {
				t.Errorf("expected downtime %d, got %d", tt.expectedDowntime, downtime)
			}
		})
	}
}
