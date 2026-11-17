// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type podRetryNoWriteClient struct {
	libovsdbclient.Client
}

func (c *podRetryNoWriteClient) Transact(_ context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return nil, fmt.Errorf("pod enqueue retry must not write OVN node resources")
}

var _ = ginkgo.Describe("Node pod enqueue retries", func() {
	var fakeOvn *FakeOVN
	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.IPv4Mode = true
		config.Gateway.DisableSNATMultipleGWs = true
		fakeOvn = NewFakeOVN(false, "node1")
	})
	ginkgo.AfterEach(func() { fakeOvn.shutdown() })

	ginkgo.DescribeTable("retries pod enqueueing without repeating successful node setup", func(bootstrap bool) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				types.NodeSubnetsAnnotation: `{"default":["10.1.0.0/24"]}`,
			},
		}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
			Spec:       corev1.PodSpec{NodeName: node.Name},
		}
		fakeOvn.start(node, pod)
		oc := fakeOvn.controller
		_, err := oc.addNode(node)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(oc.hasLocalNodeSwitchState(node)).To(gomega.BeTrue())

		// Start at the retry boundary: node setup succeeded, but enqueueing pods
		// remains pending. Any subsequent OVN transaction fails this test.
		oc.syncPodsFailed.Store(node.Name, true)
		oc.nbClient = &podRetryNoWriteClient{Client: fakeOvn.nbClient}
		oc.sbClient = &podRetryNoWriteClient{Client: fakeOvn.sbClient}
		key, err := retry.GetResourceKey(pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("retaining pending pod work while the gateway is not ready")
		oc.gatewaysFailed.Store(node.Name, true)
		gomega.Expect(oc.addUpdateLocalNodeEvent(context.Background(), node, &nodeSyncs{syncPods: true})).To(gomega.Succeed())
		_, pending := oc.syncPodsFailed.Load(node.Name)
		gomega.Expect(pending).To(gomega.BeTrue())
		gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeFalse())
		oc.gatewaysFailed.Delete(node.Name)

		ginkgo.By("enqueueing pods and clearing only the pod retry flag")
		oldNode := node
		if bootstrap {
			oldNode = nil
		}
		gomega.Expect(oc.ReconcileNode(context.Background(), oldNode, node, nil, nil)).To(gomega.Succeed())
		gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeTrue())
		_, pending = oc.syncPodsFailed.Load(node.Name)
		gomega.Expect(pending).To(gomega.BeFalse())
		_, failed := oc.addNodeFailed.Load(node.Name)
		gomega.Expect(failed).To(gomega.BeFalse())

		ginkgo.By("not enqueueing pods again after the retry succeeds")
		oc.retryPods.DoWithLock(key, oc.retryPods.DeleteRetryObj)
		gomega.Expect(oc.ReconcileNode(context.Background(), node, node, nil, nil)).To(gomega.Succeed())
		gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeFalse())

		ginkgo.By("clearing pending pod work when the node is deleted")
		oc.nbClient = fakeOvn.nbClient
		oc.sbClient = fakeOvn.sbClient
		oc.syncPodsFailed.Store(node.Name, true)
		gomega.Expect(oc.ReconcileNode(context.Background(), node, nil, nil, nil)).To(gomega.Succeed())
		_, pending = oc.syncPodsFailed.Load(node.Name)
		gomega.Expect(pending).To(gomega.BeFalse())
	},
		ginkgo.Entry("bootstrap retry", true),
		ginkgo.Entry("identical-node update retry", false),
	)

	ginkgo.DescribeTable("retries L3 UDN pod enqueueing without repeating successful node setup", func(role string, bootstrap bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
		nad := ovntest.GenerateNAD("blue", "blue", "default", types.Layer3Topology, "10.2.0.0/16", role)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		netInfo, err := util.ParseNADInfo(nad)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "node1", Annotations: map[string]string{
				types.NodeSubnetsAnnotation: `{"blue":["10.2.0.0/24"]}`,
			},
		}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
			Spec:       corev1.PodSpec{NodeName: node.Name},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouter{UUID: "blue-router-UUID", Name: netInfo.GetNetworkScopedName(types.OVNClusterRouter)},
		}}, node, pod)
		gomega.Expect(fakeOvn.NewUserDefinedNetworkController(nad)).To(gomega.Succeed())
		oc := fakeOvn.fullL3UDNControllers["blue"]
		_, err = oc.addNode(node, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		oc.syncPodsFailed.Store(node.Name, true)
		oc.nbClient = &podRetryNoWriteClient{Client: fakeOvn.nbClient}
		oc.sbClient = &podRetryNoWriteClient{Client: fakeOvn.sbClient}
		key, err := retry.GetResourceKey(pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if role == types.NetworkRolePrimary {
			ginkgo.By("retaining primary-network pod work while the gateway is not ready")
			oc.gatewaysFailed.Store(node.Name, true)
			gomega.Expect(oc.addUpdateLocalNodeEvent(context.Background(), node, &nodeSyncs{syncPods: true})).To(gomega.Succeed())
			_, pending := oc.syncPodsFailed.Load(node.Name)
			gomega.Expect(pending).To(gomega.BeTrue())
			gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeFalse())
			oc.gatewaysFailed.Delete(node.Name)
		}

		ginkgo.By("enqueueing pods without any OVN writes")
		oldNode := node
		if bootstrap {
			oldNode = nil
		}
		gomega.Expect(oc.ReconcileNode(context.Background(), oldNode, node, nil, nil)).To(gomega.Succeed())
		gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeTrue())
		_, pending := oc.syncPodsFailed.Load(node.Name)
		gomega.Expect(pending).To(gomega.BeFalse())
		_, failed := oc.addNodeFailed.Load(node.Name)
		gomega.Expect(failed).To(gomega.BeFalse())
		oc.retryPods.DoWithLock(key, oc.retryPods.DeleteRetryObj)
		gomega.Expect(oc.ReconcileNode(context.Background(), node, node, nil, nil)).To(gomega.Succeed())
		gomega.Expect(retry.CheckRetryObj(key, oc.retryPods)).To(gomega.BeFalse())

		ginkgo.By("clearing pending pod work when the node is deleted")
		oc.nbClient = fakeOvn.nbClient
		oc.sbClient = fakeOvn.sbClient
		oc.syncPodsFailed.Store(node.Name, true)
		gomega.Expect(oc.ReconcileNode(context.Background(), node, nil, nil, nil)).To(gomega.Succeed())
		_, pending = oc.syncPodsFailed.Load(node.Name)
		gomega.Expect(pending).To(gomega.BeFalse())
	},
		ginkgo.Entry("primary bootstrap retry", types.NetworkRolePrimary, true),
		ginkgo.Entry("primary identical-node update retry", types.NetworkRolePrimary, false),
		ginkgo.Entry("secondary bootstrap retry", types.NetworkRoleSecondary, true),
		ginkgo.Entry("secondary identical-node update retry", types.NetworkRoleSecondary, false),
	)
})
