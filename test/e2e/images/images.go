package images

import (
	"os"

	"k8s.io/kubernetes/test/utils/image"
)

var (
	// We limit the set of images used by e2e to reduce duplication and to allow us to provide offline mirroring of images
	// for customers and restricted test environments.
	// Ideally, every image used in e2e must be part of this package.
	// New test images should ideally be sourced from the upstream k8s.io/kubernetes/test/utils/image package.
	// Failing to find an image from upstream k8s, please get community approval because downstream consumers must
	// pre-approve new images.
	agnHost = image.GetE2EImage(image.Agnhost)
	// FIXME: iperf3 image should not be retrieved from a users repo and should not have latest tag
	iperf3 = "quay.io/sronanrh/iperf:latest"
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
