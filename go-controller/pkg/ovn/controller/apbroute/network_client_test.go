// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package apbroute

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("northBoundClient", func() {
	It("skips deleting pod SNAT for a remote node without looking up the Node", func() {
		nb := &northBoundClient{nodeName: "local-node"}

		Expect(nb.deletePodSNAT("remote-node", "GR_remote-node", nil, nil)).To(Succeed())
	})
})
