// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const vmName = "test-vm"

var _ = Describe("Kubevirt Pod", func() {
	const (
		t0 = time.Duration(0)
		t1 = time.Duration(1)
		t2 = time.Duration(2)
		t3 = time.Duration(3)
		t4 = time.Duration(4)
	)
	runningKvSourcePod := runningKubevirtPod(t0)
	successfullyMigratedKvSourcePod := completedKubevirtPod(t1)

	failedMigrationKvTargetPod := failedKubevirtPod(t2)
	successfulMigrationKvTargetPod := runningKubevirtPod(t3)
	anotherFailedMigrationKvTargetPod := failedKubevirtPod(t4)
	duringMigrationKvTargetPod := runningKubevirtPod(t4)
	yetAnotherDuringMigrationKvTargetPod := runningKubevirtPod(t4)
	readyMigrationKvTargetPod := domainReadyKubevirtPod(t4)

	// Pods for hostname mismatch scenario (vm.kubevirt.io/name label != kubevirt.io/domain annotation)
	hostnameVMName := "real-vm-name"
	hostname := "shared-hostname"
	runningKvSourcePodWithHostname := newKubevirtPodWithHostname(corev1.PodRunning, t0, hostnameVMName, hostname)
	duringMigrationKvTargetPodWithHostname := newKubevirtPodWithHostname(corev1.PodRunning, t4, hostnameVMName, hostname)

	// Pods for long VM name scenario (label truncated to 63 chars)
	longVMName := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters"
	runningKvSourcePodWithLongName := newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName)
	duringMigrationKvTargetPodWithLongName := newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName)

	// Pods for colliding long VM names scenario (different VMs share the same truncated 63-char label)
	collidingLongVMName := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-collider"
	collidingVMPod := newKubevirtPodWithLongName(corev1.PodRunning, t2, collidingLongVMName)

	initWatchFactory := func(pods []corev1.Pod) (*factory.WatchFactory, func()) {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
		wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
		Expect(err).ToNot(HaveOccurred())

		for _, pod := range pods {
			_, err := fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), &pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(wf.Start()).To(Succeed())
		return wf, wf.Shutdown
	}

	type testParams struct {
		pods                    []corev1.Pod
		expectedError           error
		expectedMigrationStatus *LiveMigrationStatus
	}
	DescribeTable("DiscoverLiveMigrationStatus", func(params testParams) {
		wf, shutdown := initWatchFactory(params.pods)
		defer shutdown()

		currentPod := params.pods[0]
		migrationStatus, err := DiscoverLiveMigrationStatus(wf.PodCoreInformer().Lister(), &currentPod)
		if params.expectedError == nil {
			Expect(err).ToNot(HaveOccurred())
		} else {
			Expect(err).To(MatchError(ContainSubstring(params.expectedError.Error())))
		}

		if params.expectedMigrationStatus == nil {
			Expect(migrationStatus).To(BeNil())
		} else {
			Expect(migrationStatus.State).To(Equal(params.expectedMigrationStatus.State))
			Expect(migrationStatus.TargetPod.Name).To(Equal(params.expectedMigrationStatus.TargetPod.Name))
			if params.expectedMigrationStatus.SourcePod == nil {
				Expect(migrationStatus.SourcePod).To(BeNil())
			} else {
				Expect(migrationStatus.SourcePod.Name).To(Equal(params.expectedMigrationStatus.SourcePod.Name))
			}
		}
	},
		Entry("returns nil when pod is not kubevirt related",
			testParams{
				pods: []corev1.Pod{nonKubevirtPod()},
			},
		),
		Entry("returns nil when migration was not performed",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod},
			},
		),
		Entry("returns nil when there is no active migration",
			testParams{
				pods: []corev1.Pod{successfullyMigratedKvSourcePod, successfulMigrationKvTargetPod},
			},
		),
		Entry("returns nil when there is no active migration (multiple migrations)",
			testParams{
				pods: []corev1.Pod{successfullyMigratedKvSourcePod, failedMigrationKvTargetPod, successfulMigrationKvTargetPod},
			},
		),
		Entry("returns nil when there is all the pods are completed (not running vm after migration)",
			testParams{
				pods: []corev1.Pod{completedKubevirtPod(t0), completedKubevirtPod(t1), completedKubevirtPod(t3)},
			},
		),
		Entry("returns Migration in progress status when 2 pods are running, target pod is not yet ready",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, duringMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &duringMigrationKvTargetPod,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("returns Migration Failed status when latest target pod failed",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &failedMigrationKvTargetPod,
					State:     LiveMigrationFailed,
				},
			},
		),
		Entry("returns Migration Failed status when latest target pod failed (multiple migrations)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod, anotherFailedMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &anotherFailedMigrationKvTargetPod,
					State:     LiveMigrationFailed,
				},
			},
		),
		Entry("returns Migration Ready status when latest target pod is ready",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns Migration Ready status when latest target pod is ready (multiple migrations)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod, readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns Migration Ready status when source pod is gone and target has ready annotation",
			testParams{
				pods: []corev1.Pod{readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns Migration Ready status when source pod is completed and target has ready annotation",
			testParams{
				pods: []corev1.Pod{readyMigrationKvTargetPod, successfullyMigratedKvSourcePod},
				expectedMigrationStatus: &LiveMigrationStatus{
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns nil when source pod is gone and target has no ready annotation",
			testParams{
				pods: []corev1.Pod{duringMigrationKvTargetPod},
			},
		),
		Entry("returns err when kubevirt VM has several living pods",
			testParams{
				pods:          []corev1.Pod{runningKvSourcePod, duringMigrationKvTargetPod, yetAnotherDuringMigrationKvTargetPod},
				expectedError: fmt.Errorf("unexpected live migration state at pods"),
			},
		),
		Entry("discovers migration when VM has hostname configured (vmName label != domain annotation)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithHostname, duringMigrationKvTargetPodWithHostname},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithHostname,
					TargetPod: &duringMigrationKvTargetPodWithHostname,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("discovers migration when VM name exceeds 63 chars (vmName label truncated)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithLongName, duringMigrationKvTargetPodWithLongName},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithLongName,
					TargetPod: &duringMigrationKvTargetPodWithLongName,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("discovers migration when another VM has colliding truncated vmName label",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithLongName, duringMigrationKvTargetPodWithLongName, collidingVMPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithLongName,
					TargetPod: &duringMigrationKvTargetPodWithLongName,
					State:     LiveMigrationInProgress,
				},
			},
		),
	)

	const (
		localIP       = "10.244.0.5/24"
		otherNodeIP   = "10.244.1.5/24"
		releasedIP    = "10.244.2.5/24"
		localIPv6     = "fd00:10:244:1::5/64"
		otherNodeIPv6 = "fd00:10:244:2::5/64"
	)
	type otherPod struct {
		phase corev1.PodPhase
		// ips are the default network IPs of the pod, none for a pod without OVN annotation.
		ips []string
		// mirror makes the pod a mirror pod.
		mirror bool
	}
	DescribeTable("findPodAnnotation", func(otherPods []otherPod, expectedIPs []string) {
		const (
			namespace = "ns1"
			nodeName  = "node1"
		)
		vmPod := func(name string, phase corev1.PodPhase, annotations map[string]string) corev1.Pod {
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
					UID:       ktypes.UID(name),
					Labels: map[string]string{
						kubevirtv1.AppLabel:                "virt-launcher",
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					Annotations: map[string]string{
						kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
						kubevirtv1.DomainAnnotation:                             vmName,
					},
				},
				Spec:   corev1.PodSpec{NodeName: nodeName},
				Status: corev1.PodStatus{Phase: phase},
			}
			for k, v := range annotations {
				pod.Annotations[k] = v
			}
			return pod
		}
		pods := []corev1.Pod{vmPod("new", corev1.PodPending, nil)}
		for i, other := range otherPods {
			var annotations map[string]string
			if len(other.ips) > 0 {
				ips, err := json.Marshal(other.ips)
				Expect(err).ToNot(HaveOccurred(), "failed to marshal the IPs of pod other-%d", i)
				annotations = map[string]string{
					ovntypes.OvnPodAnnotationName: fmt.Sprintf(`{"default":{"ip_addresses":%s,"mac_address":"0a:58:0a:f4:00:05"}}`, ips),
				}
			}
			if other.mirror {
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations[corev1.MirrorPodAnnotationKey] = "hash"
			}
			pods = append(pods, vmPod(fmt.Sprintf("other-%d", i), other.phase, annotations))
		}

		Expect(config.PrepareTestConfig()).To(Succeed(), "failed to prepare the test config")
		fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
		_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        nodeName,
				Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":["10.244.0.0/24","fd00:10:244:1::/64"]}`},
			},
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred(), "failed to create node %s", nodeName)
		for _, pod := range pods {
			_, err := fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), &pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred(), "failed to create pod %s", pod.Name)
		}
		wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, nodeName)
		Expect(err).ToNot(HaveOccurred(), "failed to create the watch factory")
		defer wf.Shutdown()
		Expect(wf.Start()).To(Succeed(), "failed to start the watch factory")

		podAnnotation, err := findPodAnnotation(wf, &pods[0], ovntypes.DefaultNetworkName)
		Expect(err).ToNot(HaveOccurred(), "findPodAnnotation must not fail")
		if len(expectedIPs) == 0 {
			Expect(podAnnotation).To(BeNil(), "no pod annotation must be inherited")
			return
		}
		Expect(podAnnotation).ToNot(BeNil(), "a pod annotation must be inherited")
		var ips []string
		for _, ip := range podAnnotation.IPs {
			ips = append(ips, ip.String())
		}
		Expect(ips).To(Equal(expectedIPs), "unexpected inherited IPs")
	},
		Entry("returns nil when the virtual machine has no other pod",
			nil, nil),
		Entry("returns the IPs of another node held by a running pod",
			[]otherPod{{corev1.PodRunning, []string{otherNodeIP}, false}}, []string{otherNodeIP}),
		Entry("returns the IPs of the running pod when a completed pod listed before it has other IPs",
			[]otherPod{{corev1.PodSucceeded, []string{releasedIP}, false}, {corev1.PodRunning, []string{otherNodeIP}, false}}, []string{otherNodeIP}),
		Entry("returns the IPs of the running pod when a completed pod listed after it has other IPs",
			[]otherPod{{corev1.PodRunning, []string{otherNodeIP}, false}, {corev1.PodSucceeded, []string{releasedIP}, false}}, []string{otherNodeIP}),
		Entry("returns the IPs of the pod's node held by a completed pod when the running pod has none",
			[]otherPod{{corev1.PodSucceeded, []string{localIP}, false}, {corev1.PodRunning, nil, false}}, []string{localIP}),
		Entry("returns nil for the IPs of another node held by a completed pod when the running pod has none",
			[]otherPod{{corev1.PodSucceeded, []string{releasedIP}, false}, {corev1.PodRunning, nil, false}}, nil),
		Entry("returns nil for the IPs of another node when all the other pods are completed",
			[]otherPod{{corev1.PodSucceeded, []string{releasedIP}, false}, {corev1.PodFailed, []string{otherNodeIP}, false}}, nil),
		Entry("returns the IPs of the pod's node when all the other pods are completed",
			[]otherPod{{corev1.PodSucceeded, []string{localIP}, false}}, []string{localIP}),
		Entry("returns the IPs of the pod's node when a completed pod listed before has IPs of another node",
			[]otherPod{{corev1.PodSucceeded, []string{releasedIP}, false}, {corev1.PodFailed, []string{localIP}, false}}, []string{localIP}),
		Entry("returns the IPs of the pod's node when a completed pod listed after has IPs of another node",
			[]otherPod{{corev1.PodFailed, []string{localIP}, false}, {corev1.PodSucceeded, []string{releasedIP}, false}}, []string{localIP}),
		Entry("returns nil for the IPs held by a running mirror pod",
			[]otherPod{{corev1.PodRunning, []string{otherNodeIP}, true}}, nil),
		Entry("returns the IPs of the running pod when a running mirror pod has other IPs",
			[]otherPod{{corev1.PodRunning, []string{releasedIP}, true}, {corev1.PodRunning, []string{otherNodeIP}, false}}, []string{otherNodeIP}),
		Entry("returns both IPs of the pod's node held by a completed dual-stack pod",
			[]otherPod{{corev1.PodSucceeded, []string{localIP, localIPv6}, false}}, []string{localIP, localIPv6}),
		Entry("returns nil when one IP of a completed dual-stack pod is of another node",
			[]otherPod{{corev1.PodSucceeded, []string{localIP, otherNodeIPv6}, false}}, nil),
	)

	Describe("NewVMDescriptionFromPod", func() {
		It("returns nil for non-kubevirt pod", func() {
			pod := nonKubevirtPod()
			desc, err := NewVMDescriptionFromPod(&pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(desc).To(BeNil())
		})

		It("returns correct key and finds owned pods for standard pod (vmName label == domain)", func() {
			sourcePod := runningKvSourcePod
			targetPod := duringMigrationKvTargetPod

			wf, shutdown := initWatchFactory([]corev1.Pod{sourcePod, targetPod, nonKubevirtPod()})
			defer shutdown()

			desc, err := NewVMDescriptionFromPod(&sourcePod)
			Expect(err).ToNot(HaveOccurred())
			Expect(desc).ToNot(BeNil())
			Expect(desc.Key().Name).To(Equal(vmName))

			pods, err := desc.OwnedPods(wf.PodCoreInformer().Lister())
			Expect(err).ToNot(HaveOccurred())
			Expect(pods).To(HaveLen(2))
			for _, p := range pods {
				Expect(p.Annotations[kubevirtv1.DomainAnnotation]).To(Equal(vmName))
			}
		})

		It("does not find a pod carrying the VM name label and domain without the virt-launcher label", func() {
			sourcePod := runningKvSourcePod
			otherPod := nonKubevirtPod()
			otherPod.Namespace = sourcePod.Namespace
			otherPod.Labels = map[string]string{kubevirtv1.VirtualMachineNameLabel: sourcePod.Labels[kubevirtv1.VirtualMachineNameLabel]}
			otherPod.Annotations = map[string]string{kubevirtv1.DomainAnnotation: vmName}

			wf, shutdown := initWatchFactory([]corev1.Pod{sourcePod, otherPod})
			defer shutdown()

			desc, err := NewVMDescriptionFromPod(&sourcePod)
			Expect(err).ToNot(HaveOccurred(), "failed to get the VM description of pod %s", sourcePod.Name)
			Expect(desc).ToNot(BeNil(), "pod %s must have a VM description", sourcePod.Name)

			pods, err := desc.OwnedPods(wf.PodCoreInformer().Lister())
			Expect(err).ToNot(HaveOccurred(), "failed to list the pods of the VM")
			Expect(pods).To(HaveLen(1), "only the virt-launcher pod must be found")
			Expect(pods[0].Name).To(Equal(sourcePod.Name), "unexpected pod of the VM")
		})

		type vmDescriptionParams struct {
			pods           []corev1.Pod
			expectedVMName string
			expectedPods   int
		}
		DescribeTable("returns correct key and finds only owned pods",
			func(params vmDescriptionParams) {
				allPods := append(params.pods, nonKubevirtPod())
				wf, shutdown := initWatchFactory(allPods)
				defer shutdown()

				sourcePod := params.pods[0]
				desc, err := NewVMDescriptionFromPod(&sourcePod)
				Expect(err).ToNot(HaveOccurred())
				Expect(desc).ToNot(BeNil())
				Expect(desc.Key().Name).To(Equal(params.expectedVMName))

				pods, err := desc.OwnedPods(wf.PodCoreInformer().Lister())
				Expect(err).ToNot(HaveOccurred())
				Expect(pods).To(HaveLen(params.expectedPods))
				for _, p := range pods {
					Expect(p.Annotations[kubevirtv1.DomainAnnotation]).To(Equal(params.expectedVMName))
				}
			},
			Entry("when VM has hostname (vmName label != domain annotation)",
				vmDescriptionParams{
					pods: []corev1.Pod{
						newKubevirtPodWithHostname(corev1.PodRunning, t0, hostnameVMName, hostname),
						newKubevirtPodWithHostname(corev1.PodRunning, t4, hostnameVMName, hostname),
						newKubevirtPodWithHostname(corev1.PodRunning, t2, "other-vm", hostname),
					},
					expectedVMName: hostnameVMName,
					expectedPods:   2,
				},
			),
			Entry("when VM name exceeds 63 chars (truncated vmName label)",
				vmDescriptionParams{
					pods: []corev1.Pod{
						newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName),
						newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName),
					},
					expectedVMName: longVMName,
					expectedPods:   2,
				},
			),
			Entry("when multiple VMs share the same truncated vmName label",
				vmDescriptionParams{
					pods: func() []corev1.Pod {
						longVMName1 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm1"
						longVMName2 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm2"
						longVMName3 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm3"
						return []corev1.Pod{
							newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName1),
							newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName1),
							newKubevirtPodWithLongName(corev1.PodRunning, t2, longVMName2),
							newKubevirtPodWithLongName(corev1.PodRunning, t3, longVMName3),
						}
					}(),
					expectedVMName: "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm1",
					expectedPods:   2,
				},
			),
		)

		It("returns error when domain annotation is missing", func() {
			pod := corev1.Pod{
				TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "virt-launcher-broken",
					Namespace: corev1.NamespaceDefault,
					Labels:    map[string]string{kubevirtv1.AppLabel: "virt-launcher"},
				},
			}
			desc, err := NewVMDescriptionFromPod(&pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing the mandatory kubevirt annotation"))
			Expect(desc).To(BeNil())
		})
	})
})

func completedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodSucceeded, creationOffset)
}

func failedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodFailed, creationOffset)
}

func runningKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodRunning, creationOffset)
}

func domainReadyKubevirtPod(creationOffset time.Duration) corev1.Pod {
	virtLauncherPod := newKubevirtPod(corev1.PodRunning, creationOffset)
	virtLauncherPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = "some-timestamp"
	return virtLauncherPod
}

func nonKubevirtPod() corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-pod",
			Namespace: corev1.NamespaceDefault,
		},
		Spec: corev1.PodSpec{},
	}
}

func newKubevirtPod(phase corev1.PodPhase, creationOffset time.Duration) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + vmName + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: vmName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: vmName},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func newKubevirtPodWithHostname(phase corev1.PodPhase, creationOffset time.Duration, vmName, hostname string) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + vmName + "-" + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: vmName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: hostname},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func newKubevirtPodWithLongName(phase corev1.PodPhase, creationOffset time.Duration, longVMName string) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + longVMName[:40] + "-" + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: longVMName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: longVMName[:63]},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}
