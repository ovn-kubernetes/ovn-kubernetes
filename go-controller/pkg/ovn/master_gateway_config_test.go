// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("nodeGatewayConfig join address contract", func() {
	const (
		clusterCIDR = "10.1.0.0/16"
	)

	var app *cli.App

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	ginkgo.It("derives join addresses from node-id", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
						"k8s.ovn.org/node-chassis-id":   "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-subnets":      `{"default":["10.244.1.0/24"]}`,
						util.OvnNodeID:                  "2",
					},
				},
			}

			oc := getFakeController("default-network-controller")
			gwConfig, err := oc.nodeGatewayConfig(node)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(gwConfig.gwRouterJoinCIDRs).To(gomega.HaveLen(1))
			gomega.Expect(gwConfig.gwRouterJoinCIDRs[0].IP.String()).To(gomega.Equal("100.64.0.2"))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("fails when node-id is missing", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
						"k8s.ovn.org/node-chassis-id":   "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-subnets":      `{"default":["10.244.1.0/24"]}`,
						// Retired annotation with a valid value: if the deprecated
						// node-id fallback is ever restored, this test would stop
						// failing and thus catch the regression.
						"k8s.ovn.org/node-gateway-router-lrp-ifaddr": `{"ipv4":"100.64.0.99/16"}`,
					},
				},
			}

			oc := getFakeController("default-network-controller")
			_, err = oc.nodeGatewayConfig(node)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				fmt.Sprintf("failed to get join switch port IP address for node %s", node.Name))))
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				fmt.Sprintf("%s annotation not found", util.OvnNodeID))))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
