package images

import "os"

var (
	agnHost = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	iperf3  = "quay.io/sronanrh/iperf:latest"
)

func init() {
	if agnHostOverride := os.Getenv("AGNHOST_IMAGE"); agnHostOverride != "" {
		agnHost = agnHostOverride
	}
	if iperf3Override := os.Getenv("IPERF3_IMAGE"); iperf3Override != "" {
		iperf3 = iperf3Override
	}
}

func AgnHost() string {
	return agnHost
}

func IPerf3() string {
	return iperf3
}
