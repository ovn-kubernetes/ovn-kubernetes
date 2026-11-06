// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type fakeNodeHandler struct {
	netName        string
	syncErr        error
	reconcileErr   error
	syncCalls      int
	reconcileCalls int
	deleteCalls    int
	lastOldNode    *corev1.Node
	gatewaySyncs   []string
}

func (f *fakeNodeHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakeNodeHandler) ReconcileNode(_ context.Context, oldNode *corev1.Node, newNode *corev1.Node, _, _ *NodeAnnotationState) error {
	if oldNode != nil {
		f.lastOldNode = oldNode.DeepCopy()
	} else {
		f.lastOldNode = nil
	}
	if newNode == nil {
		f.deleteCalls++
		return nil
	}
	f.reconcileCalls++
	return f.reconcileErr
}

func (f *fakeNodeHandler) SyncNodes(_ []*corev1.Node) error {
	f.syncCalls++
	return f.syncErr
}

func (f *fakeNodeHandler) MarkGatewaySyncNeeded(nodeName string) {
	f.gatewaySyncs = append(f.gatewaySyncs, nodeName)
}

type fakeNodeActivityNetworkManager struct {
	networkmanager.FakeNetworkManager
	active bool
}

func (f *fakeNodeActivityNetworkManager) NodeHasNetwork(_, _ string) bool {
	return f.active
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
		Reconcile:   func(_ context.Context, _ string) error { return nil },
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
		name:               "topology-test",
		networkManager:     networkmanager.Default().Interface(),
		nodeController:     newNodeControllerForTest(0, nil),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}

	err := c.Start()
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if c.started {
		t.Fatal("expected controller to remain not-started on Start failure")
	}
}

func TestNodeControllerStartFailureRollsBackBothQueues(t *testing.T) {
	tests := []struct {
		name              string
		sharedThreadiness int
		localThreadiness  int
	}{
		{name: "shared queue", sharedThreadiness: 0, localThreadiness: 1},
		{name: "local queue", sharedThreadiness: 1, localThreadiness: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sharedRan := make(chan struct{}, 1)
			localRan := make(chan struct{}, 1)
			c := &NodeController{
				name:           "topology-test",
				networkManager: networkmanager.Default().Interface(),
				nodeController: controller.NewController("topology-test-node-controller-"+tt.name, &controller.ControllerConfig[corev1.Node]{
					RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
					Reconcile:      func(context.Context, string) error { sharedRan <- struct{}{}; return nil },
					Threadiness:    tt.sharedThreadiness,
					Lister:         func(labels.Selector) ([]*corev1.Node, error) { return nil, nil },
					ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
				}),
				localNodeController: controller.NewReconciler("topology-test-local-node-"+tt.name, &controller.ReconcilerConfig{
					RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
					Reconcile:   func(context.Context, string) error { localRan <- struct{}{}; return nil },
					Threadiness: tt.localThreadiness,
				}),
				handlers:           syncmap.NewSyncMap[NodeHandler](),
				nodeReconciliation: map[string]map[string]bool{},
				nodeActive:         map[string]map[string]struct{}{},
				nodeCache:          map[string]map[string]*corev1.Node{},
				annotationCache:    NewNodeAnnotationCache(),
			}

			err := c.Start()
			if err == nil {
				t.Fatal("expected Start to fail")
			}
			if c.started {
				t.Fatal("expected controller to remain not-started on Start failure")
			}

			c.nodeController.Reconcile("node-a|net-a")
			c.localNodeController.Reconcile("node-a|net-a")
			select {
			case <-sharedRan:
				t.Fatal("shared queue reconciled after Start failure")
			case <-localRan:
				t.Fatal("local queue reconciled after Start failure")
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

func TestRegisterNetworkControllerBootstrapFailure(t *testing.T) {
	handler := &fakeNodeHandler{
		netName: "net-a",
		syncErr: errors.New("sync failed"),
	}
	c := &NodeController{
		name:               "topology-test",
		networkManager:     networkmanager.Default().Interface(),
		nodeController:     newNodeControllerForTest(1, nil),
		nodeLister:         newNodeLister(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}

	err := c.RegisterNetworkController(handler)
	if err == nil {
		t.Fatal("expected bootstrap failure to be returned")
	}
	if handler.syncCalls != 1 {
		t.Fatalf("expected first bootstrap attempt, got %d SyncNodes calls", handler.syncCalls)
	}
	if got, ok := c.handlers.Load(handler.netName); ok || got != nil {
		t.Fatalf("expected failed bootstrap to roll back handler registration for %q", handler.netName)
	}
}

func TestRegisterNetworkControllerPanicsOnDuplicateNetworkHandler(t *testing.T) {
	c := &NodeController{
		name:               "topology-test",
		networkManager:     networkmanager.Default().Interface(),
		nodeController:     newNodeControllerForTest(1, nil),
		nodeLister:         newNodeLister(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	h1 := &fakeNodeHandler{netName: "net-a"}
	h2 := &fakeNodeHandler{netName: "net-a"}

	if err := c.RegisterNetworkController(h1); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected duplicate handler registration to panic")
		}
	}()
	_ = c.RegisterNetworkController(h2)
}

func TestDeregisterNetworkControllerClearsNetworkState(t *testing.T) {
	handler := &fakeNodeHandler{netName: "net-a"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handler.netName, handler)
	c := &NodeController{
		networkManager: networkmanager.Default().Interface(),
		handlers:       handlers,
		nodeReconciliation: map[string]map[string]bool{
			handler.netName: {"node-a": false},
		},
		nodeActive: map[string]map[string]struct{}{
			handler.netName: {"node-a": {}},
		},
	}

	c.DeregisterNetworkController(handler.netName)

	if got, ok := c.handlers.Load(handler.netName); ok || got != nil {
		t.Fatalf("expected handler for %q to be removed", handler.netName)
	}
	if _, ok := c.nodeReconciliation[handler.netName]; ok {
		t.Fatalf("expected bootstrap nodes for %q to be removed", handler.netName)
	}
	if _, ok := c.nodeActive[handler.netName]; ok {
		t.Fatalf("expected configured state for %q to be removed", handler.netName)
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
		networkManager:     networkmanager.Default().Interface(),
		handlers:           handlers,
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	newState := c.annotationCache.UpdateNodeAnnotationState(node, true)

	if err := c.reconcileUpdate(context.Background(), handlerA, nil, node, "net-a", nil, newState); err != nil {
		t.Fatalf("reconcileUpdate returned error: %v", err)
	}
	if handlerA.reconcileCalls != 1 {
		t.Fatalf("expected net-a handler to be called once, got %d", handlerA.reconcileCalls)
	}
	if handlerB.reconcileCalls != 0 {
		t.Fatalf("expected net-b handler to not be called, got %d", handlerB.reconcileCalls)
	}
}

func TestReconcileNetworkWithPreflightScopesHandler(t *testing.T) {
	handlerA := &fakeNodeHandler{netName: "net-a"}
	handlerB := &fakeNodeHandler{netName: "net-b"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handlerA.netName, handlerA)
	handlers.Store(handlerB.netName, handlerB)

	c := &NodeController{
		nodeController: newNodeControllerForTest(1, nil),
		handlers:       handlers,
	}

	c.ReconcileNetworkWithPreflight("node-a", "net-a", func(handler NodeHandler) {
		handler.(*fakeNodeHandler).MarkGatewaySyncNeeded("node-a")
	})

	if len(handlerA.gatewaySyncs) != 1 || handlerA.gatewaySyncs[0] != "node-a" {
		t.Fatalf("expected net-a preflight for node-a, got %v", handlerA.gatewaySyncs)
	}
	if len(handlerB.gatewaySyncs) != 0 {
		t.Fatalf("expected net-b preflight to not run, got %v", handlerB.gatewaySyncs)
	}
}

func TestNodeControllerFiltersRemoteNetworkActivityByNodeName(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	controller := &NodeController{networkManager: networkmanager.Default().Interface(), nodeName: "local-node"}
	remoteNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "remote-node",
			Annotations: map[string]string{
				util.OvnNodeZoneName: "local-node",
			},
		},
	}
	localNode := remoteNode.DeepCopy()
	localNode.Name = "local-node"
	localNode.Annotations[util.OvnNodeZoneName] = "some-other-zone"

	if controller.shouldFilterByRemoteNetworkActivity(remoteNode, "default") {
		t.Fatal("expected default network to skip dynamic UDN remote activity filtering")
	}
	if !controller.shouldFilterByRemoteNetworkActivity(remoteNode, "net-a") {
		t.Fatal("expected non-default network to apply dynamic UDN remote activity filtering")
	}
	if controller.shouldFilterByRemoteNetworkActivity(localNode, "net-a") {
		t.Fatal("expected the controller node to skip remote activity filtering regardless of its zone annotation")
	}
}

func TestReconcileUpdateFailureKeepsConfiguredCacheAndStoresLatestInformerNode(t *testing.T) {
	handler := &fakeNodeHandler{
		netName:      "net-a",
		reconcileErr: errors.New("reconcile failed"),
	}
	oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Annotations: map[string]string{"v": "old"}}}
	newNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Annotations: map[string]string{"v": "new"}}}

	c := &NodeController{
		networkManager:     networkmanager.Default().Interface(),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache: map[string]map[string]*corev1.Node{
			handler.netName: {oldNode.Name: oldNode.DeepCopy()},
		},
		annotationCache: NewNodeAnnotationCache(),
	}

	newState := c.annotationCache.UpdateNodeAnnotationState(newNode, true)
	err := c.reconcileUpdate(context.Background(), handler, oldNode, newNode, handler.netName, nil, newState)
	if err == nil {
		t.Fatal("expected reconcileUpdate to fail")
	}

	cachedNode := c.getCachedNode(handler.netName, oldNode.Name)
	if cachedNode == nil || cachedNode.Annotations["v"] != "old" {
		t.Fatalf("expected configured cache to retain old node, got %#v", cachedNode)
	}

	latestInformerNode := c.getLatestInformerNode(handler.netName, oldNode.Name)
	if latestInformerNode == nil || latestInformerNode.Annotations["v"] != "new" {
		t.Fatalf("expected latest informer cache to store new node, got %#v", latestInformerNode)
	}
}

func TestReconcileDeleteFallsBackToLatestInformerNode(t *testing.T) {
	handler := &fakeNodeHandler{netName: "net-a"}
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Annotations: map[string]string{"cleanup": "needed"},
		},
	}

	c := &NodeController{
		networkManager:     networkmanager.Default().Interface(),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	c.setLatestInformerNode(handler.netName, newNode)

	if err := c.reconcileDelete(context.Background(), handler, newNode.Name, handler.netName, c.getCachedNode(handler.netName, newNode.Name), nil); err != nil {
		t.Fatalf("reconcileDelete returned error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected delete to be called once, got %d", handler.deleteCalls)
	}
	if handler.lastOldNode == nil || handler.lastOldNode.Annotations["cleanup"] != "needed" {
		t.Fatalf("expected delete to use latest informer node, got %#v", handler.lastOldNode)
	}
	if c.getLatestInformerNode(handler.netName, newNode.Name) != nil {
		t.Fatal("expected latest informer cache to be cleared after delete")
	}
}

func TestReconcileNodeRemoteNodeBecomesActiveTreatsAsAdd(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	handler := &fakeNodeHandler{netName: "net-a"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handler.netName, handler)

	fakeNM := &fakeNodeActivityNetworkManager{active: true}
	c := &NodeController{
		nodeName:           "local-node",
		networkManager:     fakeNM,
		handlers:           handlers,
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
		},
	}
	c.nodeLister = newNodeLister(t, node)

	if err := c.reconcileNode(context.Background(), scopedNodeQueueKey(node.Name, handler.netName)); err != nil {
		t.Fatalf("reconcileNode returned error: %v", err)
	}
	if handler.reconcileCalls != 1 {
		t.Fatalf("expected remote active node to reconcile once, got %d calls", handler.reconcileCalls)
	}
	if handler.deleteCalls != 0 {
		t.Fatalf("expected no delete calls, got %d", handler.deleteCalls)
	}
	if !c.nodeHasNetwork(handler.netName, node.Name) {
		t.Fatal("expected node to be marked active")
	}
	if c.nodeNeedsReconciliation(handler.netName, node.Name) {
		t.Fatal("expected reconciliation marker to be cleared")
	}
}

func TestReconcileNodeRemoteNodeBecomesInactiveDeletes(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	handler := &fakeNodeHandler{netName: "net-a"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handler.netName, handler)

	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
		},
	}

	c := &NodeController{
		nodeName:           "local-node",
		networkManager:     &fakeNodeActivityNetworkManager{active: false},
		handlers:           handlers,
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive: map[string]map[string]struct{}{
			handler.netName: {"node-a": {}},
		},
		nodeCache: map[string]map[string]*corev1.Node{
			handler.netName: {"node-a": oldNode.DeepCopy()},
		},
		annotationCache: NewNodeAnnotationCache(),
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
		},
	}
	c.nodeLister = newNodeLister(t, node)

	if err := c.reconcileNode(context.Background(), scopedNodeQueueKey(node.Name, handler.netName)); err != nil {
		t.Fatalf("reconcileNode returned error: %v", err)
	}
	if handler.reconcileCalls != 0 {
		t.Fatalf("expected inactive node to skip add/update, got %d reconcile calls", handler.reconcileCalls)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected inactive node to invoke delete once, got %d", handler.deleteCalls)
	}
	if c.nodeHasNetwork(handler.netName, node.Name) {
		t.Fatal("expected node configured state to remain inactive")
	}
	if c.nodeNeedsDeleteReconciliation(handler.netName, node.Name) {
		t.Fatal("expected delete reconciliation marker to be cleared")
	}
}

func TestReconcileNodeDeleteCacheMissStillClearsState(t *testing.T) {
	handler := &fakeNodeHandler{netName: "net-a"}
	handlers := syncmap.NewSyncMap[NodeHandler]()
	handlers.Store(handler.netName, handler)

	c := &NodeController{
		name:               "topology-test",
		networkManager:     &fakeNodeActivityNetworkManager{active: false},
		nodeLister:         newNodeLister(t),
		handlers:           handlers,
		nodeReconciliation: map[string]map[string]bool{handler.netName: {"node-a": true}},
		nodeActive:         map[string]map[string]struct{}{handler.netName: {"node-a": {}}},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}

	if err := c.reconcileNode(context.Background(), scopedNodeQueueKey("node-a", handler.netName)); err != nil {
		t.Fatalf("reconcileNode returned error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected cache-miss delete to invoke handler once, got %d delete calls", handler.deleteCalls)
	}
	if c.nodeHasNetwork(handler.netName, "node-a") {
		t.Fatal("expected node active state to be cleared")
	}
	if c.nodeNeedsDeleteReconciliation(handler.netName, "node-a") {
		t.Fatal("expected delete reconciliation marker to be cleared")
	}
}

// bootstrapQueuedKeysForTest registers a handler for netName and returns the keys dequeued by the
// shared remote queue and, when nodeName is one of nodes, the dedicated local queue.
func bootstrapQueuedKeysForTest(t *testing.T, nodeName, netName string, nodes []*corev1.Node) (remoteKeys, localKeys []string) {
	t.Helper()
	var mu sync.Mutex
	done := make(chan struct{})
	record := func(dst *[]string) func(context.Context, string) error {
		return func(_ context.Context, key string) error {
			mu.Lock()
			defer mu.Unlock()
			*dst = append(*dst, key)
			if len(remoteKeys)+len(localKeys) == len(nodes) {
				close(done)
			}
			return nil
		}
	}
	nodeController := controller.NewController("topology-test-node-controller", &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      record(&remoteKeys),
		Threadiness:    1,
		Lister:         func(labels.Selector) ([]*corev1.Node, error) { return nil, nil },
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
	})
	c := &NodeController{
		name:               "topology-test",
		nodeName:           nodeName,
		networkManager:     networkmanager.Default().Interface(),
		nodeController:     nodeController,
		nodeLister:         newNodeLister(t, nodes...),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	reconcilers := []controller.Reconciler{nodeController}
	if nodeListed(nodeName, nodes) {
		c.localNodeController = controller.NewReconciler("topology-test-local-node", &controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   record(&localKeys),
			Threadiness: 1,
		})
		reconcilers = append(reconcilers, c.localNodeController)
	}

	// Keys are queued here, before any worker runs.
	if err := c.RegisterNetworkController(&fakeNodeHandler{netName: netName}); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := controller.Start(reconcilers...); err != nil {
		t.Fatalf("failed to start node controller: %v", err)
	}
	defer controller.Stop(reconcilers...)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("timed out waiting for %d keys, got %d remote and %d local", len(nodes), len(remoteKeys), len(localKeys))
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), remoteKeys...), append([]string(nil), localKeys...)
}

func nodeListed(nodeName string, nodes []*corev1.Node) bool {
	for _, node := range nodes {
		if node.Name == nodeName {
			return true
		}
	}
	return false
}

func testNodes(count int) []*corev1.Node {
	nodes := make([]*corev1.Node, 0, count)
	for i := 0; i < count; i++ {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%04d", i)}})
	}
	return nodes
}

func TestBootstrapNetworkQueuesLocalNodeSeparately(t *testing.T) {
	const netName = "net-a"
	nodes := testNodes(50)
	localNode := nodes[len(nodes)-1].Name

	remoteKeys, localKeys := bootstrapQueuedKeysForTest(t, localNode, netName, nodes)

	if len(localKeys) != 1 || localKeys[0] != scopedNodeQueueKey(localNode, netName) {
		t.Fatalf("expected only the local node on the local queue, got %q", localKeys)
	}
	if len(remoteKeys) != len(nodes)-1 {
		t.Fatalf("expected %d remote keys, got %d", len(nodes)-1, len(remoteKeys))
	}
	seen := make(map[string]struct{}, len(remoteKeys))
	for _, key := range remoteKeys {
		if key == localKeys[0] {
			t.Fatalf("local node %q was also queued on the remote queue", key)
		}
		if _, dup := seen[key]; dup {
			t.Fatalf("key %q queued more than once", key)
		}
		seen[key] = struct{}{}
	}
	for _, node := range nodes[:len(nodes)-1] {
		if _, ok := seen[scopedNodeQueueKey(node.Name, netName)]; !ok {
			t.Fatalf("node %q was not queued", node.Name)
		}
	}
}

func TestLocalNodeQueueRunsWhileRemoteQueueIsBlocked(t *testing.T) {
	const netName = "net-a"
	nodes := testNodes(20)
	localNode := nodes[len(nodes)-1].Name

	remoteStarted := make(chan struct{})
	releaseRemote := make(chan struct{})
	var once sync.Once
	nodeController := controller.NewController("topology-test-node-controller-blocked", &controller.ControllerConfig[corev1.Node]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(context.Context, string) error {
			once.Do(func() { close(remoteStarted) })
			<-releaseRemote
			return nil
		},
		Threadiness:    1,
		Lister:         func(labels.Selector) ([]*corev1.Node, error) { return nil, nil },
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
	})
	localDone := make(chan string, 1)
	localController := controller.NewReconciler("topology-test-local-node-blocked", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(_ context.Context, key string) error {
			localDone <- key
			return nil
		},
		Threadiness: 1,
	})
	c := &NodeController{
		name:                "topology-test",
		nodeName:            localNode,
		networkManager:      networkmanager.Default().Interface(),
		nodeController:      nodeController,
		localNodeController: localController,
		nodeLister:          newNodeLister(t, nodes...),
		handlers:            syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation:  map[string]map[string]bool{},
		nodeActive:          map[string]map[string]struct{}{},
		nodeCache:           map[string]map[string]*corev1.Node{},
		annotationCache:     NewNodeAnnotationCache(),
	}

	if err := c.RegisterNetworkController(&fakeNodeHandler{netName: netName}); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := controller.Start(nodeController, localController); err != nil {
		t.Fatalf("failed to start node controller: %v", err)
	}
	defer func() {
		close(releaseRemote)
		controller.Stop(nodeController, localController)
	}()

	select {
	case key := <-localDone:
		if want := scopedNodeQueueKey(localNode, netName); key != want {
			t.Fatalf("expected local queue to reconcile %q, got %q", want, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("local node was not reconciled while the remote queue was blocked")
	}
}

func TestRemoteReconcileWaitsUntilLocalNodeHasRun(t *testing.T) {
	const netName = "net-a"
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false

	localNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "local-node"}}
	remoteNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote-node"}}
	handler := &fakeNodeHandler{netName: netName}
	c := &NodeController{
		name:               "topology-test",
		nodeName:           localNode.Name,
		networkManager:     networkmanager.Default().Interface(),
		nodeLister:         newNodeLister(t, localNode, remoteNode),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	c.nodeController = controller.NewController("topology-test-node-controller-gate", &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(context.Context, string) error { return nil },
		Threadiness:    1,
		Lister:         func(labels.Selector) ([]*corev1.Node, error) { return nil, nil },
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
	})
	c.localNodeController = controller.NewReconciler("topology-test-local-node-gate", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   func(context.Context, string) error { return nil },
		Threadiness: 1,
	})

	if err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	remoteKey := scopedNodeQueueKey(remoteNode.Name, netName)
	if err := c.reconcileNode(context.Background(), remoteKey); err != nil {
		t.Fatalf("remote reconcile returned error: %v", err)
	}
	if handler.reconcileCalls != 0 {
		t.Fatalf("remote node reconciled before the local node, calls=%d", handler.reconcileCalls)
	}

	if err := c.reconcileNode(context.Background(), scopedNodeQueueKey(localNode.Name, netName)); err != nil {
		t.Fatalf("local reconcile returned error: %v", err)
	}
	if handler.reconcileCalls != 1 || handler.lastOldNode != nil {
		t.Fatalf("expected the local node to reconcile once as an add, calls=%d old=%v", handler.reconcileCalls, handler.lastOldNode)
	}

	if err := c.reconcileNode(context.Background(), remoteKey); err != nil {
		t.Fatalf("remote reconcile after local returned error: %v", err)
	}
	if handler.reconcileCalls != 2 {
		t.Fatalf("expected the remote node to reconcile after the local node, calls=%d", handler.reconcileCalls)
	}
}

func TestFailedLocalReconcileKeepsPriority(t *testing.T) {
	const netName = "net-a"
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false

	localNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "local-node"}}
	remoteNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote-node"}}
	handler := &fakeNodeHandler{netName: netName, reconcileErr: errors.New("transient")}
	c := &NodeController{
		name:               "topology-test",
		nodeName:           localNode.Name,
		networkManager:     networkmanager.Default().Interface(),
		nodeLister:         newNodeLister(t, localNode, remoteNode),
		handlers:           syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation: map[string]map[string]bool{},
		nodeActive:         map[string]map[string]struct{}{},
		nodeCache:          map[string]map[string]*corev1.Node{},
		annotationCache:    NewNodeAnnotationCache(),
	}
	c.nodeController = controller.NewController("topology-test-node-controller-gate-retry", &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(context.Context, string) error { return nil },
		Threadiness:    1,
		Lister:         func(labels.Selector) ([]*corev1.Node, error) { return nil, nil },
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
	})
	c.localNodeController = controller.NewReconciler("topology-test-local-node-gate-retry", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   func(context.Context, string) error { return nil },
		Threadiness: 1,
	})

	if err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	localKey := scopedNodeQueueKey(localNode.Name, netName)
	remoteKey := scopedNodeQueueKey(remoteNode.Name, netName)
	if err := c.reconcileNode(context.Background(), localKey); err == nil {
		t.Fatal("expected local reconcile to fail")
	}
	if !c.waitingForLocalNode(netName) {
		t.Fatal("failed local reconcile cleared local-node priority")
	}
	if err := c.reconcileNode(context.Background(), remoteKey); err != nil {
		t.Fatalf("remote reconcile returned error: %v", err)
	}
	if handler.reconcileCalls != 1 {
		t.Fatalf("remote node reconciled while the local retry still has priority, calls=%d", handler.reconcileCalls)
	}

	handler.reconcileErr = nil
	if err := c.reconcileNode(context.Background(), localKey); err != nil {
		t.Fatalf("local reconcile retry returned error: %v", err)
	}
	if c.waitingForLocalNode(netName) {
		t.Fatal("successful local reconcile left priority in place")
	}
	if err := c.reconcileNode(context.Background(), remoteKey); err != nil {
		t.Fatalf("remote reconcile after local success returned error: %v", err)
	}
	if handler.reconcileCalls != 3 {
		t.Fatalf("expected the remote node to reconcile after local success, calls=%d", handler.reconcileCalls)
	}
}

func TestBootstrapNetworkQueuesAllNodesWithoutLocalNode(t *testing.T) {
	const netName = "net-a"
	nodes := testNodes(50)

	for _, nodeName := range []string{"", "not-in-the-cluster"} {
		remoteKeys, localKeys := bootstrapQueuedKeysForTest(t, nodeName, netName, nodes)
		if len(localKeys) != 0 {
			t.Fatalf("nodeName %q: expected no local queue, got %q", nodeName, localKeys)
		}
		if len(remoteKeys) != len(nodes) {
			t.Fatalf("nodeName %q: expected %d queued keys, got %d", nodeName, len(nodes), len(remoteKeys))
		}
	}
}
