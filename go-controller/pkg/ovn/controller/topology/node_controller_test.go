package topology

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	multinetworkmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

type fakeNodeHandler struct {
	netName   string
	syncErr   error
	syncCalls int
}

func (f *fakeNodeHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakeNodeHandler) ReconcileNode(_, _ *corev1.Node, _, _ *util.NodeAnnotationState) error {
	return nil
}

func (f *fakeNodeHandler) DeleteNode(_ *corev1.Node, _ *util.NodeAnnotationState) error {
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

func newNADReconcilerForTest(threadiness int) networkmanager.NADReconciler {
	return controller.NewReconciler("topology-test-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   func(string) error { return nil },
		Threadiness: threadiness,
		MaxAttempts: controller.InfiniteAttempts,
	})
}

func newNetInfoMock(networkName string) util.NetInfo {
	netInfo := &multinetworkmocks.NetInfo{}
	netInfo.On("GetNetworkName").Return(networkName)
	return netInfo
}

func TestNodeControllerStartDeregistersNADReconcilerOnStartFailure(t *testing.T) {
	nm := &networkmanager.FakeNetworkManager{}
	c := &NodeController{
		name:            "topology-test",
		nodeController:  newNodeControllerForTest(0, nil),
		nadReconciler:   newNADReconcilerForTest(1),
		networkManager:  nm,
		handlers:        map[string]NodeHandler{},
		nadToNetwork:    map[string]string{},
		networkRefs:     map[string]int{},
		bootstrapped:    map[string]bool{},
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
	}

	err := c.Start()
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if !strings.Contains(err.Error(), "threadiness should be > 0") {
		t.Fatalf("expected start failure from controller threadiness, got %v", err)
	}
	if got := len(nm.Reconcilers); got != 0 {
		t.Fatalf("expected NAD reconciler registration to be cleaned up, got %d remaining registrations", got)
	}
	if c.nadReconcilerID != 0 {
		t.Fatalf("expected nadReconcilerID to be reset, got %d", c.nadReconcilerID)
	}
}

func TestBootstrapActiveNetworksMarksBootstrappedOnlyOnSyncSuccess(t *testing.T) {
	nodes := []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	}
	okHandler := &fakeNodeHandler{netName: "net-ok"}
	failHandler := &fakeNodeHandler{
		netName: "net-fail",
		syncErr: errors.New("sync failed"),
	}
	reconcileAllCalls := 0

	c := &NodeController{
		nodeController: newNodeControllerForTest(1, &reconcileAllCalls),
		nodeLister:     newNodeLister(t, nodes...),
		handlers: map[string]NodeHandler{
			okHandler.netName:   okHandler,
			failHandler.netName: failHandler,
		},
		networkRefs:     map[string]int{okHandler.netName: 1, failHandler.netName: 1},
		bootstrapped:    map[string]bool{},
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
	}

	err := c.bootstrapActiveNetworks()
	if err == nil {
		t.Fatal("expected bootstrapActiveNetworks to return an error")
	}
	if !strings.Contains(err.Error(), "sync failed") {
		t.Fatalf("expected sync error to be returned, got %v", err)
	}
	if !c.isBootstrapped(okHandler.netName) {
		t.Fatalf("expected successful network %q to be bootstrapped", okHandler.netName)
	}
	if c.isBootstrapped(failHandler.netName) {
		t.Fatalf("expected failed network %q to remain unbootstrapped", failHandler.netName)
	}
	if !c.isBootstrapNode(okHandler.netName, "node-a") || !c.isBootstrapNode(okHandler.netName, "node-b") {
		t.Fatalf("expected bootstrap nodes to be tracked for network %q", okHandler.netName)
	}
	if c.isBootstrapNode(failHandler.netName, "node-a") || c.isBootstrapNode(failHandler.netName, "node-b") {
		t.Fatalf("did not expect bootstrap nodes to be tracked for failed network %q", failHandler.netName)
	}
	if reconcileAllCalls != 0 {
		t.Fatalf("expected ReconcileAll not to run when errors occur, got %d calls", reconcileAllCalls)
	}
	if okHandler.syncCalls != 1 || failHandler.syncCalls != 1 {
		t.Fatalf("expected each handler SyncNodes to be called once, got ok=%d fail=%d", okHandler.syncCalls, failHandler.syncCalls)
	}
}

func TestBootstrapActiveNetworksReconcileAllAfterSuccess(t *testing.T) {
	nodes := []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
	}
	handler := &fakeNodeHandler{netName: "net-ok"}
	reconcileAllCalls := 0

	c := &NodeController{
		nodeController: newNodeControllerForTest(1, &reconcileAllCalls),
		nodeLister:     newNodeLister(t, nodes...),
		handlers:       map[string]NodeHandler{handler.netName: handler},
		networkRefs:    map[string]int{handler.netName: 1},
		bootstrapped:   map[string]bool{},
		bootstrapNodes: map[string]map[string]struct{}{},
	}

	if err := c.bootstrapActiveNetworks(); err != nil {
		t.Fatalf("expected bootstrapActiveNetworks to succeed, got %v", err)
	}
	if !c.isBootstrapped(handler.netName) {
		t.Fatalf("expected network %q to be bootstrapped", handler.netName)
	}
	if !c.isBootstrapNode(handler.netName, "node-a") {
		t.Fatalf("expected node-a to be tracked as bootstrap node for network %q", handler.netName)
	}
	if reconcileAllCalls != 1 {
		t.Fatalf("expected ReconcileAll to run once, got %d", reconcileAllCalls)
	}
}

func TestUpdateNetworkIndexRefLifecycle(t *testing.T) {
	netA := newNetInfoMock("net-a")
	netB := newNetInfoMock("net-b")
	c := &NodeController{
		nadToNetwork:   map[string]string{},
		networkRefs:    map[string]int{},
		bootstrapped:   map[string]bool{},
		bootstrapNodes: map[string]map[string]struct{}{},
	}

	netName, becameActive := c.updateNetworkIndex("ns1/nad1", netA)
	if netName != "net-a" || !becameActive {
		t.Fatalf("expected nad1 add to net-a to activate network, got net=%q becameActive=%t", netName, becameActive)
	}
	if got := c.networkRefs["net-a"]; got != 1 {
		t.Fatalf("expected net-a refs=1, got %d", got)
	}
	if got := c.nadToNetwork["ns1/nad1"]; got != "net-a" {
		t.Fatalf("expected nad mapping to net-a, got %q", got)
	}

	// Same mapping should be a no-op for activation and refcount.
	netName, becameActive = c.updateNetworkIndex("ns1/nad1", netA)
	if netName != "net-a" || becameActive {
		t.Fatalf("expected nad1 same-network update to not activate, got net=%q becameActive=%t", netName, becameActive)
	}
	if got := c.networkRefs["net-a"]; got != 1 {
		t.Fatalf("expected net-a refs to remain 1, got %d", got)
	}

	// Second NAD on same network increments refcount.
	netName, becameActive = c.updateNetworkIndex("ns1/nad2", netA)
	if netName != "net-a" || becameActive {
		t.Fatalf("expected nad2 add on existing net-a to not activate, got net=%q becameActive=%t", netName, becameActive)
	}
	if got := c.networkRefs["net-a"]; got != 2 {
		t.Fatalf("expected net-a refs=2, got %d", got)
	}

	// Prepare bootstrap state to verify it is only cleared when refs hit zero.
	c.bootstrapped["net-a"] = true
	c.bootstrapNodes["net-a"] = map[string]struct{}{"node-a": {}}

	// Move nad1 from net-a to net-b: net-a decremented, net-b activated.
	netName, becameActive = c.updateNetworkIndex("ns1/nad1", netB)
	if netName != "net-b" || !becameActive {
		t.Fatalf("expected nad1 move to activate net-b, got net=%q becameActive=%t", netName, becameActive)
	}
	if got := c.networkRefs["net-a"]; got != 1 {
		t.Fatalf("expected net-a refs=1 after move, got %d", got)
	}
	if got := c.networkRefs["net-b"]; got != 1 {
		t.Fatalf("expected net-b refs=1 after move, got %d", got)
	}
	if _, ok := c.bootstrapped["net-a"]; !ok {
		t.Fatal("expected net-a bootstrap state to remain while refs are non-zero")
	}

	// Removing nad2 should drop net-a refs to zero and clear bootstrap state.
	netName, becameActive = c.updateNetworkIndex("ns1/nad2", nil)
	if netName != "" || becameActive {
		t.Fatalf("expected nad2 removal to return empty network/inactive, got net=%q becameActive=%t", netName, becameActive)
	}
	if _, ok := c.networkRefs["net-a"]; ok {
		t.Fatal("expected net-a refs to be removed at zero")
	}
	if _, ok := c.bootstrapped["net-a"]; ok {
		t.Fatal("expected net-a bootstrapped state to be cleared at zero refs")
	}
	if _, ok := c.bootstrapNodes["net-a"]; ok {
		t.Fatal("expected net-a bootstrap nodes to be cleared at zero refs")
	}

	// Removing nad1 should do the same for net-b.
	c.bootstrapped["net-b"] = true
	c.bootstrapNodes["net-b"] = map[string]struct{}{"node-b": {}}
	netName, becameActive = c.updateNetworkIndex("ns1/nad1", nil)
	if netName != "" || becameActive {
		t.Fatalf("expected nad1 removal to return empty network/inactive, got net=%q becameActive=%t", netName, becameActive)
	}
	if _, ok := c.networkRefs["net-b"]; ok {
		t.Fatal("expected net-b refs to be removed at zero")
	}
	if _, ok := c.bootstrapped["net-b"]; ok {
		t.Fatal("expected net-b bootstrapped state to be cleared at zero refs")
	}
	if _, ok := c.bootstrapNodes["net-b"]; ok {
		t.Fatal("expected net-b bootstrap nodes to be cleared at zero refs")
	}

	// Removing an unknown NAD key should be a no-op.
	netName, becameActive = c.updateNetworkIndex("ns1/unknown", nil)
	if netName != "" || becameActive {
		t.Fatalf("expected unknown NAD removal to be noop, got net=%q becameActive=%t", netName, becameActive)
	}
	if len(c.networkRefs) != 0 || len(c.nadToNetwork) != 0 {
		t.Fatalf("expected no residual state, refs=%v nadToNetwork=%v", c.networkRefs, c.nadToNetwork)
	}
}

func TestClaimBootstrap(t *testing.T) {
	c := &NodeController{
		networkRefs:    map[string]int{"net-a": 1},
		bootstrapped:   map[string]bool{},
		bootstrapping:  map[string]bool{},
		bootstrapNodes: map[string]map[string]struct{}{},
	}

	if !c.claimBootstrap("net-a") {
		t.Fatal("expected first claim to succeed")
	}
	if c.claimBootstrap("net-a") {
		t.Fatal("expected second claim to fail while claim is held")
	}

	c.releaseBootstrapClaim("net-a")
	if !c.claimBootstrap("net-a") {
		t.Fatal("expected claim to succeed after release")
	}
	c.releaseBootstrapClaim("net-a")

	c.bootstrapped["net-a"] = true
	if c.claimBootstrap("net-a") {
		t.Fatal("expected claim to fail for bootstrapped network")
	}

	delete(c.bootstrapped, "net-a")
	c.networkRefs["net-a"] = 0
	if c.claimBootstrap("net-a") {
		t.Fatal("expected claim to fail for inactive network")
	}
}

func TestRegisterNetworkControllerRetriesBootstrapAfterFailure(t *testing.T) {
	reconcileAllCalls := 0
	handler := &fakeNodeHandler{
		netName: "net-a",
		syncErr: errors.New("sync failed"),
	}
	c := &NodeController{
		name:            "topology-test",
		nodeController:  newNodeControllerForTest(1, &reconcileAllCalls),
		nodeLister:      newNodeLister(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}),
		handlers:        map[string]NodeHandler{},
		networkRefs:     map[string]int{"net-a": 1},
		bootstrapped:    map[string]bool{},
		bootstrapping:   map[string]bool{},
		bootstrapNodes:  map[string]map[string]struct{}{},
		annotationCache: newNodeAnnotationCache(),
		started:         true,
	}

	c.RegisterNetworkController(handler)
	if c.isBootstrapped("net-a") {
		t.Fatal("expected failed bootstrap attempt to leave network unbootstrapped")
	}
	if handler.syncCalls != 1 {
		t.Fatalf("expected first bootstrap attempt, got %d SyncNodes calls", handler.syncCalls)
	}
	if len(c.bootstrapping) != 0 {
		t.Fatalf("expected bootstrap claim to be released after failure, got %v", c.bootstrapping)
	}

	handler.syncErr = nil
	c.RegisterNetworkController(handler)
	if !c.isBootstrapped("net-a") {
		t.Fatal("expected retry bootstrap attempt to succeed")
	}
	if handler.syncCalls != 2 {
		t.Fatalf("expected second bootstrap attempt, got %d SyncNodes calls", handler.syncCalls)
	}
	if reconcileAllCalls != 1 {
		t.Fatalf("expected ReconcileAll once after successful bootstrap, got %d", reconcileAllCalls)
	}
	if len(c.bootstrapping) != 0 {
		t.Fatalf("expected bootstrap claim to be released after success, got %v", c.bootstrapping)
	}
}
