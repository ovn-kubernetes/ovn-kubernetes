package iperf3helper

import (
	"encoding/json"
	"fmt"
)

// Iperf3Result represents the parsed iperf3 JSON output when using --json flag
type Iperf3Result struct {
	SumSent           Iperf3TestRun `json:"sum_sent"`
	SumReceived       Iperf3TestRun `json:"sum_received"`
	IsBytesEqual      bool
	IsZeroRetransmits bool
}

func (i Iperf3Result) String() string {
	res, err := json.Marshal(i)
	if err != nil {
		panic(fmt.Sprintf("failed to unmarshall Iperf3 test run result: %+v", i))
	}
	return string(res)
}

// Iperf3TestRun represents the sum data from iperf3 output when using --json flag
type Iperf3TestRun struct {
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	Seconds       float64 `json:"seconds"`
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Retransmits   int     `json:"retransmits,omitempty"` // retransmits is only available from sum_sent therefore add omitempty
}

func (i Iperf3TestRun) String() string {
	res, err := json.Marshal(i)
	if err != nil {
		panic(fmt.Sprintf("failed to unmarshall Iperf3 test run: %+v", i))
	}
	return string(res)
}

// iperf3Output represents the iperf3 JSON structure specifically for parsing json output "end" key. "end" map contains
// "sum_sent" and "sum_received" which contain data about the data send and received during test time.
type iperf3Output struct {
	End struct {
		SumSent     Iperf3TestRun `json:"sum_sent"`
		SumReceived Iperf3TestRun `json:"sum_received"`
	} `json:"end"`
}

// ParseIperf3JSON parses iperf3 JSON output and extracts sum_sent and sum_received from the "end" key. Arg jsonOutput must be
// supplied from an iperf3 client with the --json flag specified.
func ParseIperf3JSON(jsonOutput string) (*Iperf3Result, error) {
	var output iperf3Output

	if err := json.Unmarshal([]byte(jsonOutput), &output); err != nil {
		return nil, fmt.Errorf("failed to parse iperf3 JSON, JSON input:\n%q\n,err: %w", jsonOutput, err)
	}

	result := &Iperf3Result{
		SumSent:     output.End.SumSent,
		SumReceived: output.End.SumReceived,
	}

	// Check if bytes sent equals bytes received
	result.IsBytesEqual = result.SumSent.Bytes == result.SumReceived.Bytes
	// Check if there were retransmissions (tcp only)
	result.IsZeroRetransmits = result.SumSent.Retransmits == 0

	return result, nil
}

// IsSendReceivedBytesEqual returns true if bytes sent equals bytes received
func (r *Iperf3Result) IsSendReceivedBytesEqual() bool {
	return r.IsBytesEqual
}

// GetSendReceivedBytesDifference returns the difference between sent and received bytes during the test run
func (r *Iperf3Result) GetSendReceivedBytesDifference() int64 {
	return r.SumSent.Bytes - r.SumReceived.Bytes
}

// GetRetransmissions returns the total amount of re-transmissions seen over the test run
func (r *Iperf3Result) GetRetransmissions() int {
	return r.SumSent.Retransmits
}
