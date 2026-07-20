// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestHandleNetworkRefChangeUpdatesStatusAndMetrics(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	metrics.RegisterClusterManagerFunctional()

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_refchange_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.1.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()
	defer metrics.DeleteDynamicUDNNodeCount(networkName)

	var gotNetwork string
	var gotCondStatus string
	var gotCondMsg string
	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()
	nm := &networkmanager.FakeNetworkManager{}
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()
	ncc := &networkClusterController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		nodeReconciler:      nodeController,
		statusReporter: func(networkName, _ string, condition *metav1.Condition, _ ...*util.EventDetails) error {
			gotNetwork = networkName
			if condition != nil {
				gotCondStatus = string(condition.Status)
				gotCondMsg = condition.Message
			}
			return nil
		},
	}

	ncc.HandleNetworkRefChange("node1", true)
	g.Expect(gotNetwork).To(gomega.Equal(networkName))
	g.Expect(gotCondStatus).To(gomega.Equal(string(metav1.ConditionTrue)))
	g.Expect(gotCondMsg).To(gomega.Equal("1 node(s) rendered with network"))
	g.Expect(getUDNNodesRenderedMetric(t, networkName)).To(gomega.Equal(1.0))

	ncc.HandleNetworkRefChange("node1", true)
	g.Expect(gotNetwork).To(gomega.Equal(networkName))
	g.Expect(gotCondStatus).To(gomega.Equal(string(metav1.ConditionTrue)))
	g.Expect(gotCondMsg).To(gomega.Equal("1 node(s) rendered with network"))
	g.Expect(getUDNNodesRenderedMetric(t, networkName)).To(gomega.Equal(1.0))

	ncc.HandleNetworkRefChange("node2", true)
	g.Expect(gotNetwork).To(gomega.Equal(networkName))
	g.Expect(gotCondStatus).To(gomega.Equal(string(metav1.ConditionTrue)))
	g.Expect(gotCondMsg).To(gomega.Equal("2 node(s) rendered with network"))
	g.Expect(getUDNNodesRenderedMetric(t, networkName)).To(gomega.Equal(2.0))

	ncc.HandleNetworkRefChange("node2", false)
	g.Expect(gotNetwork).To(gomega.Equal(networkName))
	g.Expect(gotCondStatus).To(gomega.Equal(string(metav1.ConditionTrue)))
	g.Expect(gotCondMsg).To(gomega.Equal("1 node(s) rendered with network"))
	g.Expect(getUDNNodesRenderedMetric(t, networkName)).To(gomega.Equal(1.0))

	ncc.HandleNetworkRefChange("node1", false)
	ncc.HandleNetworkRefChange("node1", false)
	g.Expect(gotCondStatus).To(gomega.Equal(string(metav1.ConditionFalse)))
	g.Expect(gotCondMsg).To(gomega.Equal("no nodes currently rendered with network"))
	g.Expect(getUDNNodesRenderedMetric(t, networkName)).To(gomega.Equal(0.0))
}

func TestHandleNetworkRefChangeCleanupWithZeroGraceOnStart(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = 0

	metrics.RegisterClusterManagerFunctional()

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_cleanup_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.1.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()
	defer metrics.DeleteDynamicUDNNodeCount(networkName)

	nodeSubnet := ovntest.MustParseIPNet("10.1.0.0/24")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}
	node.Annotations, err = util.UpdateNodeHostSubnetAnnotation(node.Annotations, []*net.IPNet{nodeSubnet}, networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	node.Annotations, err = util.UpdateNetworkIDAnnotation(node.Annotations, networkName, 7)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClient := util.GetOVNClientset(node).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()
	nm := &networkmanager.FakeNetworkManager{}
	nm.SetNodeActive(networkName, node.Name, false)
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
		nodeController,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	defer ncc.Stop()

	g.Eventually(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		if util.HasNodeHostSubnetAnnotation(updatedNode, networkName) {
			return false
		}
		_, err = util.ParseNetworkIDAnnotation(updatedNode, networkName)
		return util.IsAnnotationNotSetError(err)
	}).Should(gomega.BeTrue())
}

func TestHandleNetworkRefChangeCleanupWithZeroGraceAfterStart(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = 0

	metrics.RegisterClusterManagerFunctional()

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_cleanup_after_start_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.2.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()
	defer metrics.DeleteDynamicUDNNodeCount(networkName)

	nodeSubnet := ovntest.MustParseIPNet("10.2.0.0/24")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}
	node.Annotations, err = util.UpdateNodeHostSubnetAnnotation(node.Annotations, []*net.IPNet{nodeSubnet}, networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	node.Annotations, err = util.UpdateNetworkIDAnnotation(node.Annotations, networkName, 7)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClient := util.GetOVNClientset(node).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()
	nm := &networkmanager.FakeNetworkManager{}
	nm.SetNodeActive(networkName, node.Name, true)
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
		nodeController,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	defer ncc.Stop()

	updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(util.HasNodeHostSubnetAnnotation(updatedNode, networkName)).To(gomega.BeTrue())

	nm.SetNodeActive(networkName, node.Name, false)
	ncc.HandleNetworkRefChange(node.Name, false)

	g.Eventually(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		if util.HasNodeHostSubnetAnnotation(updatedNode, networkName) {
			return false
		}
		_, err = util.ParseNetworkIDAnnotation(updatedNode, networkName)
		return util.IsAnnotationNotSetError(err)
	}).Should(gomega.BeTrue())
}

func TestHandleNetworkRefChangeAllocatesOnActivation(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_activate_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.10.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()
	defer metrics.DeleteDynamicUDNNodeCount(networkName)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}

	fakeClient := util.GetOVNClientset(node).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()
	nm := &networkmanager.FakeNetworkManager{}
	nm.SetNodeActive(networkName, node.Name, false)
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
		nodeController,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	defer ncc.Stop()

	// Ensure node does not get allocated while inactive.
	g.Consistently(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return util.HasNodeHostSubnetAnnotation(updatedNode, networkName)
	}, time.Second, 50*time.Millisecond).Should(gomega.BeFalse())

	// Activate and verify allocation occurs via retry framework.
	nm.SetNodeActive(networkName, node.Name, true)
	ncc.HandleNetworkRefChange(node.Name, true)

	g.Eventually(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return util.HasNodeHostSubnetAnnotation(updatedNode, networkName)
	}).Should(gomega.BeTrue())
}

func TestReconcileNodeCleansUpOnNoHostSubnetTransition(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	origNoHostSubnetNodes := config.Kubernetes.NoHostSubnetNodes
	config.Kubernetes.NoHostSubnetNodes, err = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"no-host-subnet": "true"},
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(func() {
		config.Kubernetes.NoHostSubnetNodes = origNoHostSubnetNodes
	})

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_no_host_subnet_transition_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.20.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()

	nodeSubnet := ovntest.MustParseIPNet("10.20.0.0/24")
	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}
	oldNode.Annotations, err = util.UpdateNodeHostSubnetAnnotation(oldNode.Annotations, []*net.IPNet{nodeSubnet}, networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	oldNode.Annotations, err = util.UpdateNetworkIDAnnotation(oldNode.Annotations, networkName, 7)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	newNode := oldNode.DeepCopy()
	newNode.Labels = map[string]string{"no-host-subnet": "true"}

	fakeClient := util.GetOVNClientset(newNode).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()

	nm := &networkmanager.FakeNetworkManager{}
	nm.SetNodeActive(networkName, newNode.Name, false)
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
		nodeController,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())

	g.Expect(ncc.ReconcileNode(oldNode, newNode, nil, nil)).To(gomega.Succeed())

	updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), newNode.Name, metav1.GetOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(util.HasNodeHostSubnetAnnotation(updatedNode, networkName)).To(gomega.BeFalse())
	_, err = util.ParseNetworkIDAnnotation(updatedNode, networkName)
	g.Expect(util.IsAnnotationNotSetError(err)).To(gomega.BeTrue())
}

func TestReconcileNodeMarksNodeSyncFailedOnCleanupError(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "ns1_udn1_cleanup_error_test",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.30.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	networkName := netInfo.GetNetworkName()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				util.OvnNetworkIDs: "{broken-json",
			},
		},
	}

	fakeClient := util.GetOVNClientset(node).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	defer wf.Shutdown()

	nm := &networkmanager.FakeNetworkManager{}
	nm.SetNodeActive(networkName, node.Name, false)
	nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
	g.Expect(nodeController.Start()).To(gomega.Succeed())
	defer nodeController.Stop()

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
		nodeController,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())

	err = ncc.ReconcileNode(nil, node, nil, nil)
	g.Expect(err).To(gomega.HaveOccurred())
	_, failed := ncc.nodeSyncFailed.Load(node.Name)
	g.Expect(failed).To(gomega.BeTrue())
	_, parseErr := util.ParseNetworkIDAnnotation(node, networkName)
	g.Expect(parseErr).To(gomega.HaveOccurred())
}

func TestInitReserveTransitTunnelKey(t *testing.T) {
	tests := []struct {
		name           string
		transitRouter  bool
		nodeTunnelIDs  map[string]int // node name -> tunnel ID annotation
		wantAllocMinID int            // verify AllocateID returns >= this value
	}{
		// Transit router ON: only the transit-router-to-switch key (ID 1) is
		// reserved. Node tunnel IDs are not consumed by the allocator.
		{
			name:           "transit router: no nodes, transit key reserved",
			transitRouter:  true,
			nodeTunnelIDs:  nil,
			wantAllocMinID: 2,
		},
		{
			name:          "transit router: node IDs are not consumed by allocator",
			transitRouter: true,
			nodeTunnelIDs: map[string]int{
				"node1": 10,
				"node2": 11,
			},
			wantAllocMinID: 2,
		},
		// Transit router OFF: no transit key reservation.
		// Node tunnel IDs are consumed by the allocator.
		{
			name:           "no transit router: no nodes, no transit key reserved",
			transitRouter:  false,
			nodeTunnelIDs:  nil,
			wantAllocMinID: 1,
		},
		{
			name:          "no transit router: node IDs consumed by allocator",
			transitRouter: false,
			nodeTunnelIDs: map[string]int{
				"node1": 10,
				"node2": 11,
			},
			wantAllocMinID: 1,
		},
		{
			name:          "no transit router: node with ID 1 consumed by allocator",
			transitRouter: false,
			nodeTunnelIDs: map[string]int{
				"node1": 1,
			},
			wantAllocMinID: 2,
		},
		{
			name:          "no transit router: node without annotation",
			transitRouter: false,
			nodeTunnelIDs: map[string]int{
				"node1": types.InvalidID,
			},
			wantAllocMinID: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.Layer2UsesTransitRouter = tt.transitRouter

			const networkName = "l2-primary-test"
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: networkName,
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: types.Layer2Topology,
				Role:     types.NetworkRolePrimary,
			}
			netInfo, err := util.NewNetInfo(netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			var nodes []corev1.Node
			for nodeName, tunnelID := range tt.nodeTunnelIDs {
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:        nodeName,
						Annotations: map[string]string{},
					},
				}
				if tunnelID != types.InvalidID {
					node.Annotations, err = util.UpdateUDNLayer2NodeGRLRPTunnelIDs(node.Annotations, networkName, tunnelID)
					g.Expect(err).ToNot(gomega.HaveOccurred())
				}
				nodes = append(nodes, node)
			}

			runtimeObjs := make([]runtime.Object, len(nodes))
			for i := range nodes {
				runtimeObjs[i] = &nodes[i]
			}
			fakeClient := util.GetOVNClientset(runtimeObjs...).GetClusterManagerClientset()

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(wf.Start()).To(gomega.Succeed())
			defer wf.Shutdown()

			nm := &networkmanager.FakeNetworkManager{}
			nodeController := nodecontroller.NewController(wf, "clustermanager-node", nm)
			g.Expect(nodeController.Start()).To(gomega.Succeed())
			defer nodeController.Stop()

			ncc := newNetworkClusterController(
				netInfo,
				fakeClient,
				wf,
				nil,
				nm,
				nil,
				nodeController,
			)

			err = ncc.init()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(ncc.tunnelIDAllocator).ToNot(gomega.BeNil())

			// Verify transit-router-to-switch key reservation depends on mode.
			// Reserve-then-release to probe without affecting subsequent allocation.
			transitKeyErr := ncc.tunnelIDAllocator.ReserveID("transit-key-probe", transitRouterToSwitchTunnelKey)
			if transitKeyErr == nil {
				ncc.tunnelIDAllocator.ReleaseID("transit-key-probe")
			}
			if tt.transitRouter {
				g.Expect(transitKeyErr).To(gomega.HaveOccurred(),
					"transit key (ID %d) should be reserved when transit router is enabled", transitRouterToSwitchTunnelKey)
			}

			// Verify the next allocated ID reflects the reservation state:
			// transit router ON reserves ID 1 so pods start at 2;
			// transit router OFF has no reservation so pods start at 1
			// (unless a node consumed low IDs).
			nextID, allocErr := ncc.tunnelIDAllocator.AllocateID("test-pod")
			g.Expect(allocErr).ToNot(gomega.HaveOccurred())
			g.Expect(nextID).To(gomega.BeNumerically(">=", tt.wantAllocMinID),
				"allocated ID should be >= %d", tt.wantAllocMinID)
		})
	}
}

func getUDNNodesRenderedMetric(t *testing.T, networkName string) float64 {
	t.Helper()

	metricName := fmt.Sprintf("%s_%s_%s",
		types.MetricOvnkubeNamespace,
		types.MetricOvnkubeSubsystemClusterManager,
		"udn_nodes_rendered",
	)
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if labelValue(metric.GetLabel(), "network_name") != networkName {
				continue
			}
			if metric.GetGauge() == nil {
				t.Fatalf("metric %s for %s is not a gauge", metricName, networkName)
			}
			return metric.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s with network_name=%s not found", metricName, networkName)
	return 0
}

func labelValue(labels []*dto.LabelPair, name string) string {
	for _, label := range labels {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}
