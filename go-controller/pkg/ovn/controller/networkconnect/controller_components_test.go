package networkconnect

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Test for cncNeedsUpdate function
func TestCNCNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *networkconnectv1.ClusterNetworkConnect
		newObj   *networkconnectv1.ClusterNetworkConnect
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &networkconnectv1.ClusterNetworkConnect{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &networkconnectv1.ClusterNetworkConnect{},
			newObj:   nil,
			expected: true,
		},
		{
			name: "no changes",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			expected: false,
		},
		{
			name: "connectivity changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork, networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			expected: true,
		},
		{
			name: "irrelevant annotations changed - should not trigger update",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"key": "value1"},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"key": "value2"},
				},
			},
			expected: false,
		},
		{
			name: "subnet annotation changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`},
				},
			},
			expected: true,
		},
		{
			name: "tunnel key annotation changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/connect-router-tunnel-key": "12345"},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/connect-router-tunnel-key": "67890"},
				},
			},
			expected: true,
		},
		{
			name: "relevant annotations unchanged",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/network-connect-subnet":    `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`,
						"k8s.ovn.org/connect-router-tunnel-key": "12345",
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/network-connect-subnet":    `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`,
						"k8s.ovn.org/connect-router-tunnel-key": "12345",
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors changed - CUDN selector added",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{
						{
							NetworkSelectionType: types.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &types.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors changed - PUDN selector label changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{
						{
							NetworkSelectionType: types.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &types.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"env": "dev"},
								},
							},
						},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{
						{
							NetworkSelectionType: types.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &types.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"env": "prod"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors unchanged",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{
						{
							NetworkSelectionType: types.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &types.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []types.NetworkSelector{
						{
							NetworkSelectionType: types.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &types.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cncNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test for nodeNeedsUpdate function
func TestNodeNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *corev1.Node
		newObj   *corev1.Node
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &corev1.Node{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &corev1.Node{},
			newObj:   nil,
			expected: true,
		},
		{
			name: "no changes",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{},
				},
			},
			expected: false,
		},
		{
			name: "zone annotation changed",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/zone-name": "zone1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/zone-name": "zone2"},
				},
			},
			expected: true,
		},
		{
			name: "node subnet annotation changed",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"10.244.1.0/24"}`},
				},
			},
			expected: true,
		},
		{
			name: "node ID annotation changed",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "2"},
				},
			},
			expected: false,
		},
		{
			name: "node ID annotation update during add time",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "2"},
				},
			},
			expected: true,
		},
		{
			name: "irrelevant annotation changed - should not trigger update",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"some-other-annotation": "value1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"some-other-annotation": "value2"},
				},
			},
			expected: false,
		},
		{
			name: "relevant annotations unchanged",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/zone-name":    "zone1",
						"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
						"k8s.ovn.org/node-id":      "1",
					},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/zone-name":    "zone1",
						"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
						"k8s.ovn.org/node-id":      "1",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := nodeNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestController_reconcileNode tests that reconcileNode requeues all CNCs
func TestController_reconcileNode(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableNetworkConnect = true

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	// Create test CNCs
	cnc1 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
	}
	cnc2 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc2"},
	}
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc1, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc2, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create test node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"k8s.ovn.org/zone-name":    "zone1",
				"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
			},
		},
	}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(
		context.Background(), node, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Wait for informer caches to sync
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ClusterNetworkConnectInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
	)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	// Track reconciled CNCs
	reconciledCNCs := sets.New[string]()
	reconciledMutex := sync.Mutex{}

	// Create controller with listers from watch factory
	c := &Controller{
		cncLister:  wf.ClusterNetworkConnectInformer().Lister(),
		nodeLister: wf.NodeCoreInformer().Lister(),
	}

	// Create CNC controller with custom reconcile function that tracks calls
	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      wf.ClusterNetworkConnectInformer().Lister().List,
		Reconcile: func(key string) error {
			reconciledMutex.Lock()
			defer reconciledMutex.Unlock()
			reconciledCNCs.Insert(key)
			return nil
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController("test-cnc-controller", cncCfg)

	err = controllerutil.Start(c.cncController)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controllerutil.Stop(c.cncController)

	// Wait for initial sync, then clear recorded reconciliations
	g.Eventually(func() int {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.Len()
	}).Should(gomega.BeNumerically(">=", 2))
	reconciledMutex.Lock()
	reconciledCNCs = sets.New[string]()
	reconciledMutex.Unlock()

	// Call reconcileNode
	err = c.reconcileNode("node1")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify all CNCs were requeued
	g.Eventually(func() []string {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.UnsortedList()
	}).Should(gomega.ConsistOf("cnc1", "cnc2"))
}

// TestController_syncNAD tests that syncNAD requeues CNCs matching the NAD network ID.
func TestController_syncNAD(t *testing.T) {
	g := gomega.NewWithT(t)
	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableNetworkConnect = true

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	networkID := 7
	ownerKey := fmt.Sprintf("layer3_%d", networkID)
	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc1",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": fmt.Sprintf("{\"%s\":{\"ipv4\":\"192.168.0.0/24\"}}", ownerKey),
			},
		},
	}
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nadConfig := `{"cniVersion":"0.4.0","name":"net1","type":"ovn-k8s-cni-overlay","topology":"layer3","role":"primary","netAttachDefName":"ns1/nad1"}`
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "nad1",
			Annotations: map[string]string{
				"k8s.ovn.org/network-id": strconv.Itoa(networkID),
			},
		},
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: nadConfig,
		},
	}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(
		context.Background(), nad, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nadKey := util.GetNADName(nad.Namespace, nad.Name)
	netInfo, err := util.ParseNADInfo(nad)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(nadKey)
	mutableNetInfo.SetNetworkID(networkID)

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Wait for informer caches to sync
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ClusterNetworkConnectInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
	)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	// Track reconciled CNCs
	reconciledCNCs := sets.New[string]()
	reconciledMutex := sync.Mutex{}

	// Create controller with listers from watch factory
	c := &Controller{
		cncLister: wf.ClusterNetworkConnectInformer().Lister(),
		nadLister: wf.NADInformer().Lister(),
		networkManager: &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{
				nadKey: mutableNetInfo,
			},
		},
	}

	// Create CNC controller with custom reconcile function that tracks calls
	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      wf.ClusterNetworkConnectInformer().Lister().List,
		Reconcile: func(key string) error {
			reconciledMutex.Lock()
			defer reconciledMutex.Unlock()
			reconciledCNCs.Insert(key)
			return nil
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController("test-cnc-controller", cncCfg)

	err = controllerutil.Start(c.cncController)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controllerutil.Stop(c.cncController)

	// Wait for initial sync, then clear recorded reconciliations
	g.Eventually(func() int {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.Len()
	}).Should(gomega.BeNumerically(">=", 1))
	reconciledMutex.Lock()
	reconciledCNCs = sets.New[string]()
	reconciledMutex.Unlock()

	err = c.syncNAD("ns1/nad1")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify CNC was requeued
	g.Eventually(func() []string {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.UnsortedList()
	}).Should(gomega.ConsistOf("cnc1"))
}

// Test for serviceNeedsUpdate function
func TestServiceNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *corev1.Service
		newObj   *corev1.Service
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &corev1.Service{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &corev1.Service{},
			newObj:   nil,
			expected: false,
		},
		{
			name:     "both nil",
			oldObj:   nil,
			newObj:   nil,
			expected: false,
		},
		{
			name: "update event - should not trigger",
			oldObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
			},
			newObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
					Labels:    map[string]string{"app": "test"},
				},
			},
			expected: false,
		},
		{
			name: "spec change - should not trigger",
			oldObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
				},
			},
			newObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.2",
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := serviceNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test for mustProcessCNCForService function
func TestMustProcessCNCForService(t *testing.T) {
	tests := []struct {
		name            string
		cncCache        map[string]*networkConnectState
		cnc             *networkconnectv1.ClusterNetworkConnect
		svc             *corev1.Service
		networkOwnerKey string
		expected        bool
	}{
		{
			name:     "CNC without ClusterIPServiceNetwork - should return false",
			cncCache: map[string]*networkConnectState{},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name:     "CNC with ClusterIPServiceNetwork but not in cache - should return false",
			cncCache: map[string]*networkConnectState{},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "CNC in cache but network not connected - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer3_2", "layer3_3"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "CNC in cache and network is connected - should return true",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer3_1", "layer3_2"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        true,
		},
		{
			name: "CNC with multiple connectivity types including ClusterIPServiceNetwork - should return true",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer2_5"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{
						networkconnectv1.PodNetwork,
						networkconnectv1.ClusterIPServiceNetwork,
					},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "ns1"},
			},
			networkOwnerKey: "layer2_5",
			expected:        true,
		},
		{
			name: "Different CNC in cache - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc2": {
					name:              "cnc2",
					tunnelID:          67890,
					connectedNetworks: sets.New("layer3_1"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "Empty connected networks set - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New[string](),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{
				cncCache: tt.cncCache,
			}
			result := c.mustProcessCNCForService(tt.svc, tt.cnc, tt.networkOwnerKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestController_reconcileService tests that reconcileService requeues matching CNCs
func TestController_reconcileService(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableNetworkConnect = true

	// Create NetInfo for the UDN network (layer3, ID=1)
	netInfo, err := createNetInfo(testNetwork{
		name:         "udn1",
		id:           1,
		topologyType: ovntypes.Layer3Topology,
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	// Create test CNCs - cnc1 has ClusterIPServiceNetwork and connects layer3_1
	// cnc2 has ClusterIPServiceNetwork but connects different network
	// cnc3 has only PodNetwork
	cnc1 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
		},
	}
	cnc2 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc2"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ClusterIPServiceNetwork},
		},
	}
	cnc3 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc3"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
		},
	}
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc1, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc2, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc3, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create test service in namespace "ns1"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc1",
			Namespace: "ns1",
		},
	}
	_, err = fakeClientset.KubeClient.CoreV1().Services("ns1").Create(
		context.Background(), svc, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Wait for informer caches to sync
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ClusterNetworkConnectInformer().Informer().HasSynced,
		wf.ServiceCoreInformer().Informer().HasSynced,
	)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	// Track reconciled CNCs
	reconciledCNCs := sets.New[string]()
	reconciledMutex := sync.Mutex{}

	// Create FakeNetworkManager - namespace "ns1" uses UDN network
	fakeNetworkManager := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			"ns1": netInfo, // ns1 uses the UDN network (layer3_1)
		},
	}

	// Create controller with listers and network manager
	c := &Controller{
		cncLister:      wf.ClusterNetworkConnectInformer().Lister(),
		serviceLister:  wf.ServiceCoreInformer().Lister(),
		networkManager: fakeNetworkManager,
		cncCache: map[string]*networkConnectState{
			"cnc1": {
				name:              "cnc1",
				tunnelID:          12345,
				connectedNetworks: sets.New("layer3_1", "layer3_2"), // connects layer3_1
			},
			"cnc2": {
				name:              "cnc2",
				tunnelID:          67890,
				connectedNetworks: sets.New("layer3_3", "layer3_4"), // doesn't connect layer3_1
			},
			"cnc3": {
				name:              "cnc3",
				tunnelID:          11111,
				connectedNetworks: sets.New("layer3_1"), // connects layer3_1 but no ClusterIPServiceNetwork
			},
		},
	}

	// Create CNC controller with custom reconcile function that tracks calls
	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      wf.ClusterNetworkConnectInformer().Lister().List,
		Reconcile: func(key string) error {
			reconciledMutex.Lock()
			defer reconciledMutex.Unlock()
			reconciledCNCs.Insert(key)
			return nil
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController("test-cnc-controller", cncCfg)

	err = controllerutil.Start(c.cncController)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controllerutil.Stop(c.cncController)

	// Wait for initial sync, then clear recorded reconciliations
	g.Eventually(func() int {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.Len()
	}).Should(gomega.BeNumerically(">=", 3))
	reconciledMutex.Lock()
	reconciledCNCs = sets.New[string]()
	reconciledMutex.Unlock()

	// Call reconcileService
	err = c.reconcileService("ns1/svc1")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify only cnc1 was requeued (has ClusterIPServiceNetwork AND connects layer3_1)
	// cnc2 doesn't connect layer3_1, cnc3 has only PodNetwork
	g.Eventually(func() []string {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.UnsortedList()
	}).Should(gomega.ConsistOf("cnc1"))
}
