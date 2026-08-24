// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zone_tracker

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	informerfactory "k8s.io/client-go/informers"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const noHostSubnetLabel = "zone-tracker-test/no-host-subnet"

func getNode(nodeName string, noHostSubnet bool) *corev1.Node {
	nodeLabels := map[string]string{}
	if noHostSubnet {
		nodeLabels[noHostSubnetLabel] = "true"
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: nodeLabels,
		},
	}
}

var _ = Describe("Cluster Manager Zone Tracker", func() {
	var (
		zoneTracker      *ZoneTracker
		subscribeCounter atomic.Uint64
		fakeClient       *util.OVNClusterManagerClientset
		coreFactory      informerfactory.SharedInformerFactory
		factoryStopChan  chan struct{}
		originalSelector labels.Selector
	)

	const (
		node1 = "node1"
		node2 = "node2"
	)

	checkZones := func(expectedZones ...string) {
		Eventually(func() bool {
			zoneTracker.zonesLock.RLock()
			defer zoneTracker.zonesLock.RUnlock()
			return zoneTracker.zones.Equal(sets.New(expectedZones...))
		}).Should(BeTrue(), fmt.Sprintf("expected zones %v", expectedZones))
	}

	checkSubscribeCounter := func(expected int) {
		Eventually(subscribeCounter.Load).Should(BeEquivalentTo(expected), fmt.Sprintf("expected %v subscriber calls", expected))
	}

	createNode := func(node *corev1.Node) {
		_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	setNoHostSubnet := func(nodeName string, noHostSubnet bool) {
		node, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if noHostSubnet {
			node.Labels[noHostSubnetLabel] = "true"
		} else {
			delete(node.Labels, noHostSubnetLabel)
		}
		_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteNode := func(nodeName string) {
		err := fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		originalSelector = config.Kubernetes.NoHostSubnetNodes
		config.Kubernetes.NoHostSubnetNodes = labels.SelectorFromSet(labels.Set{noHostSubnetLabel: "true"})

		fakeClient = util.GetOVNClientset().GetClusterManagerClientset()
		coreFactory = informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)
		zoneTracker = NewZoneTracker(coreFactory.Core().V1().Nodes(), func(sets.Set[string]) {
			subscribeCounter.Add(1)
		})

		subscribeCounter.Store(0)
		factoryStopChan = make(chan struct{})
		coreFactory.Start(factoryStopChan)
		err := zoneTracker.Start()
		Expect(err).ToNot(HaveOccurred())

		checkSubscribeCounter(1)
	})

	AfterEach(func() {
		close(factoryStopChan)
		zoneTracker.Stop()
		// Wait for informer callbacks that may still read NoHostSubnetNodes.
		coreFactory.Shutdown()
		config.Kubernetes.NoHostSubnetNodes = originalSelector
	})

	It("tracks each OVN-managed node as its own zone", func() {
		createNode(getNode(node1, false))
		checkZones(node1)
		checkSubscribeCounter(2)

		createNode(getNode(node2, false))
		checkZones(node1, node2)
		checkSubscribeCounter(3)

		deleteNode(node1)
		checkZones(node2)
		checkSubscribeCounter(4)

		deleteNode(node2)
		checkZones()
		checkSubscribeCounter(5)
	})

	It("excludes no-host-subnet nodes and handles eligibility changes", func() {
		createNode(getNode(node1, true))
		checkZones()
		checkSubscribeCounter(1)

		setNoHostSubnet(node1, false)
		checkZones(node1)
		checkSubscribeCounter(2)

		setNoHostSubnet(node1, true)
		checkZones()
		checkSubscribeCounter(3)
	})

	It("ignores updates that do not change zone eligibility", func() {
		createNode(getNode(node1, false))
		checkZones(node1)
		checkSubscribeCounter(2)

		node, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		node.Labels["unrelated"] = "value"
		_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		checkZones(node1)
		checkSubscribeCounter(2)
	})

	It("does not notify when reconciling a missing node", func() {
		err := zoneTracker.reconcileNode(node1)
		Expect(err).ToNot(HaveOccurred())
		checkZones()
		checkSubscribeCounter(1)
	})
})
