// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package udn

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/gomega"
)

// TestGetGWRouterIPsSecondaryUDN validates the join-router-IP allocation gate:
// only an advertised secondary UDN gets a gateway-router join IP; a plain
// secondary still returns an empty result, while primary and default keep their
// behavior.
func TestGetGWRouterIPsSecondaryUDN(t *testing.T) {
	g := NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	const (
		nodeName  = "worker1"
		networkID = "5"
	)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.OvnNodeID:            "7",
				"k8s.ovn.org/network-ids": `{"bluenet": "5"}`,
			},
		},
	}

	mkNet := func(role string) util.NetInfo {
		nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", role)
		ovntest.AnnotateNADWithNetworkID(networkID, nad)
		ni, err := util.ParseNADInfo(nad)
		g.Expect(err).NotTo(HaveOccurred())
		return ni
	}
	advertisedAt := func(ni util.NetInfo, n string) util.NetInfo {
		m := util.NewMutableNetInfo(ni)
		m.SetPodNetworkAdvertisedVRFs(map[string][]string{n: {ni.GetNetworkName()}})
		return m
	}

	primary := mkNet(types.NetworkRolePrimary)
	secondary := mkNet(types.NetworkRoleSecondary)
	advertisedSecondary := advertisedAt(secondary, nodeName)

	// Primary UDN: gets a join IP (pre-existing behavior).
	primaryIPs, err := GetGWRouterIPs(node, primary)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(primaryIPs).NotTo(BeEmpty(), "primary UDN must get a join router IP")

	// Non-advertised secondary UDN: stays east/west-only, no join IP.
	secondaryIPs, err := GetGWRouterIPs(node, secondary)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(secondaryIPs).To(BeEmpty(), "non-advertised secondary UDN must not get a join router IP")

	// Advertised secondary UDN: gets a join IP.
	advIPs, err := GetGWRouterIPs(node, advertisedSecondary)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(advIPs).NotTo(BeEmpty(), "advertised secondary UDN must get a join router IP")

	// Advertised at a different node only: not advertised here, so no join IP.
	advElsewhere := advertisedAt(secondary, "otherNode")
	elsewhereIPs, err := GetGWRouterIPs(node, advElsewhere)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(elsewhereIPs).To(BeEmpty(),
		"secondary advertised on a different node must not get a join router IP here")
}
