// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
)

func TestResolvedUplinkL3GatewayConfig(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.NodeportEnable = true

	gwConfig, err := resolvedUplinkL3GatewayConfig(testUplinkState(metav1.ConditionTrue), "node-a", "chassis-a")

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gwConfig.Mode).To(gomega.Equal(config.GatewayModeShared))
	g.Expect(gwConfig.ChassisID).To(gomega.Equal("chassis-a"))
	g.Expect(gwConfig.BridgeID).To(gomega.Equal("breth0"))
	g.Expect(gwConfig.InterfaceID).To(gomega.Equal("breth0_node-a"))
	g.Expect(gwConfig.MACAddress.String()).To(gomega.Equal("02:42:ac:12:00:02"))
	g.Expect(gwConfig.IPAddresses).To(gomega.HaveLen(1))
	g.Expect(gwConfig.IPAddresses[0].String()).To(gomega.Equal("192.0.2.10/24"))
	g.Expect(gwConfig.NextHops).To(gomega.HaveLen(1))
	g.Expect(gwConfig.NextHops[0].String()).To(gomega.Equal("192.0.2.1"))
	g.Expect(gwConfig.NodePortEnable).To(gomega.BeTrue())
}

func TestUplinkStateResolved(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(uplinkutil.StateResolved(testUplinkState(metav1.ConditionTrue))).To(gomega.BeTrue())
	g.Expect(uplinkutil.StateResolved(testUplinkState(metav1.ConditionFalse))).To(gomega.BeFalse())
}

func testUplinkState(resolvedStatus metav1.ConditionStatus) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: "blue-node-a"},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: "blue",
			NodeName:   "node-a",
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "breth0",
			OVSBridge: &uplinkv1alpha1.OVSBridgeStatus{
				Name: "breth0",
			},
			MACAddress:      "02:42:ac:12:00:02",
			IPAddresses:     []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"},
			DefaultGateways: []uplinkv1alpha1.IPAddress{"192.0.2.1"},
			Conditions: []metav1.Condition{
				{
					Type:               uplinkv1alpha1.UplinkStateConditionResolved,
					Status:             resolvedStatus,
					Reason:             uplinkv1alpha1.UplinkStateReasonResolved,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}
}
