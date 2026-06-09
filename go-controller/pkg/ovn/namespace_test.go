// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"net"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
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

func getStaleNamespaceAddrSetDbIDs(namespaceName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controller, map[libovsdbops.ExternalIDKey]string{
		// namespace has only 1 address set, no additional ids are required
		libovsdbops.ObjectNameKey: namespaceName,
	})
}

func buildStaleNamespaceAddressSets(namespace string, ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	return addressset.GetTestDbAddrSets(getStaleNamespaceAddrSetDbIDs(namespace, "default-network-controller"), ips)
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	const (
		namespaceName  = "namespace1"
		controllerName = ovntypes.DefaultNetworkControllerName
	)
	var (
		fakeOvn *FakeOVN
		wg      *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fakeOvn = NewFakeOVN(false)
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		wg.Wait()
	})

	ginkgo.Context("deleting a namespace that was never added (bootstrap race)", func() {
		ginkgo.It("cleans up stale namespace port group and multicast ACLs", func() {
			fakeOvn.startWithDBSetup(libovsdb.TestSetup{})
			// multicastSupport makes needNamespacedPortGroup() true AND
			// enables the no-nsInfo multicast cleanup branch. Set it on the
			// already-constructed controller (the construction-time flag is
			// false under PrepareTestConfig).
			fakeOvn.controller.multicastSupport = true

			// Simulate stale state from a previous run: a namespace port
			// group and its multicast ACLs exist, but the namespace was
			// never added on this start (no nsInfo). This is the bootstrap
			// race — the namespace was listed by bootstrapNetwork (so
			// SyncNamespaces/syncNsMulticast kept its port group and
			// multicast ACLs as "expected") and then deleted before its
			// scoped add reconcile ran, so the add leg never created nsInfo.
			pgName, err := fakeOvn.controller.createNamespacePortGroup(namespaceName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// createMulticastAllowPolicy programs the namespace multicast
			// ACLs (+ address set); the throwaway nsInfo stands in for the
			// previous run's.
			gomega.Expect(fakeOvn.controller.createMulticastAllowPolicy(namespaceName, &namespaceInfo{})).To(gomega.Succeed())

			mcastACLPredicate := libovsdbops.GetPredicate[*nbdb.ACL](
				libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, controllerName,
					map[libovsdbops.ExternalIDKey]string{libovsdbops.ObjectNameKey: namespaceName}), nil)
			countMcastACLs := func() int {
				acls, e := libovsdbops.FindACLsWithPredicate(fakeOvn.controller.nbClient, mcastACLPredicate)
				gomega.Expect(e).NotTo(gomega.HaveOccurred())
				return len(acls)
			}
			gomega.Eventually(func() error {
				_, e := libovsdbops.GetPortGroup(fakeOvn.controller.nbClient, &nbdb.PortGroup{Name: pgName})
				return e
			}).Should(gomega.Succeed(), "precondition: stale port group must exist")
			gomega.Expect(countMcastACLs()).To(gomega.BeNumerically(">", 0), "precondition: stale multicast ACLs must exist")

			// The delete leg must clean the deterministically-named port
			// group AND the multicast ACLs even though no nsInfo was ever
			// created.
			nsInfo, err := fakeOvn.controller.deleteNamespaceLocked(namespaceName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nsInfo).To(gomega.BeNil())

			gomega.Eventually(func() error {
				_, e := libovsdbops.GetPortGroup(fakeOvn.controller.nbClient, &nbdb.PortGroup{Name: pgName})
				return e
			}).ShouldNot(gomega.Succeed(), "stale port group must be deleted")
			gomega.Expect(countMcastACLs()).To(gomega.Equal(0), "stale multicast ACLs must be deleted")
		})
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("only cleans up address sets owned by namespace", func() {
			// namespace address sets are now deprecated and should all be removed on startup
			namespace1 := ovntest.NewNamespace(namespaceName)
			// namespace-owned address set for existing namespace, should be deleted
			ns1, _ := buildStaleNamespaceAddressSets(namespaceName, []string{"1.1.1.1"})
			// namespace-owned address set for stale namespace, should be deleted
			ns2, _ := buildStaleNamespaceAddressSets("namespace2", []string{"1.1.1.2"})
			// netpol peer address set will be removed by the addresssetManager as unreferenced
			netpol := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", ovntypes.DefaultNetworkControllerName, false)
			netpolAS, _ := addressset.GetTestDbAddrSets(netpol, []string{"1.1.1.3"})
			// egressQoS-owned address set, should stay
			qos := getEgressQosAddrSetDbIDs("namespace", "0", controllerName)
			qosAS, _ := addressset.GetTestDbAddrSets(qos, []string{"1.1.1.4"})
			// hybridNode-owned address set, should stay
			hybridNode := apbroute.GetHybridRouteAddrSetDbIDs("node", ovntypes.DefaultNetworkControllerName)
			hybridNodeAS, _ := addressset.GetTestDbAddrSets(hybridNode, []string{"1.1.1.5"})
			// egress firewall-owned address set, should stay
			ef := dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs("dnsname", controllerName)
			efAS, _ := addressset.GetTestDbAddrSets(ef, []string{"1.1.1.6"})

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: []libovsdb.TestData{ns1, ns2, netpolAS, qosAS, hybridNodeAS, efAS}})
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{ns1, ns2, qosAS, hybridNodeAS, efAS}))

			// now namespace address sets will be cleaned up
			err := fakeOvn.controller.syncNamespaces([]interface{}{namespace1})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{qosAS, hybridNodeAS, efAS}))
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
			err := fakeOvn.controller.RegisterNamespaceHandler()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
			err := fakeOvn.controller.RegisterNamespaceHandler()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

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

			err := fakeOvn.controller.RegisterNamespaceHandler()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

			err := fakeOvn.controller.RegisterNamespaceHandler()
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

			err := fakeOvn.controller.RegisterNamespaceHandler()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
	})
})
