// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"net"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	allocationTestNamespace = "default"
	allocationTestNAD       = "default/nad"
	allocationTestPod       = "test-pod"
)

// newAllocationTestController returns a network cluster controller for a
// primary layer2 network that is not started yet.
func newAllocationTestController(t *testing.T, g *gomega.WithT, netConf *ovncnitypes.NetConf,
	fakeClient *util.OVNClusterManagerClientset) (*networkClusterController, *factory.WatchFactory) {
	t.Helper()
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	t.Cleanup(wf.Shutdown)

	nm := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{allocationTestNamespace: netInfo},
		NADNetworks:     map[string]util.NetInfo{allocationTestNAD: netInfo},
	}
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	t.Cleanup(nodeController.Stop)

	allocControllers := newAllocationControllers(wf)
	g.Expect(allocControllers.Start()).To(gomega.Succeed())
	t.Cleanup(allocControllers.Stop)

	ncc := newNetworkClusterController(netInfo, fakeClient, wf, record.NewFakeRecorder(10), nm, nil, nodeController, allocControllers)
	t.Cleanup(ncc.Stop)
	return ncc, wf
}

func deletedObjectsOf[T any](c *networkObjectController[T], netName, key string) []*T {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.networks[netName].deleted.get(key)
}

func allocationTestPodAndNode(uid string) (*corev1.Pod, *corev1.Node) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationTestPod,
			Namespace: allocationTestNamespace,
			UID:       k8stypes.UID(uid),
		},
		Spec: corev1.PodSpec{NodeName: "node1"},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	return pod, node
}

func podIPs(pod *corev1.Pod) []*net.IPNet {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, allocationTestNAD)
	if err != nil {
		return nil
	}
	return podAnnotation.IPs
}

func TestNetworkClusterControllerPodAllocation(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	pod, node := allocationTestPodAndNode("uid-1")
	otherPod := pod.DeepCopy()
	otherPod.Namespace = "other"
	fakeClient := util.GetOVNClientset(node, pod, otherPod).GetClusterManagerClientset()
	ncc, _ := newAllocationTestController(t, g, &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "pod_allocation_test", Type: "ovn-k8s-cni-overlay"},
		Topology: types.Layer2Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.60.0.0/24",
	}, fakeClient)
	networkName := ncc.GetNetworkName()
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())

	allocatedIPs := func(uid string) func() []*net.IPNet {
		return func() []*net.IPNet {
			current, err := fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Get(context.Background(), allocationTestPod, metav1.GetOptions{})
			if err != nil || string(current.UID) != uid {
				return nil
			}
			return podIPs(current)
		}
	}

	t.Log("only the pods of the namespace of the network are on it")
	g.Expect(ncc.podAllocator.PodOnNetwork(pod)).To(gomega.BeTrue())
	g.Expect(ncc.podAllocator.PodOnNetwork(otherPod)).To(gomega.BeFalse())

	t.Log("an existing pod is allocated")
	g.Eventually(allocatedIPs("uid-1"), 3*time.Second).ShouldNot(gomega.BeEmpty())
	firstIPs := allocatedIPs("uid-1")()

	t.Log("only the pods that were allocated on the network may have something to release on it")
	allocated, err := fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Get(context.Background(), allocationTestPod, metav1.GetOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(ncc.podAllocator.DeletedPodOnNetwork(allocated)).To(gomega.BeTrue())
	g.Expect(ncc.podAllocator.DeletedPodOnNetwork(otherPod)).To(gomega.BeFalse())

	t.Log("a pod replaced by one with the same name is allocated, and what the replaced one had is released")
	replacement, _ := allocationTestPodAndNode("uid-2")
	_, err = fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Update(context.Background(), replacement, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(allocatedIPs("uid-2"), 3*time.Second).ShouldNot(gomega.BeEmpty())
	secondIPs := allocatedIPs("uid-2")()
	if !secondIPs[0].IP.Equal(firstIPs[0].IP) {
		g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", firstIPs)).To(gomega.Succeed())
	}

	t.Log("what a deleted pod had is released")
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", secondIPs)).NotTo(gomega.Succeed())
	err = fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Delete(context.Background(), allocationTestPod, metav1.DeleteOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() error {
		return ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", secondIPs)
	}, 3*time.Second).Should(gomega.Succeed())
	g.Expect(deletedObjectsOf(ncc.allocationControllers.pods, networkName, allocationTestNamespace+"/"+allocationTestPod)).To(gomega.BeEmpty())
}

// What each of two pods that had the same name was allocated is released.
func TestNetworkClusterControllerReleasesDeletedPods(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	pod, node := allocationTestPodAndNode("uid-1")
	fakeClient := util.GetOVNClientset(node, pod).GetClusterManagerClientset()
	ncc, wf := newAllocationTestController(t, g, &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "deleted_pods_test", Type: "ovn-k8s-cni-overlay"},
		Topology: types.Layer2Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.61.0.0/24",
	}, fakeClient)
	networkName := ncc.GetNetworkName()
	ctx := context.Background()
	key := allocationTestNamespace + "/" + allocationTestPod
	// the controller is initialized but not started: the test plays its part
	g.Expect(ncc.init(ctx)).To(gomega.Succeed())

	allocatedPod := func(uid string) *corev1.Pod {
		var allocated *corev1.Pod
		g.Eventually(func() []*net.IPNet {
			current, err := wf.GetPod(allocationTestNamespace, allocationTestPod)
			if err != nil || string(current.UID) != uid {
				return nil
			}
			allocated = current
			return podIPs(current)
		}, 3*time.Second).ShouldNot(gomega.BeEmpty())
		return allocated
	}

	g.Expect(ncc.reconcilePod(ctx, key)).To(gomega.Succeed())
	firstPod := allocatedPod("uid-1")

	replacement, _ := allocationTestPodAndNode("uid-2")
	_, err := fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Update(ctx, replacement, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() string {
		current, err := wf.GetPod(allocationTestNamespace, allocationTestPod)
		if err != nil {
			return ""
		}
		return string(current.UID)
	}, 3*time.Second).Should(gomega.Equal("uid-2"))
	// the first pod is not released yet: the second one gets other IPs
	g.Expect(ncc.reconcilePod(ctx, key)).To(gomega.Succeed())
	secondPod := allocatedPod("uid-2")
	g.Expect(podIPs(secondPod)[0].IP.Equal(podIPs(firstPod)[0].IP)).To(gomega.BeFalse())

	g.Expect(fakeClient.KubeClient.CoreV1().Pods(allocationTestNamespace).Delete(ctx, allocationTestPod, metav1.DeleteOptions{})).To(gomega.Succeed())
	g.Eventually(func() bool {
		_, err := wf.GetPod(allocationTestNamespace, allocationTestPod)
		return apierrors.IsNotFound(err)
	}, 3*time.Second).Should(gomega.BeTrue())

	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", podIPs(firstPod))).NotTo(gomega.Succeed())
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", podIPs(secondPod))).NotTo(gomega.Succeed())

	g.Expect(ncc.releasePod(ctx, firstPod)).To(gomega.Succeed())
	g.Expect(ncc.releasePod(ctx, secondPod)).To(gomega.Succeed())
	g.Expect(ncc.reconcilePod(ctx, key)).To(gomega.Succeed())
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", podIPs(firstPod))).To(gomega.Succeed())
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", podIPs(secondPod))).To(gomega.Succeed())
}

func TestPodNeedsAllocation(t *testing.T) {
	g := gomega.NewWithT(t)
	pod, _ := allocationTestPodAndNode("uid-1")
	g.Expect(podNeedsAllocation(nil, pod)).To(gomega.BeTrue())

	unscheduled := pod.DeepCopy()
	unscheduled.Spec.NodeName = ""
	g.Expect(podNeedsAllocation(nil, unscheduled)).To(gomega.BeFalse())
	g.Expect(podNeedsAllocation(unscheduled, pod)).To(gomega.BeTrue())

	hostNetwork := pod.DeepCopy()
	hostNetwork.Spec.HostNetwork = true
	g.Expect(podNeedsAllocation(nil, hostNetwork)).To(gomega.BeFalse())
}

func TestNetworkClusterControllerIPAMClaimRelease(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnablePersistentIPs = true

	netConf := &ovncnitypes.NetConf{
		NetConf:            cnitypes.NetConf{Name: "ipamclaim_release_test", Type: "ovn-k8s-cni-overlay"},
		Topology:           types.Layer2Topology,
		Role:               types.NetworkRolePrimary,
		Subnets:            "10.70.0.0/24",
		AllowPersistentIPs: true,
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()

	const claimName = "test-claim"
	claim := &ipamclaimsapi.IPAMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: allocationTestNamespace,
			UID:       k8stypes.UID("claim-uid-1"),
		},
		Spec:   ipamclaimsapi.IPAMClaimSpec{Network: networkName},
		Status: ipamclaimsapi.IPAMClaimStatus{IPs: []string{"10.70.0.5/24"}},
	}
	firstIPs, err := util.ParseIPNets(claim.Status.IPs)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	fakeClient.IPAMClaimsClient = fakeipamclaimclient.NewSimpleClientset(claim)
	ncc, _ := newAllocationTestController(t, g, netConf, fakeClient)
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())

	t.Log("the IPs of an existing IPAMClaim are reserved")
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", firstIPs)).NotTo(gomega.Succeed())

	t.Log("the IPs of an IPAMClaim replaced by one with the same name are released")
	replacement := claim.DeepCopy()
	replacement.UID = k8stypes.UID("claim-uid-2")
	replacement.Status.IPs = []string{"10.70.0.6/24"}
	secondIPs, err := util.ParseIPNets(replacement.Status.IPs)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// what a pod using the new IPAMClaim would have done
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, persistentips.IPAMClaimOwner(replacement), secondIPs)).To(gomega.Succeed())
	_, err = fakeClient.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(allocationTestNamespace).Update(context.Background(), replacement, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() error {
		return ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", firstIPs)
	}, 3*time.Second).Should(gomega.Succeed())

	t.Log("the IPs of a deleted IPAMClaim are released")
	g.Expect(ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", secondIPs)).NotTo(gomega.Succeed())
	err = fakeClient.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(allocationTestNamespace).Delete(context.Background(), claimName, metav1.DeleteOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() error {
		return ncc.subnetAllocator.AllocateIPs(networkName, "test-owner", secondIPs)
	}, 3*time.Second).Should(gomega.Succeed())
	g.Expect(deletedObjectsOf(ncc.allocationControllers.ipamClaims, networkName, allocationTestNamespace+"/"+claimName)).To(gomega.BeEmpty())
}
