package images

import "os"

var (
	aghHost = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	iperf3  = "quay.io/sronanrh/iperf:latest"
	httpd   = "docker.io/httpd:latest"
)

func init() {
	if agnHostOverride := os.Getenv("AGNHOST_IMAGE"); agnHostOverride != "" {
		aghHost = agnHostOverride
	}
	if iperf3Override := os.Getenv("IPERF3_IMAGE"); iperf3Override != "" {
		iperf3 = iperf3Override
	}
	if httpdOverride := os.Getenv("HTTPD_IMAGE"); httpdOverride != "" {
		httpd = httpdOverride
	}
}

func AgnHost() string {
	return aghHost
}

func IPerf3() string {
	return iperf3
}
