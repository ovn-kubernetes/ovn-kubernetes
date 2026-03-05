package topology

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

type fakeNodeHandler struct {
	netName        string
	syncErr        error
	syncCalls      int
	reconcileCalls int
	deleteCalls    int
}

func (f *fakeNodeHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakeNodeHandler) ReconcileNode(_, _ *corev1.Node, _, _ *NodeAnnotationState) error {
	f.reconcileCalls++
	return nil
}

func (f *fakeNodeHandler) DeleteNode(_ *corev1.Node, _ *NodeAnnotationState) error {
	f.deleteCalls++
	return nil
}

func (f *fakeNodeHandler) SyncNodes(_ []*corev1.Node) error {
	f.syncCalls++
	return f.syncErr
}

func newNodeLister(t *testing.T, nodes ...*corev1.Node) corelisters.NodeLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, node := range nodes {
		if err := indexer.Add(node); err != nil {
			t.Fatalf("failed to add node to indexer: %v", err)
		}
	}
	return corelisters.NewNodeLister(indexer)
}

func newNodeControllerForTest(threadiness int, reconcileAllCounter *int) controller.Controller {
	return controller.NewController("topology-test-node-controller", &controller.ControllerConfig[corev1.Node]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   func(string) error { return nil },
		Threadiness: threadiness,
		Lister: func(labels.Selector) ([]*corev1.Node, error) {
			if reconcileAllCounter != nil {
				*reconcileAllCounter++
			}
			return nil, nil
		},
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
	})
}

func TestNodeControllerStartFailure(t *testing.T) {
	c := &NodeController{
		name:            "topology-test",
		nodeController:  newNodeControllerForTest(0, nil),
		handlers:        syncmap.NewSyncMap[NodeHandler](),
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
	}

	err := c.Start()
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if c.started {
		t.Fatal("expected controller to remain not-started on Start failure")
	}
}

func TestRegisterNetworkControllerBootstrapFailure(t *testing.T) {
	handler := &fakeNodeHandler{
		netName: "net-a",
		syncErr: errors.New("sync failed"),
	}
	c := &NodeController{
		name:            "topology-test",
		nodeController:  newNodeControllerForTest(1, nil),
		nodeLister:      newNodeLister(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}),
		handlers:        syncmap.NewSyncMap[NodeHandler](),
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
	}

	c.RegisterNetworkController(handler)
	if handler.syncCalls != 1 {
		t.Fatalf("expected first bootstrap attempt, got %d SyncNodes calls", handler.syncCalls)
	}
}

func TestRegisterNetworkControllerPanicsOnDuplicateNetworkHandler(t *testing.T) {
	c := &NodeController{
		name:            "topology-test",
		nodeController:  newNodeControllerForTest(1, nil),
		nodeLister:      newNodeLister(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}),
		handlers:        syncmap.NewSyncMap[NodeHandler](),
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
	}
	h1 := &fakeNodeHandler{netName: "net-a"}
	h2 := &fakeNodeHandler{netName: "net-a"}

	c.RegisterNetworkController(h1)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected duplicate handler registration to panic")
		}
	}()
	c.RegisterNetworkController(h2)
}

func TestDeregisterNetworkControllerClearsNetworkState(t *testing.T) {
	handler := &fakeNodeHandler{netName: "net-a"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handler.netName, handler)
	c := &NodeController{
		handlers: handlers,
		bootstrapNodes: map[string]map[string]struct{}{
			handler.netName: {"node-a": {}},
		},
		nodeActive: map[string]map[string]bool{
			handler.netName: {"node-a": true},
		},
	}

	c.DeregisterNetworkController(handler.netName)

	if got, ok := c.handlers.Load(handler.netName); ok || got != nil {
		t.Fatalf("expected handler for %q to be removed", handler.netName)
	}
	if _, ok := c.bootstrapNodes[handler.netName]; ok {
		t.Fatalf("expected bootstrap nodes for %q to be removed", handler.netName)
	}
	if _, ok := c.nodeActive[handler.netName]; ok {
		t.Fatalf("expected node activity for %q to be removed", handler.netName)
	}
}

func TestScopedNodeQueueKeyRoundTrip(t *testing.T) {
	key := scopedNodeQueueKey("node-a", "net-a")
	nodeName, netName := parseScopedNodeQueueKey(key)
	if nodeName != "node-a" || netName != "net-a" {
		t.Fatalf("unexpected parse result: node=%q net=%q", nodeName, netName)
	}
}

func TestReconcileUpdateScopedNetworkOnly(t *testing.T) {
	handlerA := &fakeNodeHandler{netName: "net-a"}
	handlerB := &fakeNodeHandler{netName: "net-b"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handlerA.netName, handlerA)
	handlers.Store(handlerB.netName, handlerB)

	c := &NodeController{
		networkManager:  networkmanager.Default().Interface(),
		handlers:        handlers,
		bootstrapNodes:  map[string]map[string]struct{}{},
		nodeActive:      map[string]map[string]bool{},
		annotationCache: newNodeAnnotationCache(),
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}

	if err := c.reconcileUpdate(nil, node, "net-a"); err != nil {
		t.Fatalf("reconcileUpdate returned error: %v", err)
	}
	if handlerA.reconcileCalls != 1 {
		t.Fatalf("expected net-a handler to be called once, got %d", handlerA.reconcileCalls)
	}
	if handlerB.reconcileCalls != 0 {
		t.Fatalf("expected net-b handler to not be called, got %d", handlerB.reconcileCalls)
	}
}
