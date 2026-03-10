package addressset

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func hasAllAddressesWithSet(existing, wanted []string) bool {
	if len(existing) == 0 {
		return false
	}
	return sets.NewString(existing...).HasAll(wanted...)
}

func hasAllAddressesWithScan(existing, wanted []string) bool {
	if len(existing) == 0 {
		return false
	}
	for _, want := range wanted {
		found := false
		for _, have := range existing {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func buildBenchmarkIPs(size int) []string {
	ips := make([]string, size)
	for i := 0; i < size; i++ {
		ips[i] = fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
	}
	return ips
}

func buildWantedIPs(existing []string, wantedCount int, includeMissing bool) []string {
	wanted := make([]string, 0, wantedCount)
	for i := 0; i < wantedCount; i++ {
		// Pick addresses from the tail to avoid accidental early hits.
		wanted = append(wanted, existing[len(existing)-1-i])
	}
	if includeMissing {
		wanted[wantedCount-1] = "192.0.2.250"
	}
	return wanted
}

func benchmarkHasAllAddresses(
	b *testing.B,
	implName string,
	fn func(existing, wanted []string) bool,
	existingSize, wantedCount int,
	includeMissing bool,
) {
	b.Helper()
	name := fmt.Sprintf("%s/existing-%d/wanted-%d/missing-%t", implName, existingSize, wantedCount, includeMissing)
	b.Run(name, func(b *testing.B) {
		existing := buildBenchmarkIPs(existingSize)
		wanted := buildWantedIPs(existing, wantedCount, includeMissing)
		expected := !includeMissing
		if got := fn(existing, wanted); got != expected {
			b.Fatalf("unexpected result: got=%t expected=%t", got, expected)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = fn(existing, wanted)
		}
	})
}

func BenchmarkHasAllAddressesSetVsScan(b *testing.B) {
	existingSizes := []int{500, 50000}
	wantedCounts := []int{1, 10}
	missingCases := []bool{false, true}

	for _, existingSize := range existingSizes {
		for _, wantedCount := range wantedCounts {
			for _, includeMissing := range missingCases {
				benchmarkHasAllAddresses(b, "set", hasAllAddressesWithSet, existingSize, wantedCount, includeMissing)
				benchmarkHasAllAddresses(b, "scan", hasAllAddressesWithScan, existingSize, wantedCount, includeMissing)
			}
		}
	}
}
