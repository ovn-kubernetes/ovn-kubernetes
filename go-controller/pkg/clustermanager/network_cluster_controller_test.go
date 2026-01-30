package clustermanager

import (
	"context"
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
	ncc := &networkClusterController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
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
	nm.SetNodeActive(node.Name, false)

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
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
	nm.SetNodeActive(node.Name, true)

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	defer ncc.Stop()

	updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(util.HasNodeHostSubnetAnnotation(updatedNode, networkName)).To(gomega.BeTrue())

	nm.SetNodeActive(node.Name, false)
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
	nm.SetNodeActive(node.Name, false)

	ncc := newNetworkClusterController(
		netInfo,
		fakeClient,
		wf,
		nil,
		nm,
		nil,
	)
	g.Expect(ncc.init()).To(gomega.Succeed())
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	defer ncc.Stop()

	// Ensure node does not get allocated while inactive.
	g.Eventually(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return util.HasNodeHostSubnetAnnotation(updatedNode, networkName)
	}).Should(gomega.BeFalse())

	// Activate and verify allocation occurs via retry framework.
	nm.SetNodeActive(node.Name, true)
	ncc.HandleNetworkRefChange(node.Name, true)

	g.Eventually(func() bool {
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return util.HasNodeHostSubnetAnnotation(updatedNode, networkName)
	}).Should(gomega.BeTrue())
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
