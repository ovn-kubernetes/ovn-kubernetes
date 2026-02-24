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
	iperf3   = "quay.io/sronanrh/iperf:latest"
	netshoot = "ghcr.io/nicolaka/netshoot:v0.13"

	extraImages []string
)

func init() {
	if agnHostOverride := os.Getenv("AGNHOST_IMAGE"); agnHostOverride != "" {
		agnHost = agnHostOverride
	}
	if iperf3Override := os.Getenv("IPERF3_IMAGE"); iperf3Override != "" {
		iperf3 = iperf3Override
	}
	if netshootOverride := os.Getenv("NETSHOOT_IMAGE"); netshootOverride != "" {
		netshoot = netshootOverride
	}
}

func AgnHost() string {
	return agnHost
}

func IPerf3() string {
	return iperf3
}

func Netshoot() string {
	return netshoot
}

// Add registers images that are needed by a test suite. Call from init()
// functions after checking any relevant feature gates or environment
// variables so that only images for enabled test suites are included.
func Add(imgs ...string) {
	extraImages = append(extraImages, imgs...)
}

// Required returns the deduplicated set of images needed for the current
// test run. agnhost is always included because it is used by most e2e tests.
func Required() []string {
	seen := map[string]struct{}{
		agnHost: {},
	}
	out := []string{agnHost}
	for _, img := range extraImages {
		if _, ok := seen[img]; !ok {
			seen[img] = struct{}{}
			out = append(out, img)
		}
	}
	return out
}
