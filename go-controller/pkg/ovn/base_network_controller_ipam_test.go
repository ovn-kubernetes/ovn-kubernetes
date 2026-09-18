// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Pod IP ownership", func() {
	DescribeTable("releases conflicting startup allocations independently of cleanup order",
		func(defaultNetwork, ownerFirst bool) {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableMultiNetwork = !defaultNetwork
			const namespace, nodeName = "namespace1", "node-a"

			nadKey, switchName := types.DefaultNetworkName, nodeName
			ipString, subnet, role := "10.128.1.30/24", "10.128.1.0/24", types.NetworkRolePrimary
			var nad *nadapi.NetworkAttachmentDefinition
			if !defaultNetwork {
				nadKey = namespace + "/rednad"
				switchName = util.GetUserDefinedNetworkPrefix("bluenet") + nodeName
				ipString, subnet, role = "100.128.0.3/16", "100.128.0.0/16", types.NetworkRoleSecondary
				nad = ovntest.GenerateNAD("bluenet", "rednad", namespace,
					types.Layer3Topology, subnet, role)
				ovntest.AnnotateNADWithNetworkID("3", nad)
			}

			ips := ovntest.MustParseIPNets(ipString)
			newAnnotatedPod := func(name string) *corev1.Pod {
				pod := ovntest.NewPod(namespace, name, nodeName, ips[0].IP.String())
				pod.Annotations = map[string]string{}
				if !defaultNetwork {
					pod.Annotations[nadapi.NetworkAttachmentAnnot] = nadKey
				}
				var err error
				pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
					IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP), Role: role,
				}, nadKey)
				Expect(err).NotTo(HaveOccurred())
				return pod
			}
			completedPod := newAnnotatedPod("completed")
			completedPod.UID = "completed-uid"
			completedPod.Status.Phase = corev1.PodSucceeded
			ownerPod := newAnnotatedPod("owner")
			ownerPod.UID = "owner-uid"

			ownerPortName := util.GetLogicalPortName(namespace, ownerPod.Name)
			externalIDs := map[string]string{"pod": "true", "namespace": namespace}
			if !defaultNetwork {
				ownerPortName = util.GetUserDefinedNetworkLogicalPortName(namespace, ownerPod.Name, nadKey)
				externalIDs[types.NetworkExternalID] = "bluenet"
				externalIDs[types.NADExternalID] = nadKey
				externalIDs[types.TopologyExternalID] = types.Layer3Topology
			}
			ownerPort := &nbdb.LogicalSwitchPort{
				UUID:        "owner-port-UUID",
				Name:        ownerPortName,
				Addresses:   []string{util.IPAddrToHWAddr(ips[0].IP).String() + " " + ips[0].IP.String()},
				Options:     map[string]string{"iface-id-ver": string(ownerPod.UID), "requested-chassis": chassisIDForNode(nodeName)},
				ExternalIDs: externalIDs,
			}
			objects := []runtime.Object{
				completedPod, ownerPod, newNode(nodeName, "192.0.2.10/24"), ovntest.NewNamespace(namespace),
			}
			if nad != nil {
				objects = append(objects, &nadapi.NetworkAttachmentDefinitionList{
					Items: []nadapi.NetworkAttachmentDefinition{*nad},
				})
			}

			fakeOVN := NewFakeOVN(true, nodeName)
			fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{Name: switchName, Ports: []string{ownerPort.UUID}}, ownerPort,
			}}, objects...)
			DeferCleanup(fakeOVN.shutdown)

			baseController := &fakeOVN.controller.BaseNetworkController
			cleanup := func(pod *corev1.Pod) error {
				_, err := fakeOVN.controller.ReconcilePod(pod, nil, nil)
				return err
			}
			syncPods := fakeOVN.controller.SyncPods
			if !defaultNetwork {
				Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
				udnController := fakeOVN.userDefinedNetworkControllers["bluenet"].bnc
				baseController = &udnController.BaseNetworkController
				syncPods = udnController.SyncPods
				cleanup = func(pod *corev1.Pod) error {
					_, err := udnController.ReconcilePod(pod, nil, nil)
					if err == nil {
						udnController.PodLifecycleComplete(pod)
					}
					return err
				}
			}
			Expect(baseController.lsManager.AddOrUpdateSwitch(
				switchName, ovntest.MustParseIPNets(subnet), nil)).To(Succeed())
			Expect(syncPods([]*corev1.Pod{completedPod, ownerPod})).To(Succeed())
			Expect(baseController.wasPodReleasedBeforeStartup(string(completedPod.UID), nadKey)).To(BeTrue())
			Expect(baseController.lsManager.AllocateIPs(switchName, ips)).To(MatchError(ipallocator.ErrAllocated))

			cleanupOrder := []*corev1.Pod{completedPod, ownerPod}
			if ownerFirst {
				cleanupOrder = []*corev1.Pod{ownerPod, completedPod}
			}
			for _, pod := range cleanupOrder {
				Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Delete(
					context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Eventually(func() error {
					_, err := baseController.watchFactory.GetPod(namespace, pod.Name)
					return err
				}).Should(HaveOccurred())
				Expect(cleanup(pod)).To(Succeed())
			}

			ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(
				fakeOVN.nbClient, func(*nbdb.LogicalSwitchPort) bool { return true })
			Expect(err).NotTo(HaveOccurred())
			Expect(ports).To(BeEmpty())
			Expect(baseController.lsManager.AllocateIPs(switchName, ips)).To(Succeed())
		},
		Entry("default network, completed pod first", true, false),
		Entry("default network, live owner first", true, true),
		Entry("UDN, completed pod first", false, false),
		Entry("UDN, live owner first", false, true),
	)
})
