// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
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

	ginkgo.Context("on pod add", func() {
		ginkgo.It("does not annotate a pod until its namespace appears in the namespace informer", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *testing.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				pod := testing.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Pod informer will see the pod, but the namespace informer will
				// not have the namespace yet. GetPrimaryNADForNamespace returns
				// an error, and GetNetworkRole will fail and pod annotation will
				// not be written.
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Pod add fails and is queued for retry while namespace is missing.
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryPods)

				// Pod annotation must not be written before namespace is in the informer.
				gomega.Consistently(func() bool {
					p, getErr := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(
						context.TODO(), t.podName, metav1.GetOptions{})
					gomega.Expect(getErr).NotTo(gomega.HaveOccurred())
					_, ok := p.Annotations[ovntypes.OvnPodAnnotationName]
					return ok
				}, time.Second, 100*time.Millisecond).Should(gomega.BeFalse(),
					"pod annotation must not be written before namespace is in the informer")

				// Create namespace so that it is added to the namespace informer.
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(
					context.TODO(), &namespaceT, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait until the namespace informer has the object, then retry.
				gomega.Eventually(func() error {
					_, getErr := fakeOvn.watcher.GetNamespace(namespaceT.Name)
					return getErr
				}).Should(gomega.Succeed(), "namespace informer should have the namespace")

				// Set retry object with no backoff to immediately retry the pod.
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()

				// Pod annotation should be written and should have role=primary.
				var podAnnotation *util.PodAnnotation
				gomega.Eventually(func() bool {
					annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
					if len(annotations) == 0 {
						return false
					}
					podAnnotation, err = util.UnmarshalPodAnnotation(
						map[string]string{ovntypes.OvnPodAnnotationName: annotations},
						ovntypes.DefaultNetworkName,
					)
					if err != nil {
						return false
					}
					return podAnnotation.Role == ovntypes.NetworkRolePrimary
				}).Should(gomega.BeTrue(), fmt.Sprintf("pod annotation should be written and should have role=primary: %v", podAnnotation))

				// Pod should not be queued for retry.
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})
})
