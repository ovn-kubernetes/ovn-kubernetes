package networkmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestNADNeedsUpdate_NotifiesReconcilersOnNoopUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	c := &nadController{name: "test-nad-controller"}
	_, err := c.RegisterNADReconciler(r)
	g.Expect(err).To(gomega.Succeed())

	oldNAD := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nad", ResourceVersion: "1"},
		Spec:       nettypes.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion":"0.3.1"}`},
	}
	newNAD := oldNAD.DeepCopy()
	newNAD.ResourceVersion = "2"

	needsUpdate := c.nadNeedsUpdate(oldNAD, newNAD)
	g.Expect(needsUpdate).To(gomega.BeFalse())

	g.Eventually(keyCh, time.Second).Should(gomega.Receive(gomega.Equal("ns/nad")))
}

func TestNADNeedsUpdate_DoesNotNotifyReconcilersOnRelevantUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	c := &nadController{name: "test-nad-controller"}
	_, err := c.RegisterNADReconciler(r)
	g.Expect(err).To(gomega.Succeed())

	oldNAD := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nad", ResourceVersion: "1"},
		Spec:       nettypes.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion":"0.3.1"}`},
	}
	newNAD := oldNAD.DeepCopy()
	newNAD.ResourceVersion = "2"
	newNAD.Spec.Config = `{"cniVersion":"0.3.1","name":"changed"}`

	needsUpdate := c.nadNeedsUpdate(oldNAD, newNAD)
	g.Expect(needsUpdate).To(gomega.BeTrue())

	g.Consistently(keyCh, 200*time.Millisecond).ShouldNot(gomega.Receive())
}

func TestSyncNAD_NotifiesReconcilers(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler-sync", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	nc := &networkController{
		networks:           map[string]util.MutableNetInfo{},
		networkControllers: map[string]*networkControllerState{},
	}
	netIDAlloc := id.NewIDAllocator("NetworkIDs", MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		networkController:  nc,
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkIDAllocator: netIDAlloc,
	}
	_, err := c.RegisterNADReconciler(r)
	g.Expect(err).To(gomega.Succeed())

	nadNS := "ns"
	nadName := "nad"
	nadKey := nadNS + "/" + nadName
	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := buildNAD(nadName, nadNS, networkAPrimary)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// The NAD has no network ID annotation so syncNAD will not ensure the network,
	// but it should still notify all reconcilers.
	g.Expect(c.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Eventually(keyCh, time.Second).Should(gomega.Receive(gomega.Equal(nadKey)))
}

type testNetworkController struct {
	util.ReconcilableNetInfo
	tcm             *testControllerManager
	handleRefChange func(node string, active bool)
}

func (tnc *testNetworkController) Start(context.Context) error {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("starting network: %s\n", testNetworkKey(tnc))
	tnc.tcm.started = append(tnc.tcm.started, testNetworkKey(tnc))
	return nil
}

func (tnc *testNetworkController) Stop() {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("stopping network: %s\n", testNetworkKey(tnc))
	tnc.tcm.stopped = append(tnc.tcm.stopped, testNetworkKey(tnc))
}

func (tnc *testNetworkController) Cleanup() error {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("cleaning up network: %s\n", testNetworkKey(tnc))
	tnc.tcm.cleaned = append(tnc.tcm.cleaned, testNetworkKey(tnc))
	return nil
}

func (tnc *testNetworkController) Reconcile(netInfo util.NetInfo) error {
	return util.ReconcileNetInfo(tnc.ReconcilableNetInfo, netInfo)
}

func (tnc *testNetworkController) HandleNetworkRefChange(node string, active bool) {
	if tnc.handleRefChange != nil {
		tnc.handleRefChange(node, active)
	}
}

// GomegaString is used to avoid printing embedded mutexes which can cause a
// race
func (tnc *testNetworkController) GomegaString() string {
	return format.Object(tnc.GetNetworkName(), 1)
}

func TestSyncNAD_ForceDeleteKeepsCacheForExistingNAD(t *testing.T) {
	g := gomega.NewWithT(t)

	key := "ns/nad"
	ns := "ns"
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nad",
			Namespace: ns,
		},
	}

	netIDAlloc := id.NewIDAllocator("NetworkIDs", MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		nads:               map[string]string{key: "netA"},
		primaryNADs:        map[string]string{ns: key},
		markedForRemoval:   map[string]time.Time{key: time.Now().Add(-time.Minute)},
		networkIDAllocator: netIDAlloc,
		networkController: &networkController{
			networks:           map[string]util.MutableNetInfo{},
			networkControllers: map[string]*networkControllerState{},
		},
	}

	g.Expect(c.syncNAD(key, nad)).To(gomega.Succeed())
	g.Expect(c.nads).To(gomega.HaveKeyWithValue(key, "netA"))
	g.Expect(c.primaryNADs).To(gomega.HaveKeyWithValue(ns, key))
	g.Expect(c.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func TestSyncNAD_ForceDeleteRemovesCacheOnActualDelete(t *testing.T) {
	g := gomega.NewWithT(t)

	key := "ns/nad"
	ns := "ns"

	netIDAlloc := id.NewIDAllocator("NetworkIDs", MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		nads:               map[string]string{key: "netA"},
		primaryNADs:        map[string]string{ns: key},
		markedForRemoval:   map[string]time.Time{key: time.Now().Add(-time.Minute)},
		networkIDAllocator: netIDAlloc,
		networkController: &networkController{
			networks:           map[string]util.MutableNetInfo{},
			networkControllers: map[string]*networkControllerState{},
		},
	}

	g.Expect(c.syncNAD(key, nil)).To(gomega.Succeed())
	g.Expect(c.nads).ToNot(gomega.HaveKey(key))
	g.Expect(c.primaryNADs).ToNot(gomega.HaveKey(ns))
	g.Expect(c.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func ptrTo[T any](v T) *T { return &v }

func testNetworkKey(nInfo util.NetInfo) string {
	return nInfo.GetNetworkName() + " " + nInfo.TopologyType()
}

func networkFromTestNetworkKey(key string) string {
	return key[:strings.LastIndex(key, " ")]
}

type fakeNADNamespaceLister struct {
	nads map[string]*nettypes.NetworkAttachmentDefinition
}

func (f *fakeNADNamespaceLister) List(_ labels.Selector) ([]*nettypes.NetworkAttachmentDefinition, error) {
	result := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range f.nads {
		result = append(result, nad)
	}
	return result, nil
}

func (f *fakeNADNamespaceLister) Get(name string) (*nettypes.NetworkAttachmentDefinition, error) {
	if nad, ok := f.nads[name]; ok {
		return nad, nil
	}
	return nil, apierrors.NewNotFound(nettypes.Resource("networkattachmentdefinition"), name)
}

type fakeNADLister struct {
	nads map[string]*nettypes.NetworkAttachmentDefinition
}

func (f *fakeNADLister) List(_ labels.Selector) ([]*nettypes.NetworkAttachmentDefinition, error) {
	result := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range f.nads {
		result = append(result, nad)
	}
	return result, nil
}

func (f *fakeNADLister) NetworkAttachmentDefinitions(_ string) nadlisters.NetworkAttachmentDefinitionNamespaceLister {
	return &fakeNADNamespaceLister{nads: f.nads}
}

type testControllerManager struct {
	sync.Mutex

	defaultNetwork *testNetworkController
	controllers    map[string]NetworkController

	started []string
	stopped []string
	cleaned []string

	raiseErrorWhenCreatingController error

	valid []util.NetInfo
}

func (tcm *testControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	tcm.Lock()
	defer tcm.Unlock()
	if tcm.raiseErrorWhenCreatingController != nil {
		return nil, tcm.raiseErrorWhenCreatingController
	}
	t := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
	}
	tcm.controllers[testNetworkKey(netInfo)] = t
	return t, nil
}

func (tcm *testControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	tcm.valid = validNetworks
	return nil
}

func (tcm *testControllerManager) GetDefaultNetworkController() ReconcilableNetworkController {
	return tcm.defaultNetwork
}

func (tcm *testControllerManager) Reconcile(string, util.NetInfo, util.NetInfo) error {
	return nil
}

func (tcm *testControllerManager) Filter(*nettypes.NetworkAttachmentDefinition) (bool, error) {
	return false, nil
}

type fakeNamespaceLister struct{}

func (f *fakeNamespaceLister) List(labels.Selector) (ret []*corev1.Namespace, err error) {
	return nil, nil
}

// Get retrieves the Namespace from the index for a given name.
// Objects returned here must be treated as read-only.
func (f *fakeNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
		},
	}, nil
}

func TestNADController(t *testing.T) {
	t.Run("filter respects node trackers", func(t *testing.T) {
		if err := config.PrepareTestConfig(); err != nil {
			t.Fatalf("prepare test config: %v", err)
		}
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		pt := &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
		}
		pt.nodeNADToPodCache["node1"] = map[string]map[string]struct{}{
			"ns1/nad1": {"pod": {}},
		}

		cm := &nadController{
			filterNADsOnNode: "node1",
			podTracker:       pt,
		}

		tests := []struct {
			name     string
			nad      *nettypes.NetworkAttachmentDefinition
			expected bool
		}{
			{
				name: "no ownerRef",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "nad1"},
				},
				expected: false,
			},
			{
				name: "unrelated ownerRef",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1", Name: "nad1",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "Deployment",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: false,
			},
			{
				name: "UDN ownerRef but unused on node -> filtered",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns2", Name: "nad2",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "UserDefinedNetwork",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: true,
			},
			{
				name: "UDN ownerRef and used on node -> not filtered",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1", Name: "nad1",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "UserDefinedNetwork",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := cm.filter(tt.nad)
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if got != tt.expected {
					t.Fatalf("expected filter=%v got %v", tt.expected, got)
				}
			})
		}
	})

	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets:       "10.1.130.0/24",
		TransitSubnet: config.ClusterManager.V4TransitSubnet,
		Role:          types.NetworkRolePrimary,
		MTU:           1400,
	}
	networkAIncompatible := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	networkASecondary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRoleSecondary,
		MTU:     1400,
	}

	networkBSecondary := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkBSecondary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	networkDefault := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	type args struct {
		nad     string
		network *ovncnitypes.NetConf
		wantErr bool
	}
	type expected struct {
		network *ovncnitypes.NetConf
		nads    []string
	}
	tests := []struct {
		name     string
		args     []args
		expected []expected
	}{
		{
			name: "NAD on default network is tracked with default controller",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkDefault,
				},
			},
			expected: []expected{
				{
					network: networkDefault,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "two NADs added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "Two Primary NADs added for same namespace",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAPrimary,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two Primary NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test2/nad_2",
					network: networkAPrimary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test2/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad: "test/nad_2",
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "NAD added then updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to same network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkBSecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "NAD added then incompatible NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
				},
			},
			expected: []expected{
				{
					network: networkAIncompatible,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "non ovn-k NAD added",
			args: []args{
				{
					nad: "test/nad_1",
					network: &ovncnitypes.NetConf{
						NetConf: cnitypes.NetConf{
							Name: "test",
							Type: "sriov",
						},
					},
					wantErr: false,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadController := &nadController{
				nads:                map[string]string{},
				primaryNADs:         map[string]string{},
				networkController:   newNetworkController("", "", "", tcm, nil),
				networkIDAllocator:  id.NewIDAllocator("NetworkIDs", MaxNetworks),
				tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
				nadClient:           fakeClient.NetworkAttchDefClient,
				nodeLister:          wf.NodeCoreInformer().Lister(),
				namespaceLister:     &fakeNamespaceLister{},
			}
			err = nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			netController := nadController.networkController

			// Drive reconciliation only for networks touched by the NAD operation
			// to avoid assertions against transient async queue states.
			syncTouchedNetworks := func(nadKey, prevNetwork string) {
				networkNames := sets.New[string]()
				if prevNetwork != "" {
					networkNames.Insert(prevNetwork)
				}
				if currNetwork := nadController.nads[nadKey]; currNetwork != "" {
					networkNames.Insert(currNetwork)
				}
				for _, network := range networkNames.UnsortedList() {
					g.Expect(netController.syncNetwork(network)).To(gomega.Succeed())
				}
			}

			for _, args := range tt.args {
				namespace, name, err := cache.SplitMetaNamespaceKey(args.nad)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				var nad *nettypes.NetworkAttachmentDefinition
				if args.network != nil {
					args.network.NADName = args.nad
					nad, err = buildNAD(name, namespace, args.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())
					_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
					g.Expect(err).To(gomega.Or(gomega.Not(gomega.HaveOccurred()), gomega.MatchError(apierrors.IsAlreadyExists, "AlreadyExists")))
				}

				prevNetwork := nadController.nads[args.nad]
				err = nadController.syncNAD(args.nad, nad)
				if args.wantErr {
					g.Expect(err).To(gomega.HaveOccurred())
				} else {
					g.Expect(err).NotTo(gomega.HaveOccurred())
				}
				syncTouchedNetworks(args.nad, prevNetwork)
			}

			meetsExpectations := func(g gomega.Gomega) {
				// test that the manager has all the desired networks
				g.Expect(netController.getAllNetworks()).To(gomega.HaveLen(len(tt.expected)))

				var expectRunning []string
				for _, expected := range tt.expected {
					netInfo, err := util.NewNetInfo(expected.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())

					name := netInfo.GetNetworkName()
					testNetworkKey := testNetworkKey(netInfo)
					func() {
						netController.Lock()
						defer netController.Unlock()
						tcm.Lock()
						defer tcm.Unlock()

						// test that the desired networks have the expected
						// config and NADs, including the default network which
						// could have had NAD/Advertisement changes as well
						g.Expect(netController.networks).To(gomega.HaveKey(name))
						g.Expect(util.AreNetworksCompatible(netController.networks[name], netInfo)).To(gomega.BeTrue(),
							fmt.Sprintf("matching network config for network %s", name))
						nadKeys := nadController.GetNADKeysForNetwork(name)
						g.Expect(nadKeys).To(gomega.ConsistOf(expected.nads),
							fmt.Sprintf("matching NADs for network %s", name))
						id, err := nadController.networkIDAllocator.AllocateID(name)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						g.Expect(netController.networks[name].GetNetworkID()).To(gomega.Equal(id))
						if netInfo.TopologyType() == types.Layer2Topology && netInfo.IsPrimaryNetwork() {
							tunnelKeys, err := nadController.tunnelKeysAllocator.AllocateKeys(name, id, 2)
							g.Expect(err).ToNot(gomega.HaveOccurred())
							g.Expect(netController.networks[name].GetTunnelKeys()).To(gomega.Equal(tunnelKeys))
						}
						// test that the actual controllers have the expected config and NADs
						if !netInfo.IsDefault() {
							g.Expect(tcm.controllers).To(gomega.HaveKey(testNetworkKey))
							g.Expect(util.AreNetworksCompatible(tcm.controllers[testNetworkKey], netInfo)).To(gomega.BeTrue(),
								fmt.Sprintf("matching network config for network %s", name))
							g.Expect(tcm.controllers[testNetworkKey].GetNetworkID()).To(gomega.Equal(id))
							expectRunning = append(expectRunning, testNetworkKey)
						}
					}()
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						key := expected.nads[0]
						namespace, _, err := cache.SplitMetaNamespaceKey(key)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						netInfoFound, err := nadController.GetActiveNetworkForNamespace(namespace)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						g.Expect(util.AreNetworksCompatible(netInfoFound, netInfo)).To(gomega.BeTrue())
						nadKeys := nadController.GetNADKeysForNetwork(netInfoFound.GetNetworkName())
						g.Expect(nadKeys).To(gomega.ConsistOf(expected.nads))
					}
				}
				tcm.Lock()
				defer tcm.Unlock()
				expectStopped := sets.New(tcm.started...).Difference(sets.New(expectRunning...)).UnsortedList()
				// test that the controllers are started, stopped and cleaned up as expected
				g.Expect(tcm.started).To(gomega.ContainElements(expectRunning), "started network controllers")
				g.Expect(tcm.stopped).To(gomega.ConsistOf(expectStopped), "stopped network controllers")
				g.Expect(tcm.cleaned).To(gomega.ConsistOf(expectStopped), "cleaned up network controllers")

				// if we reallocate all stopped networks, they should get a higher id than base if the previous id was released
				base, err := nadController.networkIDAllocator.AllocateID("test")
				g.Expect(err).ToNot(gomega.HaveOccurred())
				for _, stopped := range expectStopped {
					network := networkFromTestNetworkKey(stopped)
					if nadController.GetNetwork(network) != nil {
						// this network is still running under different config
						continue
					}
					id, err := nadController.networkIDAllocator.AllocateID(network)
					g.Expect(err).ToNot(gomega.HaveOccurred())
					g.Expect(id).To(gomega.BeNumerically(">", base), "unexpected network ID for network %s", network)
				}
			}

			meetsExpectations(g)
		})
	}
}

func TestNetworkGracePeriodCleanup(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	// Enable segmentation and grace period
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = 2 * time.Second // short grace period for test
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	fakeCtrl := &controller.FakeController{}
	nadController := &nadController{
		nads:                map[string]string{},
		primaryNADs:         map[string]string{},
		networkController:   newNetworkController("", "", "", tcm, nil),
		networkIDAllocator:  id.NewIDAllocator("NetworkIDs", MaxNetworks),
		tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
		nadClient:           fakeClient.NetworkAttchDefClient,
		namespaceLister:     &fakeNamespaceLister{},
		markedForRemoval:    map[string]time.Time{},
		controller:          fakeCtrl,
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(nadController.networkController.Start()).To(gomega.Succeed())
	defer nadController.networkController.Stop()
	// --- Step 1: Add a NAD ---
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
	}
	netConf.NADName = util.GetNADName("test", "nad1")
	nad, err := buildNADWithAnnotations("nad1", "test", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// Create the NAD in the fake client so syncNAD can find it
	_, err = fakeClient.NetworkAttchDefClient.
		K8sCniCncfIoV1().
		NetworkAttachmentDefinitions(nad.Namespace).
		Create(context.Background(), nad, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = nadController.syncNAD("test/nad1", nad)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// Should have been started
	g.Eventually(func() []string {
		tcm.Lock()
		defer tcm.Unlock()
		return append([]string(nil), tcm.started...)
	}).WithTimeout(1*time.Second).Should(gomega.ContainElement(testNetworkKey(netInfo)),
		"network should be started before we check grace period")
	fakeCtrl.Lock()
	numberOfReconciles := len(fakeCtrl.Reconciles)
	fakeCtrl.Unlock()
	// --- Step 2: Mark as inactive ---
	// This triggers the grace-period timer, not immediate deletion.
	nadController.updateNADState(util.GetNADName(nad.Namespace, nad.Name), false)
	// updateNADState() also requeues immediately; capture that baseline first.
	g.Eventually(func() int {
		fakeCtrl.Lock()
		defer fakeCtrl.Unlock()
		return len(fakeCtrl.Reconciles)
	}).WithTimeout(1 * time.Second).Should(gomega.Equal(numberOfReconciles + 1))
	reconcilesAfterImmediate := numberOfReconciles + 1
	// --- Step 3: Verify that within the grace period, cleanup has NOT happened ---
	g.Consistently(func() []string {
		tcm.Lock()
		defer tcm.Unlock()
		return append([]string(nil), tcm.cleaned...)
	}).WithTimeout(1*time.Second).Should(gomega.BeEmpty(),
		"cleanup should not happen before grace period ends")
	// --- Step 4: Verify a *second* reconcile only AFTER grace period expires ---
	g.Eventually(func() int {
		fakeCtrl.Lock()
		defer fakeCtrl.Unlock()
		return len(fakeCtrl.Reconciles)
	}).WithTimeout(5 * time.Second).Should(gomega.Equal(reconcilesAfterImmediate + 1))
}

func TestFilteredNADDeleteReleasesNetworkID(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", "", tcm, nil),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", MaxNetworks),
		filterNADsOnNode:   "node1",
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey := util.GetNADName("ns1", "nad1")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "filtered-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	controller := true
	nad.OwnerReferences = []metav1.OwnerReference{
		{
			Kind:       "UserDefinedNetwork",
			Name:       "udn1",
			Controller: &controller,
		},
	}

	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Dynamic UDN is on and node is filtered, expect ID to be reserved anyway
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netInfo.GetNetworkName())).To(gomega.Equal(2))

	// Simulate a delete on filtered network and makes sure it still releases the ID
	g.Expect(nadController.syncNAD(nadKey, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netInfo.GetNetworkName())).To(gomega.Equal(types.InvalidID))
}

func TestFilteredAndActiveNADDeleteRetainsIDUntilNoRefs(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	pt := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
	}
	pt.nodeNADToPodCache["node1"] = map[string]map[string]struct{}{
		"ns1/nad1": {"pod": {}},
	}

	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", "", tcm, nil),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", MaxNetworks),
		filterNADsOnNode:   "node1",
		podTracker:         pt,
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey1 := util.GetNADName("ns1", "nad1")
	nadKey2 := util.GetNADName("ns2", "nad2")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "shared-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey1,
	}
	netConf2 := *netConf
	netConf2.NADName = nadKey2

	nad1, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad2, err := buildNADWithAnnotations("nad2", "ns2", &netConf2, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad1.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn1",
		Controller: ptrTo(true),
	}}
	nad2.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn2",
		Controller: ptrTo(true),
	}}

	// Active NAD should render, filtered NAD should not, but both should reserve ID.
	g.Expect(nadController.syncNAD(nadKey1, nad1)).To(gomega.Succeed())
	g.Expect(nadController.syncNAD(nadKey2, nad2)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Delete active NAD; filtered NAD still references the network, so ID stays reserved.
	g.Expect(nadController.syncNAD(nadKey1, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Delete filtered NAD; now no refs remain, so ID is released.
	g.Expect(nadController.syncNAD(nadKey2, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(types.InvalidID))
}

func TestDynamicDeleteDoesNotReleaseNetworkID(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", "", tcm, nil),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", MaxNetworks),
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey := util.GetNADName("ns1", "nad1")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "dyn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}

	nad, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Initial sync reserves the ID.
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Simulate inactive transition via expired grace period.
	nadController.markedForRemoval = map[string]time.Time{nadKey: time.Now().Add(-time.Minute)}
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))
}

func TestSyncAll(t *testing.T) {
	const nodeNetworkID = 1337
	type mode string

	const (
		modeZone           mode = "zone"
		modeClusterManager mode = "clusterManager"
		modeNode           mode = "node"
	)
	network_A := &ovncnitypes.NetConf{
		Topology: types.Layer3Topology,
		NetConf: cnitypes.NetConf{
			Name: "network_A",
			Type: "ovn-k8s-cni-overlay",
		},
		Role: types.NetworkRolePrimary,
		MTU:  1400,
	}
	network_A_Copy := *network_A
	network_B := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "network_B",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	type TestNAD struct {
		name      string
		netconf   *ovncnitypes.NetConf
		networkID string
	}
	tests := []struct {
		name                 string
		testNADs             []TestNAD
		syncAllInjectedError error
		expectNADIgnored     bool
		extraNADsIgnored     bool
		node                 *corev1.Node
		mode                 mode
		expectGeneratedID    bool
		expectInheritID      bool // for Node with conflicting ID
	}{
		{
			name: "multiple networks referenced by multiple nads",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
				{
					name:    "test/nad2",
					netconf: network_B,
				},
				{
					name:    "test2/nad3",
					netconf: &network_A_Copy,
				},
			},
			syncAllInjectedError: ErrNetworkControllerTopologyNotManaged,
		},
		{
			name: "nad already annotated with network ID",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
			},
		},
		{
			name: "nad and node with no network ID should not fail to sync in zone mode and should be ignored",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: map[string]string{
						// no "OVNNodeNetworkIds" annotation or it's empty
					},
				},
			},
			mode:             modeZone,
			expectNADIgnored: true,
		},
		{
			name: "nad and node with no network ID should not fail to sync in node mode and should be ignored",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: map[string]string{
						// no "OVNNodeNetworkIds" annotation or it's empty
					},
				},
			},
			mode:             modeNode,
			expectNADIgnored: true,
		},
		{
			name: "nad and node with no network ID should not fail to sync in cluster-manager mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: map[string]string{
						// no "OVNNodeNetworkIds" annotation or it's empty
					},
				},
			},
			mode:              modeClusterManager,
			expectGeneratedID: true,
		},
		{
			name: "nad without network ID + node with network ID should not fail to sync in cluster manager mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode: modeClusterManager,
		},
		{
			name: "nad with network ID + node with network ID should preserve NAD ID on sync in cluster manager mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode: modeClusterManager,
		},
		{
			name: "nad without network ID + node with network ID should sync in zone mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"%s": "%d"}`, network_A.NetConf.Name, nodeNetworkID),
					},
				},
			},
			mode: modeZone,
		},
		{
			name: "nad without network ID + node with network ID should sync in node mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"%s": "%d"}`, network_A.NetConf.Name, nodeNetworkID),
					},
				},
			},
			mode: modeNode,
		},
		{
			name: "two NADs same network but conflicting IDs on node -> second should not sync in zone mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
				{
					name:    "test2/nad2",
					netconf: network_A,
					// no ID on this NAD, but node has conflicting ID
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode:             modeZone,
			extraNADsIgnored: true,
		},
		{
			name: "two NADs same network but conflicting IDs on node -> second should be inherit NAD ID in cm mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
				{
					name:    "test2/nad2",
					netconf: network_A,
					// no ID on this NAD, but node has conflicting ID
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode:            modeClusterManager,
			expectInheritID: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
			}
			if tt.syncAllInjectedError != nil {
				tcm.raiseErrorWhenCreatingController = tt.syncAllInjectedError
			}

			// default to cluster manager
			if len(tt.mode) == 0 {
				tt.mode = modeClusterManager
			}

			var controller Controller
			if tt.mode == modeZone {
				controller, err = NewForZone("test", tcm, wf)
			} else if tt.mode == modeNode {
				controller, err = NewForNode("test", tcm, wf)
			} else {
				controller, err = NewForCluster(
					tcm,
					wf,
					fakeClient,
					nil,
					id.NewTunnelKeyAllocator("TunnelKeys"),
				)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())

			expectedNetworks := map[string]util.NetInfo{}
			expectedPrimaryNetworks := map[string]util.NetInfo{}
			for _, namespace := range []string{"test", "test2"} {
				_, err = fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(),
					&corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name:   namespace,
							Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
						},
					}, metav1.CreateOptions{},
				)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())
			for i, testNAD := range tt.testNADs {
				if i > 0 && tt.extraNADsIgnored {
					break
				}
				namespace, name, err := cache.SplitMetaNamespaceKey(testNAD.name)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				testNAD.netconf.NADName = testNAD.name
				nadAnnotations := map[string]string{
					types.OvnNetworkNameAnnotation: testNAD.netconf.Name,
				}
				if len(testNAD.networkID) > 0 {
					nadAnnotations[types.OvnNetworkIDAnnotation] = testNAD.networkID
				}
				nad, err := buildNADWithAnnotations(name, namespace, testNAD.netconf, nadAnnotations)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
					context.Background(),
					nad,
					metav1.CreateOptions{},
				)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				netInfo := expectedNetworks[testNAD.netconf.Name]
				if netInfo == nil && !tt.expectNADIgnored {
					netInfo, err = util.NewNetInfo(testNAD.netconf)
					mutableNetInfo := util.NewMutableNetInfo(netInfo)
					if tt.expectGeneratedID {
						mutableNetInfo.SetNetworkID(1)
						netInfo = mutableNetInfo
					} else if testNAD.networkID != "" {
						id, err := strconv.Atoi(testNAD.networkID)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						mutableNetInfo.SetNetworkID(id)
						netInfo = mutableNetInfo
					} else if tt.node != nil {
						mutableNetInfo.SetNetworkID(nodeNetworkID)
						netInfo = mutableNetInfo
					}
					g.Expect(err).ToNot(gomega.HaveOccurred())
					expectedNetworks[testNAD.netconf.Name] = netInfo
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						expectedPrimaryNetworks[netInfo.GetNetworkName()] = netInfo
					}
				}
			}

			if tt.node != nil {
				_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), tt.node, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = controller.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// sync has already happened, stop
			controller.Stop()

			actualNetworks := map[string]util.NetInfo{}
			for _, network := range tcm.valid {
				actualNetworks[network.GetNetworkName()] = network
			}

			g.Expect(actualNetworks).To(gomega.HaveLen(len(expectedNetworks)))
			for name, network := range expectedNetworks {
				g.Expect(actualNetworks).To(gomega.HaveKey(name))
				g.Expect(util.AreNetworksCompatible(actualNetworks[name], network)).To(
					gomega.BeTrue(),
					"network compatibility failed\nactual=%#v\nexpected=%#v",
					actualNetworks[name],
					network,
				)
				if network.GetNetworkID() != types.InvalidID {
					g.Expect(actualNetworks[name].GetNetworkID()).To(gomega.Equal(network.GetNetworkID()))
				}
			}
			if tt.expectInheritID {
				// Only one network should exist
				g.Expect(actualNetworks).To(gomega.HaveLen(1), "network should still be created")

				info := actualNetworks["network_A"]
				g.Expect(info).ToNot(gomega.BeNil())

				// Expect ID = 1 (from first NAD), not from node
				g.Expect(info.GetNetworkID()).To(gomega.Equal(1))

				// Both NADs should now be part of the same network
				nadKeys := controller.Interface().GetNADKeysForNetwork(info.GetNetworkName())
				g.Expect(nadKeys).To(gomega.HaveLen(2))

				// NAD2 should now have the inherited ID
				nad2, _ := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().
					NetworkAttachmentDefinitions("test2").Get(context.Background(), "nad2", metav1.GetOptions{})
				g.Expect(nad2).ToNot(gomega.BeNil())
				g.Expect(nad2.Annotations[types.OvnNetworkIDAnnotation]).To(gomega.Equal("1"))

				// Skip further primary network checks for this scenario
				return
			}
			if !tt.expectNADIgnored {
				actualPrimaryNetwork, err := controller.Interface().GetActiveNetworkForNamespace("test")
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(expectedPrimaryNetworks).To(gomega.HaveKey(actualPrimaryNetwork.GetNetworkName()))
				expectedPrimaryNetwork := expectedPrimaryNetworks[actualPrimaryNetwork.GetNetworkName()]
				g.Expect(util.AreNetworksCompatible(expectedPrimaryNetwork, actualPrimaryNetwork)).To(gomega.BeTrue())
			}
		})
	}
}

func TestResourceCleanup(t *testing.T) {
	g := gomega.NewWithT(t)
	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadController := &nadController{
		nads:                map[string]string{},
		primaryNADs:         map[string]string{},
		networkController:   newNetworkController("", "", "", tcm, nil),
		networkIDAllocator:  id.NewIDAllocator("NetworkIDs", MaxNetworks),
		tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
		nadClient:           fakeClient.NetworkAttchDefClient,
		namespaceLister:     &fakeNamespaceLister{},
		nodeLister:          wf.NodeCoreInformer().Lister(),
	}
	err = nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(nadController.networkController.Start()).To(gomega.Succeed())
	defer nadController.networkController.Stop()

	nadNs := "test"
	nadName := "nad_1"
	nadKey := nadNs + "/" + nadName
	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := buildNAD(nadName, nadNs, networkAPrimary)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// make annotation update fail (nad doesn't exist), make sure networkID and tunnel keys are released
	err = nadController.syncNAD(nadKey, nad)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("failed to annotate network ID and/or tunnel keys"))
	// we know the allocated network ID was 1 and tunnelKeys were [16711684, 16715779] (first available IDs after Default network)
	// try to reserve these exact IDs for a different network to make sure they were released
	err = nadController.networkIDAllocator.ReserveID("networkB", 1)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = nadController.tunnelKeysAllocator.ReserveKeys("networkB", []int{16711684, 16715779})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

func buildNAD(name, namespace string, network *ovncnitypes.NetConf) (*nettypes.NetworkAttachmentDefinition, error) {
	config, err := json.Marshal(network)
	if err != nil {
		return nil, err
	}
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: string(config),
		},
	}
	return nad, nil
}

func buildNADWithAnnotations(name, namespace string, network *ovncnitypes.NetConf, annotations map[string]string) (*nettypes.NetworkAttachmentDefinition, error) {
	nad, err := buildNAD(name, namespace, network)
	if err != nil {
		return nil, err
	}
	nad.Annotations = annotations
	return nad, nil
}

func TestOnNetworkRefChangeNotifiesNetworkController(t *testing.T) {
	tests := []struct {
		name                 string
		notifyActive         bool
		nodeHasNetworkActive bool
	}{
		{
			name:                 "active notification with no refs",
			notifyActive:         true,
			nodeHasNetworkActive: false,
		},
		{
			name:                 "inactive notification with refs",
			notifyActive:         false,
			nodeHasNetworkActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

			// Build fake NAD with UDN owner reference and overlay topology.
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "udn-net",
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
				NADName:  "ns1/primary",
			}
			nad, err := buildNAD("primary", "ns1", netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nad.OwnerReferences = []metav1.OwnerReference{{
				Kind:       "UserDefinedNetwork",
				Name:       "udn",
				Controller: ptrTo(true),
			}}

			nadLister := &fakeNADLister{
				nads: map[string]*nettypes.NetworkAttachmentDefinition{
					"primary": nad,
				},
			}
			nodeName := "node1"

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			nc := &nadController{
				nads:                map[string]string{},
				primaryNADs:         map[string]string{},
				networkController:   newNetworkController("", "", "", tcm, nil),
				networkIDAllocator:  id.NewIDAllocator("NetworkIDs", MaxNetworks),
				tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
				nadLister:           nadLister,
			}
			err = nc.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			nadNetwork, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			networkName := nadNetwork.GetNetworkName()
			mutableNetInfo := util.NewMutableNetInfo(nadNetwork)
			mutableNetInfo.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
			nc.networkController.setNetwork(networkName, mutableNetInfo)
			nc.networkController.nodeHasNetwork = func(_, _ string) bool { return tt.nodeHasNetworkActive }
			var gotNode string
			var gotActive bool
			var callCount int
			testController := &testNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(nadNetwork),
				tcm:                 tcm,
				handleRefChange: func(node string, active bool) {
					gotNode = node
					gotActive = active
					callCount++
				},
			}
			nc.networkController.networkControllers[networkName] = &networkControllerState{
				controller: testController,
			}

			// Trigger network ref change.
			nc.OnNetworkRefChange(nodeName, util.GetNADName(nad.Namespace, nad.Name), tt.notifyActive)
			err = nc.networkController.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(callCount).To(gomega.Equal(1))
			g.Expect(gotNode).To(gomega.Equal(nodeName))
			g.Expect(gotActive).To(gomega.Equal(tt.nodeHasNetworkActive))
		})
	}
}
