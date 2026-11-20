// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/onsi/gomega"
)

func TestHasAdvertisedSNAT(t *testing.T) {
	tests := []struct {
		name      string
		nats      string
		subnet    string
		expected  bool
		expectErr bool
	}{
		{
			name:     "destination match",
			nats:     `108.108.0.0/16,"eth.dst == 0a:58:5d:5d:00:02 && ip4.dst == $a123",[],[]`,
			subnet:   "108.108.0.0/16",
			expected: true,
		},
		{
			name: "allowed external IP address sets",
			nats: "108.108.0.0/16,,a-node-ips,[]\n" +
				"108.108.0.0/16,,a-service-ips,[]",
			subnet:   "108.108.0.0/16",
			expected: true,
		},
		{
			name:     "exempted external IP address set",
			nats:     "108.108.0.0/16,[],[],a-pod-subnets",
			subnet:   "108.108.0.0/16",
			expected: true,
		},
		{
			name:     "unconditional SNAT",
			nats:     "108.108.0.0/16,,[],[]",
			subnet:   "108.108.0.0/16",
			expected: false,
		},
		{
			name: "stale unconditional twin",
			nats: "108.108.0.0/16,,a-node-ips,[]\n" +
				"108.108.0.0/16,,[],[]",
			subnet:   "108.108.0.0/16",
			expected: false,
		},
		{
			name:     "output port match without advertised destination",
			nats:     `108.108.0.0/16,"outport == ""rtoe-GR_worker""",[],[]`,
			subnet:   "108.108.0.0/16",
			expected: false,
		},
		{
			name:     "subnet absent",
			nats:     "2001:db8:108:108::/64,,a-node-ips,[]",
			subnet:   "108.108.0.0/16",
			expected: false,
		},
		{
			name:      "malformed output",
			nats:      "108.108.0.0/16,,[]",
			subnet:    "108.108.0.0/16",
			expectErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			actual, err := hasAdvertisedSNAT(test.nats, test.subnet)
			if test.expectErr {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(actual).To(gomega.Equal(test.expected))
		})
	}
}
