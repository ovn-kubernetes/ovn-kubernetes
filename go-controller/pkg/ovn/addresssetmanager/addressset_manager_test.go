package addresssetmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const controllerName = "test-controller"

func getPolicyKeyWithKind(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", policy.Namespace, policy.Name)
}

func eventuallyExpectAddressSetsWithIP(asf *addressset.FakeAddressSetFactory, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(GetPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), controllerName)
		asf.EventuallyExpectAddressSetWithAddresses(dbIDs, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(asf *addressset.FakeAddressSetFactory, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(GetPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), controllerName)
		asf.EventuallyExpectEmptyAddressSetExist(dbIDs)
	}
}

var _ = ginkgo.Describe("OVN PodSelectorAddressSet", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
		netPolicyName1 = "networkpolicy1"
		netPolicyName2 = "networkpolicy2"
		nodeName       = "node1"
		podLabelKey    = "podLabel"
		ip1            = "10.128.1.1"
		ip2            = "10.128.1.2"
		ip3            = "10.128.1.3"
		ip4            = "10.128.1.4"
	)
	var (
		asf               *addressset.FakeAddressSetFactory
		addressSetManager *AddressSetManager
		wf                *factory.WatchFactory
		clientSet         *util.OVNKubeControllerClientset
		initialDB         libovsdbtest.TestSetup
		libovsdbCleanup   *libovsdbtest.Context
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		asf = addressset.NewFakeAddressSetFactory(controllerName)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}
	})

	ginkgo.AfterEach(func() {
		// stop controller
		if addressSetManager != nil {
			addressSetManager.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	startAddrSetManager := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, pods []corev1.Pod) {
		clientSet = util.GetOVNClientset(
			&corev1.NamespaceList{
				Items: namespaces,
			},
			&corev1.PodList{
				Items: pods,
			},
		).GetOVNKubeControllerClientset()
		var err error
		wf, err = factory.NewOVNKubeControllerWatchFactory(clientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(wf.Start()).To(gomega.Succeed())
		var libovsdbNBClient libovsdbclient.Client
		libovsdbNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		addressSetManager = NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), libovsdbNBClient, asf,
			controllerName, &util.DefaultNetInfo{}, func(_ string) string { return "" })
		err = addressSetManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.It("validates selectors", func() {
		// start ovn without any objects
		startAddrSetManager(initialDB, nil, nil)
		namespace := *testing.NewNamespace(namespaceName1)
		networkPolicy := testing.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace.Name,
			"", "label1", true, true)
		// create peer with invalid Operator
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "key",
					Operator: "",
					Values:   []string{"value"},
				}},
			},
		}
		// try to add invalid peer
		peerASKey, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("is not a valid label selector operator"))
		// address set will not be created
		peerASIDs := GetPodSelectorAddrSetDbIDs(peerASKey, controllerName)
		asf.EventuallyExpectNoAddressSet(peerASIDs)

		// add nil pod selector
		peerASKey, _, _, err = addressSetManager.EnsureAddressSet(
			nil, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("pod selector is nil"))
		// address set will not be created
		peerASIDs = GetPodSelectorAddrSetDbIDs(peerASKey, controllerName)
		asf.EventuallyExpectNoAddressSet(peerASIDs)

		// namespace selector is nil and namespace is empty
		peerASKey, _, _, err = addressSetManager.EnsureAddressSet(
			peer.PodSelector, nil, "", getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("namespace selector is nil and namespace is empty"))
		// address set will not be created
		peerASIDs = GetPodSelectorAddrSetDbIDs(peerASKey, controllerName)
		asf.EventuallyExpectNoAddressSet(peerASIDs)
	})
	ginkgo.It("creates one address set for multiple users with the same selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "label1",
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		peerASKey, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, namespace1.Name,
			"backref1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, namespace1.Name,
			"backref2")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(peerASKey, controllerName)
		asf.EventuallyExpectEmptyAddressSetExist(peerASIDs)
		// expect peer address set only
		asf.ExpectNumberOfAddressSets(1)
	})
	ginkgo.DescribeTable("adds selected pod ips to the address set",
		func(peer knet.NetworkPolicyPeer, staticNamespace string, addrSetIPs []string) {
			namespace1 := *testing.NewNamespace(namespaceName1)
			namespace2 := *testing.NewNamespace(namespaceName2)
			ns1pod1 := testing.NewPod(namespace1.Name, "ns1pod1", nodeName, ip1)
			ns1pod2 := testing.NewPod(namespace1.Name, "ns1pod2", nodeName, ip2)
			ns2pod1 := testing.NewPod(namespace2.Name, "ns2pod1", nodeName, ip3)
			ns2pod2 := testing.NewPod(namespace2.Name, "ns2pod2", nodeName, ip4)
			podsList := []corev1.Pod{}
			for _, pod := range []*corev1.Pod{ns1pod1, ns1pod2, ns2pod1, ns2pod2} {
				pod.Labels = map[string]string{podLabelKey: pod.Name}
				podsList = append(podsList, *pod)
			}
			startAddrSetManager(initialDB, []corev1.Namespace{namespace1, namespace2}, podsList)

			peerASKey, _, _, err := addressSetManager.EnsureAddressSet(
				peer.PodSelector, peer.NamespaceSelector, staticNamespace, "backRef")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// address set should be created and pod ips added
			peerASIDs := GetPodSelectorAddrSetDbIDs(peerASKey, controllerName)
			asf.EventuallyExpectAddressSetWithAddresses(peerASIDs, addrSetIPs)
		},
		ginkgo.Entry("all pods from a static namespace", knet.NetworkPolicyPeer{
			PodSelector:       &metav1.LabelSelector{},
			NamespaceSelector: nil,
		}, namespaceName1, []string{ip1, ip2}),
		ginkgo.Entry("selected pods from a static namespace", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
			},
			NamespaceSelector: nil,
		}, namespaceName1, []string{ip1}),
		ginkgo.Entry("all pods from all namespaces", knet.NetworkPolicyPeer{
			PodSelector:       &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{},
		}, namespaceName1, []string{ip1, ip2, ip3, ip4}),
		ginkgo.Entry("selected pods from all namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      podLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"ns1pod1", "ns2pod1"},
					},
				},
			},
			NamespaceSelector: &metav1.LabelSelector{},
		}, namespaceName1, []string{ip1, ip3}),
		ginkgo.Entry("all pods from selected namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName2,
				},
			},
		}, namespaceName1, []string{ip3, ip4}),
		ginkgo.Entry("selected pods from selected namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
			},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName2,
				},
			},
		}, namespaceName1, []string{ip3}),
	)
	ginkgo.It("on initial sync deletes unreferenced and leaves referenced address sets", func() {
		unusedPodSelIDs := GetPodSelectorAddrSetDbIDs("psasName", controllerName)
		unusedPodSelAS, _ := addressset.GetTestDbAddrSets(unusedPodSelIDs, []string{"1.1.1.2"})
		refNetpolIDs := GetPodSelectorAddrSetDbIDs("psasName2", controllerName)
		refNetpolAS, _ := addressset.GetTestDbAddrSets(refNetpolIDs, []string{"1.1.1.3"})
		netpolACL := libovsdbops.BuildACL(
			"netpolACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refNetpolAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		netpolACL.UUID = "netpolACL-UUID"
		refPodSelIDs := GetPodSelectorAddrSetDbIDs("pasName2", controllerName)
		refPodSelAS, _ := addressset.GetTestDbAddrSets(refPodSelIDs, []string{"1.1.1.4"})
		podSelACL := libovsdbops.BuildACL(
			"podSelACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refPodSelAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		podSelACL.UUID = "podSelACL-UUID"

		initialDb := []libovsdbtest.TestData{
			unusedPodSelAS,
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		dbSetup := libovsdbtest.TestSetup{NBData: initialDb}
		startAddrSetManager(dbSetup, nil, nil)

		finalDB := []libovsdbtest.TestData{
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(finalDB))
	})
	ginkgo.It("reconciles a completed and deleted pod whose IP has been assigned to a running pod", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, namespace1.Name, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(asf, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed)
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, an update event will be generated
		completedPod.Status.Phase = corev1.PodSucceeded
		_, err = clientSet.KubeClient.CoreV1().Pods(completedPod.Namespace).Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// make sure the delete event is handled and address set is not changed
		time.Sleep(100 * time.Millisecond)
		// Running pod policy should not be affected by pod deletions
		eventuallyExpectAddressSetsWithIP(asf, peer, namespace1.Name, podIP)
	})
	ginkgo.It("reconciles a completed pod whose IP has been assigned to a running pod with non-matching namespace selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		namespace2 := *testing.NewNamespace(namespaceName2)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName1,
				},
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1, namespace2}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, namespace1.Name, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(asf, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed). This pod has another namespace that is not matched by the address set
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace2.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace2.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, so delete event will be generated
		completedPod.Status.Phase = corev1.PodSucceeded
		_, err = clientSet.KubeClient.CoreV1().Pods(completedPod.Namespace).Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// IP should be deleted from the address set on delete event, since the new pod with the same ip
		// should not be present in given address set
		eventuallyExpectEmptyAddressSetsExist(asf, peer, namespace1.Name)
	})
})

var _ = ginkgo.Describe("shortLabelSelectorString function", func() {
	ginkgo.It("handles LabelSelectorRequirement.Values order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v1", "v2", "v3"},
			}},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v3", "v2", "v1"},
			}},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchExpressions order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchLabels order", func() {
		ls1 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"},
		}
		ls2 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k2": "v2", "k1": "v1", "k3": "v3"},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
})
