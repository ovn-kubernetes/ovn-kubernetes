package controllermanager

import (
	"testing"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type fakeTracker struct {
	active map[string]bool
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{active: make(map[string]bool)}
}

func (f *fakeTracker) NodeHasNAD(node, nad string) bool {
	return f.active[node+"/"+nad]
}

func (f *fakeTracker) setActive(node, nad string, hasPods bool) {
	f.active[node+"/"+nad] = hasPods
}

func (f *fakeTracker) Start() error {
	return nil
}

func (f *fakeTracker) Stop() {}

func newTestNAD(ns, name, ownerKind string) *nettypes.NetworkAttachmentDefinition {
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
	}
	if ownerKind != "" {
		nad.OwnerReferences = []metav1.OwnerReference{{
			Kind:       ownerKind,
			Controller: ptr.To(true),
		}}
	}
	return nad
}

func TestControllerManager_FilterAndOnNetworkRefChange(t *testing.T) {
	// Setup config zone
	util.PrepareTestConfig()
	config.Default.Zone = "test-node"
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	// Fake NM + podTracker
	fakeNM := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{},
		Reconciled:      make([]string, 0),
	}

	pt := newFakeTracker()
	pt.setActive("test-node", "ns1/nad1", true)
	et := newFakeTracker()
	et.setActive("test-egress-node", "ns3/nad3", true)

	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
	if err != nil {
		t.Fatal(err)
	}

	err = wf.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Shutdown()

	cm := &ControllerManager{
		networkManager:  fakeNM,
		podTracker:      pt,
		egressIPTracker: et,
		watchFactory:    wf,
	}

	tests := []struct {
		name           string
		nad            *nettypes.NetworkAttachmentDefinition
		expectFiltered bool
	}{
		{
			name:           "NAD without ownerRef is not filtered",
			nad:            newTestNAD("ns1", "nad1", ""),
			expectFiltered: false,
		},
		{
			name:           "NAD with unrelated ownerRef is filtered",
			nad:            newTestNAD("ns1", "nad1", "Deployment"),
			expectFiltered: false,
		},
		{
			name:           "NAD with UDN ownerRef but no pod using it is filtered",
			nad:            newTestNAD("ns2", "nad2", "UserDefinedNetwork"),
			expectFiltered: true,
		},
		{
			name:           "NAD with UDN ownerRef and pod using it is NOT filtered",
			nad:            newTestNAD("ns1", "nad1", "UserDefinedNetwork"),
			expectFiltered: false,
		},
		{
			name:           "NAD with UDN ownerRef and egress IP using it is NOT filtered",
			nad:            newTestNAD("ns1", "nad1", "UserDefinedNetwork"),
			expectFiltered: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shouldFilter, err := cm.Filter(tt.nad)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if shouldFilter != tt.expectFiltered {
				t.Errorf("expected filter=%v, got %v", tt.expectFiltered, shouldFilter)
			}
		})
	}

	// Exercise OnNetworkRefChange
	cm.OnNetworkRefChange("test-node", "ns1/nad1", true)
	if len(fakeNM.Reconciled) != 1 || fakeNM.Reconciled[0] != "ns1/nad1" {
		t.Errorf("expected reconcile on ns1/nad1, got %+v", fakeNM.Reconciled)
	}
}
