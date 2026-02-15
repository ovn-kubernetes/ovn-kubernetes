package util

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

var benchmarkFlowBytesSink int64
var benchmarkFlowCountSink int

func BenchmarkReplaceOFFlowsInputRendering(b *testing.B) {
	benchCases := []struct {
		name      string
		flowCount int
	}{
		{
			name:      "1k_flows",
			flowCount: 1000,
		},
		{
			name:      "5k_flows",
			flowCount: 5000,
		},
	}

	for _, tc := range benchCases {
		flows := makeBenchmarkFlows(tc.flowCount)
		totalBytes := benchmarkFlowsBytes(flows)

		b.Run(tc.name+"/join_buffer", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(totalBytes)
			for i := 0; i < b.N; i++ {
				stdin := &bytes.Buffer{}
				stdin.Write([]byte(strings.Join(flows, "\n")))
				written, err := io.Copy(io.Discard, stdin)
				if err != nil {
					b.Fatalf("failed to drain old flow payload: %v", err)
				}
				benchmarkFlowBytesSink = written
				benchmarkFlowCountSink = stdin.Len()
			}
		})

		b.Run(tc.name+"/stream_reader", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(totalBytes)
			for i := 0; i < b.N; i++ {
				stdin := &openFlowStdinReader{flows: flows}
				written, err := io.Copy(io.Discard, stdin)
				if err != nil {
					b.Fatalf("failed to drain streaming flow payload: %v", err)
				}
				benchmarkFlowBytesSink = written
				benchmarkFlowCountSink = len(flows)
			}
		})
	}
}

func makeBenchmarkFlows(flowCount int) []string {
	flows := make([]string, flowCount)
	// Keep each flow moderately long to emulate real replace-flows payload size.
	const flowSuffix = ",ip,nw_src=10.128.0.0/14,tp_dst=8080,actions=ct(commit),output:2"
	for i := 0; i < flowCount; i++ {
		flows[i] = "table=0,priority=100,in_port=1,reg0=0x1" + flowSuffix
	}
	return flows
}

func benchmarkFlowsBytes(flows []string) int64 {
	if len(flows) == 0 {
		return 0
	}
	total := len(flows) - 1
	for _, flow := range flows {
		total += len(flow)
	}
	return int64(total)
}
