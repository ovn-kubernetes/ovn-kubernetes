// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package endpointslicemirror

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Cluster manager EndpointSlice mirror controller", func() {
	var (
		app            *cli.App
		controller     *Controller
		fakeClient     *util.OVNClusterManagerClientset
		networkManager networkmanager.Controller
	)

	start := func(objects ...runtime.Object) {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDNSNameResolver = true

		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkManager, err = networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClient, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		controller, err = NewController(fakeClient, wf, networkManager.Interface())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = networkManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = controller.Start(context.Background(), 1)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	ginkgo.AfterEach(func() {
		if controller != nil {
			controller.Stop()
		}
		if networkManager != nil {
			networkManager.Stop()
		}
	})

	ginkgo.Context("on startup repair", func() {
		ginkgo.It("should delete stale mirrored EndpointSlices and create missing ones", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""
				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod", "", "10.244.2.3", "l3-network", "10.132.2.4")

				defaultEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
							discovery.LabelManagedBy:   types.EndpointSliceDefaultControllerName,
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.2.3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
					},
				}
				staleEndpointSlice := testing.MirrorEndpointSlice(&defaultEndpointSlice, "l3-network", false)
				staleEndpointSlice.Annotations[types.SourceEndpointSliceAnnotation] = "non-existing-endpointslice"

				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							*staleEndpointSlice,
							defaultEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)

				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					// defaultEndpointSlice should exist
					_, err := fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Get(context.TODO(), defaultEndpointSlice.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					// staleEndpointSlice should be removed
					staleMirror, err := fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Get(context.TODO(), staleEndpointSlice.Name, metav1.GetOptions{})
					if err == nil {
						return fmt.Errorf("the stale mirrored EndpointSlice should not exist: %v", staleMirror)
					}
					if err != nil && !apierrors.IsNotFound(err) {
						return err
					}

					// new mirrored EndpointSlice should get created
					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, defaultEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}

					if len(mirroredEndpointSlices) == 0 {
						return fmt.Errorf("expected one mirrored EndpointSlices")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].Endpoints).To(gomega.HaveLen(1))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.HaveLen(1))
				// check if the Address is set to the primary IP
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses[0]).To(gomega.BeEquivalentTo("10.132.2.4"))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on EndpointSlices changes", func() {
		ginkgo.It("should not create mirrored EndpointSlices in namespaces that are not using user defined networks as primary", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod", "", "10.244.2.3", "l3-network", "10.132.2.4")

				defaultEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
							discovery.LabelManagedBy:   types.EndpointSliceDefaultControllerName,
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.2.3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
					},
				}

				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							defaultEndpointSlice,
						},
					},
				}

				start(objs...)

				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRoleSecondary),
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					// defaultEndpointSlice should exist
					_, err := fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Get(context.TODO(), defaultEndpointSlice.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				gomega.Consistently(func() error {
					// no mirrored EndpointSlices should exist
					mirrorEndpointSliceSelector := labels.Set(map[string]string{
						discovery.LabelManagedBy: types.EndpointSliceMirrorControllerName,
					}).AsSelectorPreValidated()

					mirroredEndpointSlices, err := fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).List(context.TODO(), metav1.ListOptions{LabelSelector: mirrorEndpointSliceSelector.String()})
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices.Items) != 0 {
						return fmt.Errorf("expected no mirrored EndpointSlices")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should update/delete mirrored EndpointSlices in namespaces that use user defined networks as primary ", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod", "", "10.244.2.3", "l3-network", "10.132.2.4")

				defaultEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
							discovery.LabelManagedBy:   types.EndpointSliceDefaultControllerName,
						},
						ResourceVersion: "1",
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.2.3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
					},
				}
				mirroredEndpointSlice := testing.MirrorEndpointSlice(&defaultEndpointSlice, "l3-network", false)
				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							defaultEndpointSlice,
							*mirroredEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					// nad should exist
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					// defaultEndpointSlice should exist
					_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Get(context.TODO(), defaultEndpointSlice.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					// mirrored EndpointSlices should exist
					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, defaultEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice")
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 1 {
						return fmt.Errorf("expected one Endpoint")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.HaveLen(1))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"10.132.2.4"}))

				ginkgo.By("when the EndpointSlice changes the mirrored one gets updated")
				newPod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod-new", "", "10.244.2.4", "l3-network", "10.132.2.5")

				_, err = fakeClient.KubeClient.CoreV1().Pods(newPod.Namespace).Create(context.TODO(), &newPod, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(func() error {
					_, err = fakeClient.KubeClient.CoreV1().Pods(newPod.Namespace).Get(context.TODO(), newPod.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				defaultEndpointSlice.Endpoints = append(defaultEndpointSlice.Endpoints, discovery.Endpoint{
					Addresses: []string{"10.244.2.4"},
					TargetRef: &corev1.ObjectReference{
						Kind:      "Pod",
						Namespace: newPod.Namespace,
						Name:      newPod.Name,
					},
				})
				defaultEndpointSlice.ResourceVersion = "2"
				_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(newPod.Namespace).Update(context.TODO(), &defaultEndpointSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					_, err = fakeClient.KubeClient.CoreV1().Pods(newPod.Namespace).Get(context.TODO(), newPod.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, defaultEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice")
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 2 {
						return fmt.Errorf("expected two addresses, got: %d", len(mirroredEndpointSlices[0].Endpoints))
					}

					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses[0]).To(gomega.BeEquivalentTo("10.132.2.4"))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[1].Addresses[0]).To(gomega.BeEquivalentTo("10.132.2.5"))

				ginkgo.By("when the default EndpointSlice is removed the mirrored one follows")
				err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(newPod.Namespace).Delete(context.TODO(), defaultEndpointSlice.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, defaultEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 0 {
						return fmt.Errorf("expected no mirrored EndpointSlices")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should create mirrored EndpointSlices for long endpointslice and network names", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod", "", "10.244.2.3", "l3-network", "10.132.2.4")
				longName := strings.Repeat("a", 253)

				defaultEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      longName,
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
							discovery.LabelManagedBy:   types.EndpointSliceDefaultControllerName,
						},
						ResourceVersion: "1",
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.2.3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
					},
				}
				// make sure that really long network names work too
				longNetName := "network" + longName
				mirroredEndpointSlice := testing.MirrorEndpointSlice(&defaultEndpointSlice, longNetName, false)
				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							defaultEndpointSlice,
							*mirroredEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					// nad should exist
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					// defaultEndpointSlice should exist
					_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Get(context.TODO(), defaultEndpointSlice.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					// mirrored EndpointSlices should exist
					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, defaultEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice")
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 1 {
						return fmt.Errorf("expected one Endpoint")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.HaveLen(1))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"10.132.2.4"}))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should mirror selector-less EndpointSlices with non-Pod addresses as-is", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				blockOwnerDeletion := true
				manualEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "manual-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "external-svc",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "v1",
							Kind:               "Endpoints",
							Name:               "external-svc",
							UID:                "endpoints-uid",
							BlockOwnerDeletion: &blockOwnerDeletion,
						}},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.100.50"},
						},
					},
				}
				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							manualEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, manualEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 1 {
						return fmt.Errorf("expected one Endpoint, got %d", len(mirroredEndpointSlices[0].Endpoints))
					}
					if len(mirroredEndpointSlices[0].Endpoints[0].Addresses) != 1 {
						return fmt.Errorf("expected one Address, got %d", len(mirroredEndpointSlices[0].Endpoints[0].Addresses))
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"192.168.100.50"}))
				gomega.Expect(mirroredEndpointSlices[0].Labels[types.LabelUserDefinedServiceName]).To(gomega.Equal("external-svc"))
				gomega.Expect(mirroredEndpointSlices[0].Annotations[types.UserDefinedNetworkEndpointSliceAnnotation]).To(gomega.Equal("l3-network"))
				gomega.Expect(mirroredEndpointSlices[0].Annotations[types.SourceEndpointSliceAnnotation]).To(gomega.Equal(manualEndpointSlice.Name))
				// Endpoints owner refs with blockOwnerDeletion must not be copied
				gomega.Expect(mirroredEndpointSlices[0].OwnerReferences).To(gomega.BeEmpty())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should strip Endpoints ownerRefs from an existing mirror even when source ResourceVersion matches", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				blockOwnerDeletion := true
				manualEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "manual-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "external-svc",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "v1",
							Kind:               "Endpoints",
							Name:               "external-svc",
							UID:                "endpoints-uid",
							BlockOwnerDeletion: &blockOwnerDeletion,
						}},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.100.50"},
						},
					},
				}
				existingMirror := testing.MirrorEndpointSlice(&manualEndpointSlice, "l3-network", true)
				existingMirror.OwnerReferences = []metav1.OwnerReference{{
					APIVersion:         "v1",
					Kind:               "Endpoints",
					Name:               "external-svc",
					UID:                "endpoints-uid",
					BlockOwnerDeletion: &blockOwnerDeletion,
				}}
				existingMirror.Annotations[types.LabelSourceEndpointSliceVersion] = manualEndpointSlice.ResourceVersion

				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							manualEndpointSlice,
							*existingMirror,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					mirroredEndpointSlices, err := util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, manualEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					if len(mirroredEndpointSlices[0].OwnerReferences) != 0 {
						return fmt.Errorf("expected Endpoints ownerRefs to be stripped, got %#v", mirroredEndpointSlices[0].OwnerReferences)
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should mirror IPv6 selector-less EndpointSlices with non-Pod addresses as-is", func() {
			app.Action = func(*cli.Context) error {
				config.IPv6Mode = true
				namespaceT := *util.NewNamespace("testns-v6")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				blockOwnerDeletion := true
				manualEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "manual-endpointslice-v6",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "external-svc-v6",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "v1",
							Kind:               "Endpoints",
							Name:               "external-svc-v6",
							UID:                "endpoints-uid-v6",
							BlockOwnerDeletion: &blockOwnerDeletion,
						}},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"2001:db8::50"},
						},
					},
				}
				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							manualEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "2014:100:200::0/60/64", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, manualEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 1 || len(mirroredEndpointSlices[0].Endpoints[0].Addresses) != 1 {
						return fmt.Errorf("expected one IPv6 address on mirrored EndpointSlice")
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].AddressType).To(gomega.Equal(discovery.AddressTypeIPv6))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"2001:db8::50"}))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should mirror mixed Pod and non-Pod endpoints from a mirroring-controller EndpointSlice", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod", "", "10.244.2.3", "l3-network", "10.132.2.4")

				mixedEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "mixed-endpointslice",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "mixed-svc",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.2.3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
						{
							Addresses: []string{"10.0.0.25"},
						},
						{
							// non-Pod TargetRef: address must pass through unchanged (not swapped for UDN IP)
							Addresses: []string{"10.0.0.30"},
							TargetRef: &corev1.ObjectReference{
								Kind: "Node",
								Name: "some-node",
							},
						},
					},
				}
				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							mixedEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, mixedEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 3 {
						return fmt.Errorf("expected three Endpoints, got %d", len(mirroredEndpointSlices[0].Endpoints))
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"10.132.2.4"}))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[1].Addresses).To(gomega.BeEquivalentTo([]string{"10.0.0.25"}))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[2].Addresses).To(gomega.BeEquivalentTo([]string{"10.0.0.30"}))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should mirror mixed IPv6 Pod and non-Pod endpoints from a mirroring-controller EndpointSlice", func() {
			app.Action = func(*cli.Context) error {
				config.IPv6Mode = true
				namespaceT := *util.NewNamespace("testns-ipv6-mixed")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				pod := *testing.NewPodWithPrimaryNADIP(namespaceT.Name, "test-pod-v6", "", "2001:db8:1::3", "l3-network", "2014:100:200::4")

				mixedEndpointSliceV6 := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "mixed-endpointslice-v6",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "mixed-svc-v6",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"2001:db8:1::3"},
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Namespace: namespaceT.Name,
								Name:      pod.Name,
							},
						},
						{
							Addresses: []string{"2001:db8:2::25"},
						},
						{
							// non-Pod TargetRef: address must pass through unchanged
							Addresses: []string{"2001:db8:2::30"},
							TargetRef: &corev1.ObjectReference{
								Kind: "Node",
								Name: "some-node-v6",
							},
						},
					},
				}
				objs := []runtime.Object{
					&corev1.PodList{
						Items: []corev1.Pod{
							pod,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							mixedEndpointSliceV6,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "2014:100:200::0/60/64", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				var mirroredEndpointSlices []*discovery.EndpointSlice
				gomega.Eventually(func() error {
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}

					mirroredEndpointSlices, err = util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, mixedEndpointSliceV6.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					if len(mirroredEndpointSlices[0].Endpoints) != 3 {
						return fmt.Errorf("expected three Endpoints, got %d", len(mirroredEndpointSlices[0].Endpoints))
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				gomega.Expect(mirroredEndpointSlices[0].AddressType).To(gomega.Equal(discovery.AddressTypeIPv6))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[0].Addresses).To(gomega.BeEquivalentTo([]string{"2014:100:200::4"}))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[1].Addresses).To(gomega.BeEquivalentTo([]string{"2001:db8:2::25"}))
				gomega.Expect(mirroredEndpointSlices[0].Endpoints[2].Addresses).To(gomega.BeEquivalentTo([]string{"2001:db8:2::30"}))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should delete mirrored EndpointSlice when mirroring-controller source is deleted", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *util.NewNamespace("testns")
				namespaceT.Labels[types.RequiredUDNNamespaceLabel] = ""

				blockOwnerDeletion := true
				manualEndpointSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "manual-endpointslice-del",
						Namespace: namespaceT.Name,
						Labels: map[string]string{
							discovery.LabelServiceName: "external-svc-del",
							discovery.LabelManagedBy:   types.EndpointSliceMirroringControllerName,
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "v1",
							Kind:               "Endpoints",
							Name:               "external-svc-del",
							UID:                "endpoints-uid-del",
							BlockOwnerDeletion: &blockOwnerDeletion,
						}},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.200.50"},
						},
					},
				}
				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							manualEndpointSlice,
						},
					},
				}

				start(objs...)

				nad := testing.GenerateNAD("l3-network", "l3-network", namespaceT.Name, types.Layer3Topology, "10.132.2.0/16/24", types.NetworkRolePrimary)
				_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Create(
					context.TODO(),
					nad,
					metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					_, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespaceT.Name).Get(context.TODO(), "l3-network", metav1.GetOptions{})
					if err != nil {
						return err
					}
					mirroredEndpointSlices, err := util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, manualEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 1 {
						return fmt.Errorf("expected one mirrored EndpointSlice, got %d", len(mirroredEndpointSlices))
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("when the mirroring-controller source EndpointSlice is removed the mirror is cleaned up")
				err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(namespaceT.Name).Delete(context.TODO(), manualEndpointSlice.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					mirroredEndpointSlices, err := util.GetMirroredEndpointSlices(types.EndpointSliceMirrorControllerName, manualEndpointSlice.Name, namespaceT.Name, controller.endpointSliceLister)
					if err != nil {
						return err
					}
					if len(mirroredEndpointSlices) != 0 {
						return fmt.Errorf("expected no mirrored EndpointSlices, got %d", len(mirroredEndpointSlices))
					}
					return nil
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
