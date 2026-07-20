// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("OVN Pod Operations with network segmentation", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	const (
		node1Name = "node1"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		fakeOvn = NewFakeOVN(true)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}

	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("reconciles an existing pod with missing 'role=primary' ovn annotation field", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *testing.NewNamespace("namespace1")
				// use 2 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				// Remove network role as this is the expected initial
				t1.networkRole = ""

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								libovsdbops.RequestedChassis: requestedChassisForPod(t1),
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{t1.portUUID},
						},
					},
				}

				pod1 := testing.NewPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod1,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn)
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check db values are updated to correlate with test pods settings
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t1}, []string{"node1"})))
				// check annotations are updated with role=primary
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)

				// Expect ovn pod annotated with role=primary
				t1.networkRole = ovntypes.NetworkRolePrimary
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on update", func() {
		ginkgo.It("updates UDN open-port ACLs without recreating the pod logical switch port", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *testing.NewNamespace("namespace1")
				t := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				t.networkRole = ovntypes.NetworkRoleInfrastructure

				oldPod := testing.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				setPodAnnotations(oldPod, t)
				oldPod.Annotations[util.UDNOpenPortsAnnotationName] = "- protocol: tcp\n  port: 8080"

				primaryNetwork := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
				netInfo, err := util.NewNetInfo(primaryNetwork.netconf())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				primaryNADKey := namespaceT.Name + "/primary-network"
				mutableNetInfo.SetNADs(primaryNADKey)
				fakeOvn.networkManager = &networkmanager.FakeNetworkManager{
					PrimaryNetworks: map[string]util.NetInfo{namespaceT.Name: mutableNetInfo},
					NADNetworks:     map[string]util.NetInfo{primaryNADKey: mutableNetInfo},
				}

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespaceT}},
					&corev1.NodeList{Items: []corev1.Node{*newNode(node1Name, "192.168.126.202/24")}},
					&corev1.PodList{Items: []corev1.Pod{*oldPod}},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				gomega.Expect(fakeOvn.controller.WatchNamespaces()).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.setupUDNACLs(nil)).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.addLogicalPort(oldPod)).To(gomega.Succeed())

				appliedState := fakeOvn.controller.GetPodState(oldPod)
				gomega.Expect(appliedState).NotTo(gomega.BeNil())
				lspBefore, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient,
					&nbdb.LogicalSwitchPort{Name: t.portName})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				getIngressOpenPortACL := func() *nbdb.ACL {
					acls, err := libovsdbops.FindACLsWithPredicate(fakeOvn.nbClient, func(acl *nbdb.ACL) bool {
						return acl.Direction == nbdb.ACLDirectionToLport &&
							strings.Contains(acl.Match, fmt.Sprintf(`outport == "%s"`, t.portName))
					})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(acls).To(gomega.HaveLen(1))
					return acls[0]
				}
				gomega.Expect(getIngressOpenPortACL().Match).To(gomega.ContainSubstring("tcp.dst == 8080"))

				newPod := oldPod.DeepCopy()
				newPod.Annotations[util.UDNOpenPortsAnnotationName] = "- protocol: udp\n  port: 5353"
				gomega.Expect(newPod.UID).To(gomega.Equal(oldPod.UID))
				gomega.Expect(fakeOvn.controller.shouldEnsurePodLogicalPort(newPod, ovntypes.DefaultNetworkName)).To(gomega.BeFalse())
				newState, err := fakeOvn.controller.ReconcilePod(oldPod, newPod, appliedState)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(newState).NotTo(gomega.BeNil())

				lspAfter, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient,
					&nbdb.LogicalSwitchPort{Name: t.portName})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(lspAfter.UUID).To(gomega.Equal(lspBefore.UUID))
				updatedACL := getIngressOpenPortACL()
				gomega.Expect(updatedACL.Match).To(gomega.ContainSubstring("udp.dst == 5353"))
				gomega.Expect(updatedACL.Match).NotTo(gomega.ContainSubstring("tcp.dst == 8080"))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})
})
