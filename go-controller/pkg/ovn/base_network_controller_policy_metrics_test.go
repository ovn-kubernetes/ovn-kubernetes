// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	cnitypes "github.com/containernetworking/cni/pkg/types"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("network policy scale metrics", func() {
	It("uses the shared scale-metrics gate", func() {
		savedEnableScaleMetrics := config.Metrics.EnableScaleMetrics
		DeferCleanup(func() {
			config.Metrics.EnableScaleMetrics = savedEnableScaleMetrics
		})

		config.Metrics.EnableScaleMetrics = true
		primaryNetInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "primary-udn"},
			Role:     ovntypes.NetworkRolePrimary,
			Topology: ovntypes.Layer3Topology,
			NADName:  "ns/primary-udn",
		})
		Expect(err).NotTo(HaveOccurred())
		secondaryNetInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "secondary-udn"},
			Role:     ovntypes.NetworkRoleSecondary,
			Topology: ovntypes.Layer3Topology,
			NADName:  "ns/secondary-udn",
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(getFakeBaseController(&util.DefaultNetInfo{}).networkPolicyMetricsEnabled()).To(BeTrue())
		Expect(getFakeBaseController(primaryNetInfo).networkPolicyMetricsEnabled()).To(BeTrue())
		Expect(getFakeBaseController(secondaryNetInfo).networkPolicyMetricsEnabled()).To(BeFalse())

		config.Metrics.EnableScaleMetrics = false
		Expect(getFakeBaseController(primaryNetInfo).networkPolicyMetricsEnabled()).To(BeFalse())
	})
})
