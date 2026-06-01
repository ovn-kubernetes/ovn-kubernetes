// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"testing"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

func TestSelectNodeConfig(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := testUplink(
		testNodeConfig("role", "blue", "breth0"),
		testNodeConfig("zone", "east", "breth1"),
	)

	selected, overlapping, err := SelectNodeConfig(uplink, testNode("node-a", map[string]string{"role": "blue"}))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(overlapping).To(gomega.BeFalse())
	g.Expect(selected).To(gomega.Equal(&uplink.Spec.NodeConfigs[0]))

	selected, overlapping, err = SelectNodeConfig(uplink, testNode("node-b", map[string]string{"role": "red"}))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(overlapping).To(gomega.BeFalse())
	g.Expect(selected).To(gomega.BeNil())

	selected, overlapping, err = SelectNodeConfig(
		uplink,
		testNode("node-c", map[string]string{"role": "blue", "zone": "east"}),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(overlapping).To(gomega.BeTrue())
	g.Expect(selected).To(gomega.BeNil())
}

func TestSelectNodeConfigs(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := testUplink(
		testNodeConfig("role", "blue", "breth0"),
		testNodeConfig("zone", "east", "breth1"),
	)
	nodes := []*corev1.Node{
		testNode("node-a", map[string]string{"role": "blue"}),
		testNode("node-b", map[string]string{"role": "blue", "zone": "east"}),
		testNode("node-c", map[string]string{"role": "red"}),
	}

	selected, conflicts, err := SelectNodeConfigs(uplink, nodes)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(selected).To(gomega.Equal(map[string]uplinkv1alpha1.UplinkNodeConfig{
		"node-a": uplink.Spec.NodeConfigs[0],
	}))
	g.Expect(conflicts).To(gomega.Equal([]string{"node-b"}))
}

func TestSelectNodeConfigsValidatesSelectorsWithoutNodes(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := testUplink(uplinkv1alpha1.UplinkNodeConfig{
		NodeSelector: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "role",
				Operator: metav1.LabelSelectorOperator("Invalid"),
			}},
		},
	})

	_, _, err := SelectNodeConfigs(uplink, nil)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("nodeConfig 0 has invalid nodeSelector")))
}

func testUplink(configs ...uplinkv1alpha1.UplinkNodeConfig) *uplinkv1alpha1.Uplink {
	return &uplinkv1alpha1.Uplink{
		ObjectMeta: metav1.ObjectMeta{Name: "blue"},
		Spec:       uplinkv1alpha1.UplinkSpec{NodeConfigs: configs},
	}
}

func testNodeConfig(selectorKey, selectorValue, hostInterfaceName string) uplinkv1alpha1.UplinkNodeConfig {
	return uplinkv1alpha1.UplinkNodeConfig{
		Type: uplinkv1alpha1.UplinkTypeOVSBridge,
		NodeSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{selectorKey: selectorValue},
		},
		HostInterfaceName: uplinkv1alpha1.InterfaceName(hostInterfaceName),
	}
}

func testNode(name string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
}
