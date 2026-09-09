// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"os"

	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

var (
	// We limit the set of images used by e2e to reduce duplication and to allow us to provide offline mirroring of images
	// for customers and restricted test environments.
	// Ideally, every image used in e2e must be part of this package.
	// New test images should ideally be sourced from the upstream k8s.io/kubernetes/test/utils/image package.
	// Failing to find an image from upstream k8s, please get community approval because downstream consumers must
	// pre-approve new images.
	// FIXME: iperf3 image should not be retrieved from a users repo and should not have latest tag
	iperf3                = "quay.io/sronanrh/iperf:latest"
	netshoot              = "ghcr.io/nicolaka/netshoot:v0.13"
	nginx                 = "nginx:1"
	metallbLBService      = "quay.io/itssurya/dev-images:metallb-lbservice"
	udpServerSrcIPPrinter = "quay.io/itssurya/dev-images:udp-server-srcip-printer"
	frr                   = "quay.io/frrouting/frr:10.5.3"
	// dnsmasq 2.83; pinned by digest for CI reproducibility.
	// TODO: mirror to a project-controlled registry (ghcr/quay) — docker.io
	// pulls are rate-limited in CI and this is a personal repository.
	dnsmasq = "docker.io/andyshinn/dnsmasq:2.83@sha256:e937327fede666e55ba4c2ab8e715a2ce561945363016d42f9d698d1b18ff1be"

	agnHostOverride = ""
	extraImages     []api.ImageConfig
)

func init() {
	agnHostOverride = os.Getenv("AGNHOST_IMAGE")
	if iperf3Override := os.Getenv("IPERF3_IMAGE"); iperf3Override != "" {
		iperf3 = iperf3Override
	}
	if netshootOverride := os.Getenv("NETSHOOT_IMAGE"); netshootOverride != "" {
		netshoot = netshootOverride
	}
	if nginxOverride := os.Getenv("NGINX_IMAGE"); nginxOverride != "" {
		nginx = nginxOverride
	}
	if metallbLBServiceOverride := os.Getenv("METALLB_LB_SERVICE_IMAGE"); metallbLBServiceOverride != "" {
		metallbLBService = metallbLBServiceOverride
	}
	if udpServerOverride := os.Getenv("UDP_SERVER_SRCIP_PRINTER_IMAGE"); udpServerOverride != "" {
		udpServerSrcIPPrinter = udpServerOverride
	}
	if frrOverride := os.Getenv("FRR_IMAGE"); frrOverride != "" {
		frr = frrOverride
	}
}

func AgnHost() api.ImageConfig {
	agnHost := deploymentconfig.Get().GetAgnHostContainerImage()
	if agnHostOverride != "" {
		agnHost.PullSpec = agnHostOverride
	}
	return agnHost
}

func IPerf3() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: iperf3,
	}
}

// DNSMasq returns an image containing the dnsmasq DHCP server, used as the
// external DHCP server on the underlay for DHCP-IPAM localnet tests.
func DNSMasq() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: dnsmasq,
	}
}

func Netshoot() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: netshoot,
	}
}

func Nginx() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: nginx,
	}
}

func MetalLBLBService() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: metallbLBService,
	}
}

func UDPServerSrcIPPrinter() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: udpServerSrcIPPrinter,
	}
}

func FRR() api.ImageConfig {
	return api.ImageConfig{
		ImageID:  int(imageutils.None),
		PullSpec: frr,
	}
}

// Add registers images that are needed by a test suite. Call from init()
// functions after checking any relevant feature gates or environment
// variables so that only images for enabled test suites are included.
func Add(imgs ...api.ImageConfig) {
	extraImages = append(extraImages, imgs...)
}

// Required returns the deduplicated set of images needed for the current
// test run. agnhost is always included because it is used by most e2e tests.
func Required() []api.ImageConfig {
	agnHost := AgnHost()
	seen := map[api.ImageConfig]struct{}{
		agnHost: {},
	}
	out := []api.ImageConfig{agnHost}
	for _, img := range extraImages {
		if _, ok := seen[img]; !ok {
			seen[img] = struct{}{}
			out = append(out, img)
		}
	}
	return out
}
