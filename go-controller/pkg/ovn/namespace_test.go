// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	dnsnameresolver "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func getNamespaceAnnotations(fakeClient kubernetes.Interface, name string) map[string]string {
	ns, err := fakeClient.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return ns.Annotations
}

func newUDNNamespaceWithLabels(namespace string, additionalLabels map[string]string) *corev1.Namespace {
	n := &corev1.Namespace{
		ObjectMeta: ovntest.NewNamespaceMeta(namespace, additionalLabels),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
	n.Labels[ovntypes.RequiredUDNNamespaceLabel] = ""
	return n
}

func newUDNNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: ovntest.NewNamespaceMeta(namespace, map[string]string{ovntypes.RequiredUDNNamespaceLabel: ""}),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}

func getNsAddrSetHashNames(netControllerName, ns string) (string, string) {
	return addressset.GetHashNamesForAS(getNamespaceAddrSetDbIDs(ns, netControllerName))
}

func buildNamespaceAddressSets(namespace string, ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	return addressset.GetTestDbAddrSets(getNamespaceAddrSetDbIDs(namespace, "default-network-controller"), ips)
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	const (
		namespaceName         = "namespace1"
		clusterIPNet   string = "10.1.0.0"
		clusterCIDR    string = clusterIPNet + "/16"
		controllerName        = ovntypes.DefaultNetworkControllerName
	)
	var (
		fakeOvn *FakeOVN
		wg      *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fakeOvn = NewFakeOVN(true)
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		wg.Wait()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("only cleans up address sets owned by namespace", func() {
			namespace1 := ovntest.NewNamespace(namespaceName)
			// namespace-owned address set for existing namespace, should stay
			ns1 := getNamespaceAddrSetDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			_, err := fakeOvn.asf.NewAddressSet(ns1, []string{"1.1.1.1"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// namespace-owned address set for stale namespace, should be deleted
			ns2 := getNamespaceAddrSetDbIDs("namespace2", ovntypes.DefaultNetworkControllerName)
			_, err = fakeOvn.asf.NewAddressSet(ns2, []string{"1.1.1.2"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// netpol peer address set for existing netpol, should stay
			netpol := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", ovntypes.DefaultNetworkControllerName, false)
			_, err = fakeOvn.asf.NewAddressSet(netpol, []string{"1.1.1.3"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// egressQoS-owned address set, should stay
			qos := getEgressQosAddrSetDbIDs("namespace", "0", controllerName)
			_, err = fakeOvn.asf.NewAddressSet(qos, []string{"1.1.1.4"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// hybridNode-owned address set, should stay
			hybridNode := apbroute.GetHybridRouteAddrSetDbIDs("node", ovntypes.DefaultNetworkControllerName)
			_, err = fakeOvn.asf.NewAddressSet(hybridNode, []string{"1.1.1.5"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// egress firewall-owned address set, should stay
			ef := dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs("dnsname", controllerName)
			_, err = fakeOvn.asf.NewAddressSet(ef, []string{"1.1.1.6"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: []libovsdb.TestData{}})
			err = fakeOvn.controller.syncNamespaces([]interface{}{namespace1})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.ExpectAddressSetWithAddresses(ns1, []string{"1.1.1.1"})
			fakeOvn.asf.EventuallyExpectNoAddressSet(ns2)
			fakeOvn.asf.ExpectAddressSetWithAddresses(netpol, []string{"1.1.1.3"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(qos, []string{"1.1.1.4"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(hybridNode, []string{"1.1.1.5"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(ef, []string{"1.1.1.6"})
		})

		ginkgo.It("reconciles an existing namespace with pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			namespaceT := *ovntest.NewNamespace(namespaceName)
			tP := newTPod(
				"node1",
				"10.128.1.0/24",
				"10.128.1.2",
				"10.128.1.1",
				"myPod",
				"10.128.1.3",
				"11:22:33:44:55:66",
				namespaceT.Name,
			)

			tPod := ovntest.NewPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP)
			fakeOvn.start(
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
				&corev1.PodList{
					Items: []corev1.Pod{
						*tPod,
					},
				},
			)
			podMAC := ovntest.MustParseMAC(tP.podMAC)
			podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
			fakeOvn.controller.logicalPortCache.add(tPod, tP.nodeName, ovntypes.DefaultNetworkName, fakeUUID, podMAC, podIPNets)
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespaceName, []string{tP.podIP})

			// port group is empty, because it will be filled by pod add logic
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("creates an empty address set and port group for the namespace without pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			fakeOvn.start(&corev1.NamespaceList{
				Items: []corev1.Namespace{
					*ovntest.NewNamespace(namespaceName),
				},
			})
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)

			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("reconciles an existing namespace port group, without updating it", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			namespaceT := *ovntest.NewNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespaceName, []string{})
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialData))
		})
		ginkgo.It("deletes an existing namespace port group when egress firewall and multicast are disabled", func() {
			namespaceT := *ovntest.NewNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
		ginkgo.It("deletes an existing namespace port group when there are no namespaces", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("deletes an empty namespace's resources", func() {
			fakeOvn.start(&corev1.NamespaceList{
				Items: []corev1.Namespace{
					*ovntest.NewNamespace(namespaceName),
				},
			})
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)

			err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, *metav1.NewDeleteOptions(1))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// namespace's address set deletion is delayed by 20 second to let other handlers cleanup
			gomega.Eventually(func() bool {
				return fakeOvn.asf.AddressSetExists(namespaceName)
			}, 21*time.Second).Should(gomega.BeFalse())
		})
	})
})
