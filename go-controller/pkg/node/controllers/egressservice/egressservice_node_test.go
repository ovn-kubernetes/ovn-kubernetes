// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressservice

import (
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	"k8s.io/client-go/kubernetes/fake"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type fakeRuleManager struct {
	iprulemanager.FakeControllerWithError
	calledFamilies []int
}

func (f *fakeRuleManager) AddNodeIPFwMarkRule(family int) error {
	f.calledFamilies = append(f.calledFamilies, family)
	return nil
}

const testNodeName = "test-node"

var fexec *ovntest.FakeExec

var _ = ginkgo.Describe("EgressService", func() {
	ginkgo.BeforeEach(func() {
		fexec = ovntest.NewFakeExec()
		gomega.Expect(util.SetExec(fexec)).To(gomega.Succeed())
		nodenft.SetFakeNFTablesHelper()
	})

	ginkgo.AfterEach(func() {
		util.ResetRunner()
	})

	ginkgo.DescribeTable("AddNodeIPFwMarkRule based on gateway mode",
		func(gwMode ovnconfig.GatewayMode, ipv4, ipv6 bool, expectedFamilies []int) {
			stopCh := make(chan struct{})
			defer close(stopCh)

			ovnconfig.Gateway.Mode = gwMode
			ovnconfig.IPv4Mode = ipv4
			ovnconfig.IPv6Mode = ipv6
			ovnconfig.OVNKubernetesFeature.EnableEgressService = true

			if ipv4 {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ip -4 --json rule show", Output: "[]"})
			}
			if ipv6 {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ip -6 --json rule show", Output: "[]"})
			}

			ovnNodeClient := &util.OVNNodeClientset{
				KubeClient:          fake.NewSimpleClientset(),
				EgressServiceClient: egressservicefake.NewSimpleClientset(),
			}
			watchFactory, err := factory.NewNodeWatchFactory(ovnNodeClient, testNodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(watchFactory.Start()).To(gomega.Succeed())
			c, err := NewController(stopCh, nodetypes.OvnKubeNodeSNATMark, testNodeName,
				watchFactory.EgressServiceInformer(),
				watchFactory.ServiceInformer(),
				watchFactory.EndpointSliceInformer())
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			fakeRM := &fakeRuleManager{}
			c.ruleManager = fakeRM

			var wg sync.WaitGroup
			gomega.Expect(c.Run(&wg, 1)).To(gomega.Succeed())

			gomega.Expect(fakeRM.calledFamilies).To(gomega.ConsistOf(expectedFamilies))
		},
		ginkgo.Entry("LGW IPv4", ovnconfig.GatewayModeLocal, true, false, []int{netlink.FAMILY_V4}),
		ginkgo.Entry("LGW IPv6", ovnconfig.GatewayModeLocal, false, true, []int{netlink.FAMILY_V6}),
		ginkgo.Entry("LGW dual-stack", ovnconfig.GatewayModeLocal, true, true, []int{netlink.FAMILY_V4, netlink.FAMILY_V6}),
		ginkgo.Entry("SGW skipped", ovnconfig.GatewayModeShared, true, false, []int{}),
	)
})
