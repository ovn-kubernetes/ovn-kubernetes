// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const controllerName = "test-controller"

func getPolicyKeyWithKind(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", policy.Namespace, policy.Name)
}

func eventuallyExpectAddressSetsWithIP(nbClient libovsdbclient.Client, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip})
		gomega.Eventually(nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	}
}

func eventuallyExpectEmptyAddressSetsExist(nbClient libovsdbclient.Client, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
		gomega.Eventually(nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	}
}

// this is a test wrapper to count db transactions and optionally inject failures
type countingClient struct {
	libovsdbclient.Client
	transactCount atomic.Int64
	failRemaining atomic.Int64
}

func (c *countingClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.transactCount.Add(1)
	for {
		n := c.failRemaining.Load()
		if n <= 0 {
			break
		}
		if c.failRemaining.CompareAndSwap(n, n-1) {
			return nil, fmt.Errorf("injected OVN failure")
		}
	}
	return c.Client.Transact(ctx, ops...)
}

func newNetInfo(networkName, topology, role, subnets string) util.NetInfo {
	netconf := ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: networkName,
			Type: "ovn-k8s-cni-overlay",
		},
		Role:     role,
		Topology: topology,
		Subnets:  subnets,
	}
	netInfo, err := util.NewNetInfo(&netconf)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return netInfo
}

var _ = ginkgo.Describe("OVN podSelectorAddressSet", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
		netPolicyName1 = "networkpolicy1"
		nodeName       = "node1"
		podLabelKey    = "podLabel"
		ip1            = "10.128.1.1"
		ip2            = "10.128.1.2"
		ip3            = "10.128.1.3"
		ip4            = "10.128.1.4"
		hostNetPodIP   = "10.0.1.1"
		node1Subnet    = "10.244.0.0/24"
		node1SubnetV6  = "fd00:10:244::/64"
		node1MgmtIP    = "10.244.0.2"
		node1MgmtIPV6  = "fd00:10:244::2"
		ip1v6          = "2001:db8::1"
		gwIP1          = "100.64.0.1"
		gwIP1v6        = "fd98::1"
		gwIP2          = "100.64.0.2"
		gwIP2v6        = "fd98::2"
	)
	var (
		addressSetManager  *AddressSetManager
		wf                 *factory.WatchFactory
		clientSet          *util.OVNKubeControllerClientset
		initialDB          libovsdbtest.TestSetup
		libovsdbCleanup    *libovsdbtest.Context
		libovsdbNBClient   libovsdbclient.Client
		countingNBClient   *countingClient
		fakeNetworkManager *networkmanager.FakeNetworkManager
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}
		fakeNetworkManager = &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{},
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

	startAddrSetManagerWithNodes := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, pods []corev1.Pod, nodes []corev1.Node) {
		clientSet = util.GetOVNClientset(
			&corev1.NamespaceList{Items: namespaces},
			&corev1.PodList{Items: pods},
			&corev1.NodeList{Items: nodes},
		).GetOVNKubeControllerClientset()
		var err error
		wf, err = factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(wf.Start()).To(gomega.Succeed())
		libovsdbNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// this is only used for "saves db transactions when IPs don't change" test
		countingNBClient = &countingClient{Client: libovsdbNBClient}
		addressSetManager, err = NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), wf.NodeCoreInformer(), countingNBClient,
			fakeNetworkManager.Interface().GetNetworkNameForNADKey)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = addressSetManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	startAddrSetManager := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, pods []corev1.Pod) {
		startAddrSetManagerWithNodes(dbSetup, namespaces, pods, nil)
	}

	hostNetworkNodeSubnetsAnnotation := func(dualStack bool) string {
		if dualStack {
			return fmt.Sprintf(`{"default":["%s","%s"]}`, node1Subnet, node1SubnetV6)
		}
		return fmt.Sprintf(`{"default":"%s"}`, node1Subnet)
	}

	expectAddrSetData := func(dbIDs *libovsdbops.DbObjectIDs, ips []string, dualStack bool) []libovsdbtest.TestData {
		asV4, asV6 := addressset.GetTestDbAddrSets(dbIDs, ips)
		if dualStack {
			return []libovsdbtest.TestData{asV4, asV6}
		}
		return []libovsdbtest.TestData{asV4}
	}

	// forceReconcileAddressSet runs one reconcile under the per-key lock with an explicit
	// pending snapshot. Callers should Stop() controllers first so the workqueue cannot race.
	forceReconcileAddressSet := func(addrSetKey string, setupPending func(*podSelectorAddressSet)) error {
		return addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			if !found {
				return fmt.Errorf("address set %s not found", key)
			}
			psAddrSet.pendingUpdates.clear()
			setupPending(psAddrSet)
			return addressSetManager.reconcileAddressSetLocked(key, psAddrSet)
		})
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
		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("is not a valid label selector operator"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))

		// add nil pod selector
		_, _, _, err = addressSetManager.EnsureAddressSet(
			nil, peer.NamespaceSelector, nil, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("pod selector is nil"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))

		// namespace selector is nil and namespace is empty
		_, _, _, err = addressSetManager.EnsureAddressSet(
			peer.PodSelector, nil, nil, "", getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("namespace selector is nil and namespace is empty"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))
	})

	ginkgo.It("populates address set synchronously on first ensure", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}
		pod := testing.NewPod(namespace1.Name, "pod1", nodeName, podIP)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{podIP})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("reconciles the registered cluster node IP address set on node events", func() {
		node1 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node1",
				Annotations: map[string]string{util.OVNNodeHostCIDRs: `["10.0.0.1/24"]`},
			},
		}
		node2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node2",
				Annotations: map[string]string{util.OVNNodeHostCIDRs: `["10.0.0.2/24"]`},
			},
		}
		startAddrSetManagerWithNodes(initialDB, nil, nil, []corev1.Node{node1, node2})

		addressSetFactory := addressset.NewOvnAddressSetFactory(libovsdbNBClient, config.IPv4Mode, config.IPv6Mode)
		expectNodeIPs := func(expected ...string) {
			gomega.Eventually(func() []string {
				as, err := addressSetFactory.GetAddressSet(getClusterNodeIPsAddrSetDbIDs())
				if err != nil {
					return nil
				}
				v4Addresses, v6Addresses := as.GetAddresses()
				return append(v4Addresses, v6Addresses...)
			}).Should(gomega.ConsistOf(expected))
		}

		_, err := addressSetManager.EnsureClusterNodeIPsAddressSet(ClusterNodeIPsRouteAdvertisementsBackRef)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.1", "10.0.0.2")

		node2Copy := node2.DeepCopy()
		node2Copy.Annotations[util.OVNNodeHostCIDRs] = `["10.0.0.2/24","10.0.0.20/24"]`
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2Copy, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.1", "10.0.0.2", "10.0.0.20")

		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.2", "10.0.0.20")
	})

	ginkgo.It("creates one address set for multiple users with the same selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "label1",
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref1", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref2", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name, controllerName, false)
		peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
		// expect peer address set only
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
	})
	ginkgo.It("creates different address set for multiple users with the same selector depending on legacyNetpolMode", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "label1",
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref1", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref2", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref3", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name,
			controllerName, false)
		peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
		peerASLegacyIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name,
			controllerName, true)
		peerASLegacy, _ := addressset.GetTestDbAddrSets(peerASLegacyIDs, []string{})
		// expect 2 peer address sets only
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS, peerASLegacy}))
	})
	// this is run during ginkgo tree construction, so env is not set yet
	config.IPv4Mode = true
	config.IPv6Mode = false
	primaryNetInfo := newNetInfo("primarynet", types.Layer3Topology, types.NetworkRolePrimary, "10.128.1.0/24/27")
	secondaryNetInfo := newNetInfo("secondarynet", types.Layer2Topology, types.NetworkRoleSecondary, "10.128.1.0/24")

	newPod := func(namespace, name, node, podIP string, netInfo util.NetInfo) *corev1.Pod {
		nadKey := fmt.Sprintf("%s/%s", namespace, netInfo.GetNetworkName())
		fakeNetworkManager.NADNetworks[nadKey] = netInfo
		if netInfo.GetNetworkName() == types.DefaultNetworkName {
			return testing.NewPod(namespace, name, node, podIP)
		} else {
			return testing.NewPodWithSecondaryNADIP(namespace, name, node, "10.244.0.2", netInfo.GetNetworkName(), podIP)
		}
	}

	for _, netInfo := range []util.NetInfo{&util.DefaultNetInfo{}, primaryNetInfo, secondaryNetInfo} {
		netInfo := netInfo
		ginkgo.DescribeTable(fmt.Sprintf("[%s] adds selected pod ips to the address set and removes on delete", netInfo.GetNetworkName()),
			func(peer knet.NetworkPolicyPeer, staticNamespace string, addrSetIPsList []string, legacyMode bool, netInfo util.NetInfo) {
				addrSetIPs := sets.New(addrSetIPsList...)
				namespace1 := *testing.NewNamespace(namespaceName1)
				namespace2 := *testing.NewNamespace(namespaceName2)
				config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
				hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
				ns1pod1 := newPod(namespace1.Name, "ns1pod1", nodeName, ip1, netInfo)
				ns1pod2 := newPod(namespace1.Name, "ns1pod2", nodeName, ip2, netInfo)
				ns1pod3 := newPod(namespace1.Name, "ns1pod3", nodeName, hostNetPodIP, netInfo)
				ns1pod3.Spec.HostNetwork = true
				ns2pod1 := newPod(namespace2.Name, "ns2pod1", nodeName, ip3, netInfo)
				ns2pod2 := newPod(namespace2.Name, "ns2pod2", nodeName, ip4, netInfo)
				podsList := []corev1.Pod{}
				for _, pod := range []*corev1.Pod{ns1pod1, ns1pod2, ns1pod3, ns2pod1, ns2pod2} {
					pod.Labels = map[string]string{podLabelKey: pod.Name}
					podsList = append(podsList, *pod)
				}
				testNode := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
						Annotations: map[string]string{
							"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
						},
					},
				}
				startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{namespace1, namespace2, hostNetNamespace}, podsList,
					[]corev1.Node{testNode})

				_, _, _, err := addressSetManager.EnsureAddressSet(
					peer.PodSelector, peer.NamespaceSelector, nil, staticNamespace, "backRef", controllerName, netInfo, legacyMode)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// address set should be created and pod ips added
				peerASIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, staticNamespace, controllerName, legacyMode)
				// non-default networks don't match hostNetwork namespace remove node1MgmtIP if present
				if !netInfo.IsDefault() {
					addrSetIPs.Delete(node1MgmtIP)
					// secondary networks don't match hostNetwork pods, remove hostNetPodIP if present
					if !netInfo.IsPrimaryNetwork() {
						addrSetIPs.Delete(hostNetPodIP)
					}
				}
				peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, addrSetIPs.UnsortedList())
				gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
				// now delete ns1pod1 and ns1pod3 to test both overlay and hostnet pod IP cleanup
				err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), ns1pod1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), ns1pod3.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// deleted pod IPs should be removed from address set
				addrSetIPs.Delete(ip1, hostNetPodIP)
				peerAS, _ = addressset.GetTestDbAddrSets(peerASIDs, addrSetIPs.UnsortedList())
				gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
			},
			ginkgo.Entry("all pods from a static namespace", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1, ip2, hostNetPodIP}, false, netInfo),
			ginkgo.Entry("selected pods from a static namespace", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
				},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1}, false, netInfo),
			ginkgo.Entry("all pods from all namespaces", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip2, hostNetPodIP, ip3, ip4}, false, netInfo),
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
			}, namespaceName1, []string{ip1, ip3}, false, netInfo),
			ginkgo.Entry("all pods from selected namespaces", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3, ip4}, false, netInfo),
			ginkgo.Entry("selected pods from selected namespaces", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3}, false, netInfo),
			ginkgo.Entry("all pods from a static namespace, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1, ip2}, true, netInfo),
			ginkgo.Entry("selected pods from a static namespace, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
				},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1}, true, netInfo),
			ginkgo.Entry("all pods from all namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip2, ip3, ip4, node1MgmtIP}, true, netInfo),
			ginkgo.Entry("selected pods from all namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
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
			}, namespaceName1, []string{ip1, ip3}, true, netInfo),
			ginkgo.Entry("all pods from selected namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3, ip4}, true, netInfo),
			ginkgo.Entry("selected pods from selected namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3}, true, netInfo),
		)
	}
	ginkgo.It("on initial sync deletes unreferenced and updates referenced address sets", func() {
		unusedPodSelIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", controllerName, false)
		unusedPodSelAS, _ := addressset.GetTestDbAddrSets(unusedPodSelIDs, []string{"1.1.1.2"})
		refNetpolIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName2", controllerName, false)
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
		refPodSelIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName3", controllerName, false)
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
		// now request address sets for the referenced netpol and pod selector, addresses should be updated to empty
		// because no pods exist in that namespace
		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, "nsName2", "backref", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		updatedNetpolAS, _ := addressset.GetTestDbAddrSets(refNetpolIDs, []string{})
		updatedDB := []libovsdbtest.TestData{
			updatedNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(updatedDB))

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
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)

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
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)
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
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)

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
		eventuallyExpectEmptyAddressSetsExist(addressSetManager.nbClient, peer, namespace1.Name)
	})
	ginkgo.It("saves db transactions when IPs don't change", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		ns1pod1 := testing.NewPod(namespace1.Name, "ns1pod1", nodeName, ip1)

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*ns1pod1})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		// expect 2 transactions: 1 to create addr set and 1 to set IPs
		gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(int64(2)))

		addressSetManager.Stop()
		ns1pod1.Labels["newLabel"] = "newValue"
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), ns1pod1, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() string {
			pod, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(ns1pod1.Name)
			if getErr != nil {
				return ""
			}
			return pod.Labels["newLabel"]
		}).Should(gomega.Equal("newValue"))

		addrSetKey := getInternalKey(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
			psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, ns1pod1.Name))
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		// no new transactions should happen since IPs don't change
		gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(int64(2)))
	})

	ginkgo.DescribeTable("incrementally updates address set when a pod's IPs change",
		func(initialIPs, updatedIPs []string, ipv6Mode bool, initialTransacts, afterUpdateTransacts int64) {
			config.IPv4Mode = true
			config.IPv6Mode = ipv6Mode

			namespace1 := *testing.NewNamespace(namespaceName1)
			ns1pod1 := testing.NewPodWithLabelsAllIPFamilies(namespace1.Name, "ns1pod1", nodeName, initialIPs, nil)

			startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*ns1pod1})

			_, _, _, err := addressSetManager.EnsureAddressSet(
				&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
			expectedASv4, expectedASv6 := addressset.GetTestDbAddrSets(dbIDs, initialIPs)
			expectedData := []libovsdbtest.TestData{expectedASv4}
			if ipv6Mode {
				expectedData = append(expectedData, expectedASv6)
			}
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectedData))
			gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(initialTransacts))

			ns1pod1.Status.PodIP = updatedIPs[0]
			ns1pod1.Status.PodIPs = make([]corev1.PodIP, 0, len(updatedIPs))
			for _, ip := range updatedIPs {
				ns1pod1.Status.PodIPs = append(ns1pod1.Status.PodIPs, corev1.PodIP{IP: ip})
			}
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), ns1pod1, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedASv4, expectedASv6 = addressset.GetTestDbAddrSets(dbIDs, updatedIPs)
			expectedData = []libovsdbtest.TestData{expectedASv4}
			if ipv6Mode {
				expectedData = append(expectedData, expectedASv6)
			}
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectedData))
			// one additional ApplyAddressChanges transaction commits all family mutations together
			gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(afterUpdateTransacts))
		},
		ginkgo.Entry("IPv4",
			[]string{ip1}, []string{ip2}, false,
			int64(2), // create (1) + SetAddresses (1)
			int64(3),
		),
		ginkgo.Entry("dual-stack",
			[]string{ip1, "2001:db8::1"}, []string{ip2, "2001:db8::2"}, true,
			int64(3), // create both families (1) + SetAddresses per family (2)
			int64(4),
		),
	)

	ginkgo.It("removes pod IPs when a pod stops matching the selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		matchingPod := testing.NewPodWithLabels(namespace1.Name, "matching-pod", nodeName, ip1, map[string]string{podLabelKey: "match"})

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*matchingPod})

		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{podLabelKey: "match"},
		}
		_, _, _, err := addressSetManager.EnsureAddressSet(
			podSelector, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		matchingPod.Labels[podLabelKey] = "no-match"
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), matchingPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		emptyAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
	})

	ginkgo.It("updates address set when namespace enters and leaves namespace selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		namespace2 := *testing.NewNamespace(namespaceName2)
		namespace2.Labels = map[string]string{"name": "other"}
		podInNs2 := testing.NewPod(namespaceName2, "ns2pod1", nodeName, ip3)

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1, namespace2}, []corev1.Pod{*podInNs2})

		nsSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"name": namespaceName2},
		}
		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nsSelector, nil, namespaceName1, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nsSelector, nil, namespaceName1, controllerName, false)
		emptyAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		// namespace enters selection
		namespace2.Labels = map[string]string{"name": namespaceName2}
		_, err = clientSet.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip3})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// namespace leaves selection
		namespace2.Labels = map[string]string{"name": "other"}
		_, err = clientSet.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
	})

	ginkgo.It("keeps a shared pod IP until the last owning pod is deleted", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		sharedIP := ip1
		pod1 := testing.NewPod(namespace1.Name, "pod1", nodeName, sharedIP)
		pod2 := testing.NewPod(namespace1.Name, "pod2", nodeName, sharedIP)

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod1, *pod2})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{sharedIP})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		addressSetManager.Stop()
		err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() bool {
			_, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(pod1.Name)
			return apierrors.IsNotFound(getErr)
		}).Should(gomega.BeTrue())

		addrSetKey := getInternalKey(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
			psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, pod1.Name))
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			gomega.Expect(found).To(gomega.BeTrue())
			gomega.Expect(psAddrSet.addresses.Has(sharedIP)).To(gomega.BeTrue())
			_, stillCached := psAddrSet.podAddresses[fmt.Sprintf("%s/%s", namespace1.Name, pod2.Name)]
			gomega.Expect(stillCached).To(gomega.BeTrue())
			return nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), pod2.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() bool {
			_, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(pod2.Name)
			return apierrors.IsNotFound(getErr)
		}).Should(gomega.BeTrue())
		err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
			psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, pod2.Name))
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		emptyAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
	})

	ginkgo.It("full-syncs when pod and namespace pending updates are coalesced", func() {
		// Without full sync, coalesced pending would only run the namespace incremental path and
		// drop the pod IP change. Stop controllers, mutate API+pending, then reconcile once.
		namespace1 := *testing.NewNamespace(namespaceName1)
		namespace2 := *testing.NewNamespace(namespaceName2)
		namespace2.Labels = map[string]string{"name": "other"}
		podInNs1 := testing.NewPod(namespace1.Name, "ns1pod1", nodeName, ip1)
		podInNs2 := testing.NewPod(namespace2.Name, "ns2pod1", nodeName, ip3)

		startAddrSetManager(initialDB,
			[]corev1.Namespace{namespace1, namespace2},
			[]corev1.Pod{*podInNs1, *podInNs2})

		nsSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"name": namespaceName1},
		}
		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nsSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nsSelector, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Prevent background reconciles from splitting the coalesced pending update.
		addressSetManager.Stop()

		namespace2.Labels = map[string]string{"name": namespaceName1}
		_, err = clientSet.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podInNs1.Status.PodIP = ip2
		podInNs1.Status.PodIPs = []corev1.PodIP{{IP: ip2}}
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), podInNs1, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Eventually(func() string {
			ns, err := addressSetManager.namespaceLister.Get(namespace2.Name)
			if err != nil {
				return ""
			}
			return ns.Labels["name"]
		}).Should(gomega.Equal(namespaceName1))
		gomega.Eventually(func() string {
			pod, err := addressSetManager.podLister.Pods(namespace1.Name).Get(podInNs1.Name)
			if err != nil {
				return ""
			}
			return pod.Status.PodIP
		}).Should(gomega.Equal(ip2))

		addrSetKey := getInternalKey(&metav1.LabelSelector{}, nsSelector, nil, namespace1.Name, controllerName, false)
		err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			if !found {
				return fmt.Errorf("address set %s not found", key)
			}
			psAddrSet.pendingUpdates.clear()
			psAddrSet.pendingUpdates.addNamespaceKey(namespace2.Name)
			psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, podInNs1.Name))
			gomega.Expect(addressSetManager.pendingPodUpdateClassCount(psAddrSet.pendingUpdates.snapshot(), psAddrSet)).
				To(gomega.Equal(2))
			return addressSetManager.reconcileAddressSetLocked(key, psAddrSet)
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedAS, _ = addressset.GetTestDbAddrSets(dbIDs, []string{ip2, ip3})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("retries with full sync after OVN apply failure clears pending updates", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		ns1pod1 := testing.NewPod(namespace1.Name, "ns1pod1", nodeName, ip1)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*ns1pod1})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Fail the next OVN transaction (the incremental ApplyAddressChanges for the IP change).
		countingNBClient.failRemaining.Store(1)

		ns1pod1.Status.PodIP = ip2
		ns1pod1.Status.PodIPs = []corev1.PodIP{{IP: ip2}}
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), ns1pod1, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// First attempt clears pending and fails (marking fullSync); retry rebuilds fully.
		expectedAS, _ = addressset.GetTestDbAddrSets(dbIDs, []string{ip2})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("retains pending updates on retry after apply failure even if another pod event arrives first", func() {
		// pendingUpdates is cleared only on success. A failed pod-A update must keep A's key
		// so a later pod-B event coalesces with it instead of replacing it with a B-only path.
		namespace1 := *testing.NewNamespace(namespaceName1)
		podA := testing.NewPod(namespace1.Name, "pod-a", nodeName, ip1)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*podA})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		addressSetManager.Stop()

		podA.Status.PodIP = ip2
		podA.Status.PodIPs = []corev1.PodIP{{IP: ip2}}
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), podA, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() string {
			pod, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(podA.Name)
			if getErr != nil {
				return ""
			}
			return pod.Status.PodIP
		}).Should(gomega.Equal(ip2))

		addrSetKey := getInternalKey(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		podAKey := fmt.Sprintf("%s/%s", namespace1.Name, podA.Name)
		countingNBClient.failRemaining.Store(1)
		err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
			psAddrSet.pendingUpdates.addPodKey(podAKey)
		})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("injected OVN failure"))

		err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			if !found {
				return fmt.Errorf("address set %s not found", key)
			}
			gomega.Expect(psAddrSet.pendingUpdates.podKeys.Has(podAKey)).To(gomega.BeTrue(),
				"failed reconcile must retain the pending pod key so a later event cannot drop it")
			gomega.Expect(psAddrSet.pendingUpdates.fullSync).To(gomega.BeFalse())
			return nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		podB := testing.NewPod(namespace1.Name, "pod-b", nodeName, ip3)
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Create(context.TODO(), podB, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() error {
			_, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(podB.Name)
			return getErr
		}).Should(gomega.Succeed())

		// Simulate the pod-B event enqueueing B; A's failed pending key must still be present.
		err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			if !found {
				return fmt.Errorf("address set %s not found", key)
			}
			gomega.Expect(psAddrSet.pendingUpdates.podKeys.Has(podAKey)).To(gomega.BeTrue())
			psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, podB.Name))
			return addressSetManager.reconcileAddressSetLocked(key, psAddrSet)
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedAS, _ = addressset.GetTestDbAddrSets(dbIDs, []string{ip2, ip3})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("CleanupForController removes controller entries", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		pod1 := testing.NewPod(namespace1.Name, "pod1", nodeName, ip1)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod1})

		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}
		backRef := "NetworkPolicy/namespace1/testpolicy"

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, backRef, controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		gomega.Expect(addressSetManager.CleanupForController(controllerName)).To(gomega.Succeed())
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveEmptyData())
	})

	ginkgo.It("unregisters destroyed address sets from host-network selecting cache", func() {
		// Destroy paths must drop hostNetworkSelectingAddrSets entries; otherwise host IP
		// updates keep enqueueing keys that no longer exist in addressSets.
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		gwIP1 := "100.64.0.1"
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{hostNetNamespace}, nil, []corev1.Node{testNode})

		addrSetKey, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		gomega.Eventually(func() bool {
			addressSetManager.hostNetworkNamespaceLock.RLock()
			defer addressSetManager.hostNetworkNamespaceLock.RUnlock()
			return addressSetManager.hostNetworkSelectingAddrSets.Has(addrSetKey)
		}).Should(gomega.BeTrue())

		gomega.Expect(addressSetManager.DeleteAddressSet(addrSetKey, "backRef")).To(gomega.Succeed())
		gomega.Expect(func() bool {
			addressSetManager.hostNetworkNamespaceLock.RLock()
			defer addressSetManager.hostNetworkNamespaceLock.RUnlock()
			return addressSetManager.hostNetworkSelectingAddrSets.Has(addrSetKey)
		}()).To(gomega.BeFalse())
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveEmptyData())

		// Re-create and verify CleanupForController also unregisters.
		addrSetKey, _, _, err = addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		gomega.Eventually(func() bool {
			addressSetManager.hostNetworkNamespaceLock.RLock()
			defer addressSetManager.hostNetworkNamespaceLock.RUnlock()
			return addressSetManager.hostNetworkSelectingAddrSets.Has(addrSetKey)
		}).Should(gomega.BeTrue())

		gomega.Expect(addressSetManager.CleanupForController(controllerName)).To(gomega.Succeed())
		gomega.Expect(func() bool {
			addressSetManager.hostNetworkNamespaceLock.RLock()
			defer addressSetManager.hostNetworkNamespaceLock.RUnlock()
			return addressSetManager.hostNetworkSelectingAddrSets.Has(addrSetKey)
		}()).To(gomega.BeFalse())
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveEmptyData())
	})

	ginkgo.When("node selector is set", func() {
		const (
			nodeLabelKey         = "nodeType"
			nodeTypeWorker       = "worker"
			nodeTypeControlPlane = "control-plane"

			podLabelKey = "app"
			podAppVideo = "video"
		)
		node1 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node1",
				Labels: map[string]string{nodeLabelKey: nodeTypeWorker},
			},
		}
		node2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node2",
				Labels: map[string]string{nodeLabelKey: nodeTypeControlPlane},
			},
		}
		namespace1 := *testing.NewNamespace(namespaceName1)
		pod1 := testing.NewPod(namespace1.Name, "pod1", "node1", ip1)
		pod2 := testing.NewPod(namespace1.Name, "pod2", "node2", ip2)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{podLabelKey: podAppVideo},
		}
		nodeSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{nodeLabelKey: nodeTypeWorker},
		}
		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelector, namespace1.Name, controllerName, false)

		ginkgo.BeforeEach(func() {
			startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod1}, []corev1.Node{node1})
		})

		ginkgo.It("same pod and namespace selector with different nodeSelector create different address sets", func() {
			// add node2
			_, err := clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// add pod2
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Create(context.TODO(), pod2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			podSelector := &metav1.LabelSelector{}
			nodeSelWorker := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeTypeWorker}}
			nodeSelControlPlane := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeTypeControlPlane}}
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelWorker, namespace1.Name, "backRef1", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelControlPlane, namespace1.Name, "backRef2", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			dbIDsWorker := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelWorker, namespace1.Name, controllerName, false)
			dbIDsControlPlane := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelControlPlane, namespace1.Name, controllerName, false)
			// expect 2 address sets with IPs populated
			workerAS, _ := addressset.GetTestDbAddrSets(dbIDsWorker, []string{ip1})
			controlPlaneAS, _ := addressset.GetTestDbAddrSets(dbIDsControlPlane, []string{ip2})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{workerAS, controlPlaneAS}))
		})

		ginkgo.It("doesn't add pod IP to address set if its labels don't match pod label selector", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Consistently(addressSetManager.nbClient, 500*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})

		ginkgo.It("adds pod IP to address set if its labels match pod label selector", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		})

		ginkgo.It("adds pod IP to address set if its node labels match node label selector", func() {
			specialNodeSelector := &metav1.LabelSelector{
				MatchLabels: map[string]string{
					nodeLabelKey: "special-node",
				},
			}
			podSelector := &metav1.LabelSelector{}
			asID := GetPodSelectorAddrSetDbIDs(podSelector, nil, specialNodeSelector, namespace1.Name, controllerName, false)
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, specialNodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// expect the address set to be empty initially since nodeType "special-node" doesn't match "worker"
			emptyAS, _ := addressset.GetTestDbAddrSets(asID, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
			// label the node as "special-node"
			node1Copy := node1.DeepCopy()
			node1Copy.Labels = map[string]string{nodeLabelKey: "special-node"}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// expect the address set to be populated
			expectedAS, _ := addressset.GetTestDbAddrSets(asID, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		})

		ginkgo.It("deletes pod IP from address set when pod is deleted", func() {
			// label the pod to match the pod selector
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
			// delete the pod
			err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})

		ginkgo.It("deletes pod IP from address set when node label changes", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// add the pod to the address set
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
			// change the node label to not match the node selector
			node1Copy := node1.DeepCopy()
			node1Copy.Labels = map[string]string{nodeLabelKey: "control-plane"}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})

		ginkgo.It("enters and leaves node selector for all-namespaces sets via pod nodeName index", func() {
			// Empty namespace selector → matchedNamespaces.all; computeNodeAddressSetCachePlan
			// updates only pods on the pending node via listPodsOnNode / podsByNode.
			namespace2 := *testing.NewNamespace(namespaceName2)
			podOnNodeInNs2 := testing.NewPod(namespace2.Name, "ns2pod", node1.Name, ip3)
			_, err := clientSet.KubeClient.CoreV1().Namespaces().Create(context.TODO(), &namespace2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace2.Name).Create(context.TODO(), podOnNodeInNs2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			allPodsSelector := &metav1.LabelSelector{}
			allNamespacesSelector := &metav1.LabelSelector{}
			specialNodeSelector := &metav1.LabelSelector{
				MatchLabels: map[string]string{nodeLabelKey: "special-node"},
			}
			asID := GetPodSelectorAddrSetDbIDs(allPodsSelector, allNamespacesSelector, specialNodeSelector, "", controllerName, false)
			_, _, _, err = addressSetManager.EnsureAddressSet(allPodsSelector, allNamespacesSelector, specialNodeSelector,
				"", "backRef-all-ns", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			emptyAS, _ := addressset.GetTestDbAddrSets(asID, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

			node1Copy := node1.DeepCopy()
			node1Copy.Labels = map[string]string{nodeLabelKey: "special-node"}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// BeforeEach pod on node1 (ip1) plus the ns2 pod found via the nodeName index (ip3).
			expectedAS, _ := addressset.GetTestDbAddrSets(asID, []string{ip1, ip3})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

			node1Copy.Labels = map[string]string{nodeLabelKey: nodeTypeWorker}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})
	})

	ginkgo.It("reconciles HostNetworkNamespace IPs", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		// this is calculated based on node-id
		gwIP1 := "100.64.0.1"

		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// imitate node update, even though that should never happen
		gwIP2 := "100.64.0.2"
		testNode.Annotations["k8s.ovn.org/node-id"] = "2"
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		node2MgmtIP := "10.244.1.2"
		testNode2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node2",
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", "10.244.1.0/24"),
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode2, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2, node2MgmtIP})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// delete second node and check if IPs are removed from address set
		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode2.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Imitate restart replacing node1 with node2 to make sure HostNetworkNamespace IPs are fully updated on initialSync
		ginkgo.By("restarting addressSetManager with different node in the cluster")
		addressSetManager.Stop()
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode2, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		addressSetManager, err = NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), wf.NodeCoreInformer(), libovsdbNBClient,
			func(_ string) string { return "" })
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = addressSetManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// run EnsureAddressSet again, otherwise address set won't be reconciled with no users
		_, _, _, err = addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		restartedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node2MgmtIP})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{restartedAS}))
	})

	ginkgo.DescribeTable("keeps shared IPs when only one of pod/host ownership is removed",
		func(dualStack bool) {
			// Legacy netpol address sets union pod IPs with HostNetworkNamespace IPs. An IP present
			// in both mirrors must remain in OVN until neither owns it.
			config.IPv4Mode = true
			config.IPv6Mode = dualStack
			config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
			hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
			namespace1 := *testing.NewNamespace(namespaceName1)

			hostAndGW := []string{node1MgmtIP, gwIP1}
			sharedMgmt := []string{node1MgmtIP}
			if dualStack {
				hostAndGW = append(hostAndGW, node1MgmtIPV6, gwIP1v6)
				sharedMgmt = append(sharedMgmt, node1MgmtIPV6)
			}

			testNode := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": hostNetworkNodeSubnetsAnnotation(dualStack),
						"k8s.ovn.org/node-id":      "1",
					},
				},
			}
			// Pod IPs intentionally overlap with the host-network management IPs.
			overlappingPod := testing.NewPodWithLabelsAllIPFamilies(namespace1.Name, "overlap-pod", nodeName, sharedMgmt, nil)
			startAddrSetManagerWithNodes(initialDB,
				[]corev1.Namespace{hostNetNamespace, namespace1},
				[]corev1.Pod{*overlappingPod},
				[]corev1.Node{testNode})

			_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", controllerName, true)
			expectedData := expectAddrSetData(hostNSASIDs, hostAndGW, dualStack)
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectedData))

			// Pod leaves while host still owns the shared management IP(s) — OVN must keep them.
			addressSetManager.Stop()
			err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), overlappingPod.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(func() bool {
				_, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(overlappingPod.Name)
				return apierrors.IsNotFound(getErr)
			}).Should(gomega.BeTrue())

			addrSetKey := getInternalKey(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil, "", controllerName, true)
			err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
				psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, overlappingPod.Name))
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectedData))
			err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
				psAddrSet, found := addressSetManager.addressSets.Load(key)
				gomega.Expect(found).To(gomega.BeTrue())
				for _, ip := range sharedMgmt {
					gomega.Expect(psAddrSet.addresses.Has(ip)).To(gomega.BeTrue())
				}
				return nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Re-add a pod with the shared IP(s), then drop host ownership (delete node).
			remainingPod := testing.NewPodWithLabelsAllIPFamilies(namespace1.Name, "remaining-pod", nodeName, sharedMgmt, nil)
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Create(context.TODO(), remainingPod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(func() error {
				_, getErr := addressSetManager.podLister.Pods(namespace1.Name).Get(remainingPod.Name)
				return getErr
			}).Should(gomega.Succeed())
			err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
				psAddrSet.pendingUpdates.addPodKey(fmt.Sprintf("%s/%s", namespace1.Name, remainingPod.Name))
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectedData))

			err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(func() bool {
				_, getErr := addressSetManager.nodeLister.Get(testNode.Name)
				return apierrors.IsNotFound(getErr)
			}).Should(gomega.BeTrue())
			// Controllers are stopped; apply the host-network IP cache update that reconcileNode
			// would have performed, then force the selecting address-set reconcile.
			gomega.Expect(addressSetManager.updateHostNetworkIPs(testNode.Name)).To(gomega.Succeed())
			err = forceReconcileAddressSet(addrSetKey, func(psAddrSet *podSelectorAddressSet) {
				psAddrSet.pendingUpdates.addNodeKey(testNode.Name)
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectAddrSetData(hostNSASIDs, sharedMgmt, dualStack)))
		},
		ginkgo.Entry("IPv4", false),
		ginkgo.Entry("dual-stack", true),
	)

	ginkgo.DescribeTable("updates host-network annotation IPs without disturbing unrelated pod IPs",
		func(dualStack bool) {
			// Host-only incremental path: node annotation change must rewrite host IPs while a
			// non-overlapping selected pod IP stays put (deriveUnionAddressDelta).
			config.IPv4Mode = true
			config.IPv6Mode = dualStack
			config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
			hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
			namespace1 := *testing.NewNamespace(namespaceName1)

			podIPs := []string{ip1}
			initialIPs := []string{ip1, node1MgmtIP, gwIP1}
			updatedIPs := []string{ip1, node1MgmtIP, gwIP2}
			if dualStack {
				podIPs = append(podIPs, ip1v6)
				initialIPs = append(initialIPs, ip1v6, node1MgmtIPV6, gwIP1v6)
				updatedIPs = append(updatedIPs, ip1v6, node1MgmtIPV6, gwIP2v6)
			}

			testNode := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": hostNetworkNodeSubnetsAnnotation(dualStack),
						"k8s.ovn.org/node-id":      "1",
					},
				},
			}
			regularPod := testing.NewPodWithLabelsAllIPFamilies(namespace1.Name, "regular-pod", nodeName, podIPs, nil)
			startAddrSetManagerWithNodes(initialDB,
				[]corev1.Namespace{hostNetNamespace, namespace1},
				[]corev1.Pod{*regularPod},
				[]corev1.Node{testNode})

			_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", controllerName, true)
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectAddrSetData(hostNSASIDs, initialIPs, dualStack)))

			testNode.Annotations["k8s.ovn.org/node-id"] = "2"
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectAddrSetData(hostNSASIDs, updatedIPs, dualStack)))
		},
		ginkgo.Entry("IPv4", false),
		ginkgo.Entry("dual-stack", true),
	)

	ginkgo.DescribeTable("drains host-network IPs when the host network namespace is deleted",
		func(dualStack bool) {
			config.IPv4Mode = true
			config.IPv6Mode = dualStack
			config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
			hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)

			hostAndGW := []string{node1MgmtIP, gwIP1}
			if dualStack {
				hostAndGW = append(hostAndGW, node1MgmtIPV6, gwIP1v6)
			}

			testNode := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": hostNetworkNodeSubnetsAnnotation(dualStack),
						"k8s.ovn.org/node-id":      "1",
					},
				},
			}
			startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{hostNetNamespace}, nil, []corev1.Node{testNode})

			_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
				"", controllerName, true)
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectAddrSetData(hostNSASIDs, hostAndGW, dualStack)))

			err = clientSet.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), hostNetNamespace.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(expectAddrSetData(hostNSASIDs, []string{}, dualStack)))
		},
		ginkgo.Entry("IPv4", false),
		ginkgo.Entry("dual-stack", true),
	)

	ginkgo.It("enqueues selecting address sets for host-only reconcile when the host network namespace is deleted", func() {
		// Enqueue from updateHostNetworkNamespaceExists itself (not only reconcileNamespace),
		// using host-only pending so host IPs drain without a full pod rebuild.
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		namespace1 := *testing.NewNamespace(namespaceName1)
		gwIP1 := "100.64.0.1"
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		regularPod := testing.NewPod(namespace1.Name, "regular-pod", nodeName, ip1)
		startAddrSetManagerWithNodes(initialDB,
			[]corev1.Namespace{hostNetNamespace, namespace1},
			[]corev1.Pod{*regularPod},
			[]corev1.Node{testNode})

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{ip1, node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		addressSetManager.Stop()
		err = clientSet.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), hostNetNamespace.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() bool {
			_, getErr := addressSetManager.namespaceLister.Get(hostNetNamespace.Name)
			return apierrors.IsNotFound(getErr)
		}).Should(gomega.BeTrue())

		gomega.Expect(addressSetManager.updateHostNetworkNamespaceExists()).To(gomega.Succeed())
		addrSetKey := getInternalKey(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil, "", controllerName, true)
		err = addressSetManager.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, found := addressSetManager.addressSets.Load(key)
			if !found {
				return fmt.Errorf("address set %s not found", key)
			}
			gomega.Expect(psAddrSet.pendingUpdates.fullSync).To(gomega.BeFalse())
			gomega.Expect(psAddrSet.pendingUpdates.nodeKeys.Has("")).To(gomega.BeTrue())
			return addressSetManager.reconcileAddressSetLocked(key, psAddrSet)
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{ip1})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("updates IPs for existing nodes when the host network traffic namespace is created", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		// this is calculated based on node-id
		gwIP1 := "100.64.0.1"
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		startAddrSetManagerWithNodes(initialDB, nil, nil, []corev1.Node{testNode})
		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		hostNetNamespace := testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		_, err = clientSet.KubeClient.CoreV1().Namespaces().Create(context.TODO(), hostNetNamespace, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

	})
	ginkgo.It("includes node primary IP in HostNetworkNamespace address_set when NoOverlay mode is enabled", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		// Enable NoOverlay transport mode
		config.Default.Transport = "no-overlay"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		// Start with empty NodeList, then add node after namespace is created
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		// Wait for empty address set to be created
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		// Create the node with only 1 annotation, it shouldn't fail and will parse what we have
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					util.OvnNodeIfAddr: "{\"ipv4\":\"192.168.1.10/24\"}",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Build expected IPs following the same pattern as the remote zone test
		// When NoOverlay is enabled, primary interface IPv4 should also be included
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{"192.168.1.10"})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Now update primary IP. Even though it should never happen, make sure we handle it correctly
		testNode.Annotations[util.OvnNodeIfAddr] = "{\"ipv4\":\"192.168.1.11/24\"}"
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{"192.168.1.11"})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})
	ginkgo.It("correctly updates HostNetworkNamespace for hybrid overlay nodes", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		// set hybrid overlay selector
		config.HybridOverlay.Enabled = true
		config.Kubernetes.NoHostSubnetNodes, _ = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}})
		// Start with empty NodeList, then add node after namespace is created
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		// Wait for empty address set to be created
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		// this is calculated based on node-id
		gwIP := "100.64.0.1"
		// Create node that doesn't match hybrid overlay selector, it should be added to HostNetworkNamespace address set
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Now label node as hybrid overlay, should be removed
		testNode.Labels = map[string]string{"a": "b"}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		emptyAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
	})

	ginkgo.It("allows a second NewAddressSetManager when the pod nodeName indexer already exists", func() {
		startAddrSetManager(initialDB, nil, nil)
		second, err := NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), wf.NodeCoreInformer(), libovsdbNBClient,
			fakeNetworkManager.Interface().GetNetworkNameForNADKey)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(second).NotTo(gomega.BeNil())
		second.Stop()
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

type stubPodNodeNameIndexer struct {
	indexer        cache.Indexer
	addIndexersErr error
	// installOnError installs the indexers before returning addIndexersErr, simulating a
	// concurrent constructor that won the race.
	installOnError bool
	addCalls       int
}

func (s *stubPodNodeNameIndexer) GetIndexer() cache.Indexer { return s.indexer }

func (s *stubPodNodeNameIndexer) AddIndexers(indexers cache.Indexers) error {
	s.addCalls++
	if s.addIndexersErr != nil {
		if s.installOnError {
			_ = s.indexer.AddIndexers(indexers)
		}
		return s.addIndexersErr
	}
	return s.indexer.AddIndexers(indexers)
}

var _ = ginkgo.Describe("ensurePodNodeNameIndexer", func() {
	ginkgo.It("is a no-op when the indexer already exists", func() {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
			podNodeNameIndex: podByNodeNameIndexFunc,
		})
		stub := &stubPodNodeNameIndexer{indexer: indexer}
		gomega.Expect(ensurePodNodeNameIndexer(stub)).To(gomega.Succeed())
		gomega.Expect(stub.addCalls).To(gomega.Equal(0))
	})

	ginkgo.It("returns an error when AddIndexers fails and the index is still missing", func() {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		stub := &stubPodNodeNameIndexer{
			indexer:        indexer,
			addIndexersErr: fmt.Errorf("boom"),
		}
		err := ensurePodNodeNameIndexer(stub)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(podNodeNameIndex))
		gomega.Expect(stub.addCalls).To(gomega.Equal(1))
	})

	ginkgo.It("tolerates AddIndexers error when a concurrent constructor installed the index", func() {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		stub := &stubPodNodeNameIndexer{
			indexer:        indexer,
			addIndexersErr: fmt.Errorf("indexer conflict"),
			installOnError: true,
		}
		gomega.Expect(ensurePodNodeNameIndexer(stub)).To(gomega.Succeed())
		_, exists := stub.indexer.GetIndexers()[podNodeNameIndex]
		gomega.Expect(exists).To(gomega.BeTrue())
	})
})
