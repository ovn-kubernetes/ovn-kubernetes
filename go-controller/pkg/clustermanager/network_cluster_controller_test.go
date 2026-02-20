package clustermanager

import (
	"fmt"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
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
