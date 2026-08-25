// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("NodePortAddressConfig", func() {
	ginkgo.It("treats empty configuration as unrestricted", func() {
		cfg, err := ParseNodePortAddresses("")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(cfg.Restricted()).To(gomega.BeFalse())

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		gomega.Expect(cfg.FilterHostAddresses(hosts, nil)).To(gomega.Equal(hosts))
	})

	ginkgo.It("filters host addresses by CIDR", func() {
		cfg, err := ParseNodePortAddresses("10.0.0.0/8")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(cfg.Restricted()).To(gomega.BeTrue())

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		filtered := cfg.FilterHostAddresses(hosts, nil)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{net.ParseIP("10.0.0.5")}))
	})

	ginkgo.It("uses primary addresses when configured", func() {
		cfg, err := ParseNodePortAddresses("primary")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		primary := []net.IP{net.ParseIP("10.0.0.5")}
		filtered := cfg.FilterHostAddresses(hosts, primary)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{net.ParseIP("10.0.0.5")}))
	})

	ginkgo.It("rejects invalid CIDR values", func() {
		_, err := ParseNodePortAddresses("not-a-cidr")
		gomega.Expect(err).To(gomega.HaveOccurred())
	})
})
