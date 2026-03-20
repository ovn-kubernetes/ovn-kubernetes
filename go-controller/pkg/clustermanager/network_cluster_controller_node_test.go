package clustermanager

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cmnode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	sharednode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestShouldReconcileNodeIgnoresOtherNetworkTunnelIDChanges(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableInterconnect = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	netInfo := mustClusterManagerNetInfo(t, &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "blue", Type: "ovn-k8s-cni-overlay"},
		Topology: types.Layer2Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.1.0.0/16",
	})
	ncc := &networkClusterController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}
	ncc.nodeAllocator = cmnode.NewNodeAllocator(1, netInfo, nil, nil, nil)

	oldNode := testNodeWithAnnotations("node1", map[string]string{
		types.UDNLayer2NodeGRLRPTunnelIDAnnotation: `{"blue":"7","red":"9"}`,
	})
	newNode := testNodeWithAnnotations("node1", map[string]string{
		types.UDNLayer2NodeGRLRPTunnelIDAnnotation: `{"blue":"7","red":"10"}`,
	})

	cache := sharednode.NewNodeAnnotationCache()
	oldState := cache.UpdateNodeAnnotationState(oldNode, false)
	newState := cache.UpdateNodeAnnotationState(newNode, true)

	if ncc.shouldReconcileNode(oldNode, newNode, oldState, newState, false, false) {
		t.Fatal("expected unchanged network tunnel ID to avoid reconciliation")
	}
}

func TestShouldReconcileNodeIgnoresOtherNetworkSubnetChanges(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}

	netInfo := mustClusterManagerNetInfo(t, &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName, Type: "ovn-k8s-cni-overlay"},
	})
	ncc := &networkClusterController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}
	ncc.nodeAllocator = cmnode.NewNodeAllocator(1, netInfo, nil, nil, nil)

	oldNode := testNodeWithAnnotations("node1", map[string]string{
		types.NodeSubnetsAnnotation: `{"default":"10.1.0.0/24","red":"10.2.0.0/24"}`,
	})
	newNode := testNodeWithAnnotations("node1", map[string]string{
		types.NodeSubnetsAnnotation: `{"default":"10.1.0.0/24","red":"10.2.1.0/24"}`,
	})

	cache := sharednode.NewNodeAnnotationCache()
	oldState := cache.UpdateNodeAnnotationState(oldNode, false)
	newState := cache.UpdateNodeAnnotationState(newNode, true)

	if ncc.shouldReconcileNode(oldNode, newNode, oldState, newState, false, false) {
		t.Fatal("expected unchanged network subnet to avoid reconciliation")
	}
}

func TestShouldReconcileNodeWhenNetworkSpecificTunnelIDChanges(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableInterconnect = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	netInfo := mustClusterManagerNetInfo(t, &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "blue", Type: "ovn-k8s-cni-overlay"},
		Topology: types.Layer2Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.1.0.0/16",
	})
	ncc := &networkClusterController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}
	ncc.nodeAllocator = cmnode.NewNodeAllocator(1, netInfo, nil, nil, nil)

	oldNode := testNodeWithAnnotations("node1", map[string]string{
		types.UDNLayer2NodeGRLRPTunnelIDAnnotation: `{"blue":"7","red":"9"}`,
	})
	newNode := testNodeWithAnnotations("node1", map[string]string{
		types.UDNLayer2NodeGRLRPTunnelIDAnnotation: `{"blue":"8","red":"9"}`,
	})

	cache := sharednode.NewNodeAnnotationCache()
	oldState := cache.UpdateNodeAnnotationState(oldNode, false)
	newState := cache.UpdateNodeAnnotationState(newNode, true)

	if !ncc.shouldReconcileNode(oldNode, newNode, oldState, newState, false, false) {
		t.Fatal("expected changed network tunnel ID to trigger reconciliation")
	}
}

func TestShouldReconcileNodeWhenNetworkSpecificSubnetChanges(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}

	netInfo := mustClusterManagerNetInfo(t, &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName, Type: "ovn-k8s-cni-overlay"},
	})
	ncc := &networkClusterController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}
	ncc.nodeAllocator = cmnode.NewNodeAllocator(1, netInfo, nil, nil, nil)

	oldNode := testNodeWithAnnotations("node1", map[string]string{
		types.NodeSubnetsAnnotation: `{"default":"10.1.0.0/24","red":"10.2.0.0/24"}`,
	})
	newNode := testNodeWithAnnotations("node1", map[string]string{
		types.NodeSubnetsAnnotation: `{"default":"10.1.1.0/24","red":"10.2.0.0/24"}`,
	})

	cache := sharednode.NewNodeAnnotationCache()
	oldState := cache.UpdateNodeAnnotationState(oldNode, false)
	newState := cache.UpdateNodeAnnotationState(newNode, true)

	if !ncc.shouldReconcileNode(oldNode, newNode, oldState, newState, false, false) {
		t.Fatal("expected changed network subnet to trigger reconciliation")
	}
}

func mustClusterManagerNetInfo(t *testing.T, netConf *ovncnitypes.NetConf) util.NetInfo {
	t.Helper()

	netInfo, err := util.NewNetInfo(netConf)
	if err != nil {
		t.Fatalf("failed to create netInfo: %v", err)
	}
	return netInfo
}

func testNodeWithAnnotations(name string, annotations map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}
