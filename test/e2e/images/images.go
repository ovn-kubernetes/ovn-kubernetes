// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"os"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
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
	extraImages     []string
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

func AgnHost() string {
	if agnHostOverride != "" {
		return agnHostOverride
	}
	return deploymentconfig.Get().GetAgnHostContainerImage()
}

// ExternalAgnHost returns the default image used for external
// (host-side) containers. Such containers may run on a host that cannot reach
// the same registry as in-cluster pods, so USER_PROVIDED_AGNHOST_IMAGE can point
// them at a reachable image. When it is unset the value falls back to AgnHost()
// (which itself honors the cluster-wide AGNHOST_IMAGE override), so the default
// image is unchanged.
func ExternalAgnHost() string {
	if v := os.Getenv("USER_PROVIDED_AGNHOST_IMAGE"); v != "" {
		return v
	}
	return AgnHost()
}

func IPerf3() string {
	return iperf3
}

// DNSMasq returns an image containing the dnsmasq DHCP server, used as the
// external DHCP server on the underlay for DHCP-IPAM localnet tests.
func DNSMasq() string {
	return dnsmasq
}

func Netshoot() string {
	return netshoot
}

func Nginx() string {
	return nginx
}

func MetalLBLBService() string {
	return metallbLBService
}

func UDPServerSrcIPPrinter() string {
	return udpServerSrcIPPrinter
}

func FRR() string {
	return frr
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
	agnHost := AgnHost()
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
