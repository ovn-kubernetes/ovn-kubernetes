package routeadvertisements

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	ctesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	nmtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type testRA struct {
	Name                     string
	TargetVRF                string
	NetworkSelector          map[string]string
	NodeSelector             map[string]string
	FRRConfigurationSelector map[string]string
	SelectsDefault           bool
	AdvertisePods            bool
	AdvertiseEgressIPs       bool
	Status                   *metav1.ConditionStatus
}

func (tra testRA) RouteAdvertisements() *ratypes.RouteAdvertisements {
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: tra.Name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:                tra.TargetVRF,
			Advertisements:           []ratypes.AdvertisementType{},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: metav1.LabelSelector{},
		},
	}
	if tra.AdvertisePods {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.PodNetwork)
	}
	if tra.AdvertiseEgressIPs {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.EgressIP)
	}
	if tra.NetworkSelector != nil {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
			ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
				NetworkSelector: metav1.LabelSelector{
					MatchLabels: tra.NetworkSelector,
				},
			},
		})
	}
	if tra.SelectsDefault {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.DefaultNetwork,
		})
	}
	if tra.NodeSelector != nil {
		ra.Spec.NodeSelector = metav1.LabelSelector{
			MatchLabels: tra.NodeSelector,
		}
	}
	if tra.FRRConfigurationSelector != nil {
		ra.Spec.FRRConfigurationSelector = metav1.LabelSelector{
			MatchLabels: tra.FRRConfigurationSelector,
		}
	}
	if tra.Status != nil {
		ra.Status.Conditions = []metav1.Condition{{Type: "Accepted", Status: *tra.Status}}
	}
	return ra
}

var (
	nodePrimaryAddr = map[string]string{
		"node": "1.0.1.100/24",
	}
	nodePrimaryAddrIPv6 = map[string]string{
		"node": "fd03::ffff:0100:0050/64",
	}
)

type testNamespace struct {
	Name   string
	Labels map[string]string
}

func (tn testNamespace) Namespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   tn.Name,
			Labels: tn.Labels,
		},
	}
}

type testNode struct {
	Name                     string
	Generation               int
	Labels                   map[string]string
	PrimaryAddressAnnotation string
	SubnetsAnnotation        string
}

func (tn testNode) Node() *corev1.Node {
	primaryAddressAnnotation := tn.PrimaryAddressAnnotation
	if primaryAddressAnnotation == "" {
		primaryAddressAnnotation = "{\"ipv4\":\"" + nodePrimaryAddr[tn.Name] + "\", \"ipv6\":\"" + nodePrimaryAddrIPv6[tn.Name] + "\"}"
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:       tn.Name,
			Labels:     tn.Labels,
			Generation: int64(tn.Generation),
			Annotations: map[string]string{
				"k8s.ovn.org/node-subnets": tn.SubnetsAnnotation,
				util.OvnNodeIfAddr:         primaryAddressAnnotation,
			},
		},
	}
}

type testNeighbor struct {
	ASN       uint32
	Address   string
	DisableMP *bool
	Advertise []string
}

func (tn testNeighbor) Neighbor() frrapi.Neighbor {
	n := frrapi.Neighbor{
		ASN:       tn.ASN,
		Address:   tn.Address,
		DisableMP: true,
		ToAdvertise: frrapi.Advertise{
			Allowed: frrapi.AllowedOutPrefixes{
				Mode:     frrapi.AllowRestricted,
				Prefixes: tn.Advertise,
			},
		},
	}
	if tn.DisableMP != nil {
		n.DisableMP = *tn.DisableMP
	}

	return n
}

type testRouter struct {
	ASN       uint32
	VRF       string
	Prefixes  []string
	Neighbors []*testNeighbor
	Imports   []string
}

func (tr testRouter) Router() frrapi.Router {
	r := frrapi.Router{
		ASN:      tr.ASN,
		VRF:      tr.VRF,
		Prefixes: tr.Prefixes,
	}
	for _, n := range tr.Neighbors {
		r.Neighbors = append(r.Neighbors, n.Neighbor())
	}
	for _, vrf := range tr.Imports {
		r.Imports = append(r.Imports, frrapi.Import{VRF: vrf})
	}
	return r
}

type testFRRConfig struct {
	Name         string
	Namespace    string
	Generation   int
	Labels       map[string]string
	Annotations  map[string]string
	Routers      []*testRouter
	NodeSelector map[string]string
	OwnUpdate    bool
}

func (tf testFRRConfig) FRRConfiguration() *frrapi.FRRConfiguration {
	f := &frrapi.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tf.Name,
			Namespace:   tf.Namespace,
			Labels:      tf.Labels,
			Annotations: tf.Annotations,
			Generation:  int64(tf.Generation),
		},
		Spec: frrapi.FRRConfigurationSpec{
			NodeSelector: metav1.LabelSelector{
				MatchLabels: tf.NodeSelector,
			},
		},
	}
	for _, r := range tf.Routers {
		f.Spec.BGP.Routers = append(f.Spec.BGP.Routers, r.Router())
	}
	if tf.OwnUpdate {
		f.ManagedFields = append(f.ManagedFields, metav1.ManagedFieldsEntry{
			Manager: fieldManager,
			Time:    &metav1.Time{Time: time.Now()},
		})
	}
	return f
}

type testEIP struct {
	Name              string
	Generation        int
	NamespaceSelector map[string]string
	EIPs              map[string]string
}

func (te testEIP) EgressIP() *eiptypes.EgressIP {
	eip := eiptypes.EgressIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:       te.Name,
			Generation: int64(te.Generation),
		},
		Spec: eiptypes.EgressIPSpec{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: te.NamespaceSelector,
			},
		},
		Status: eiptypes.EgressIPStatus{
			Items: []eiptypes.EgressIPStatusItem{},
		},
	}
	for node, ip := range te.EIPs {
		eip.Status.Items = append(eip.Status.Items, eiptypes.EgressIPStatusItem{Node: node, EgressIP: ip})
	}
	return &eip
}

type testNAD struct {
	Name        string
	Namespace   string
	Network     string
	Subnet      string
	Labels      map[string]string
	Annotations map[string]string
	IsSecondary bool
	Topology    string
	OwnUpdate   bool
}

func (tn testNAD) NAD() *nadtypes.NetworkAttachmentDefinition {
	if tn.Annotations == nil {
		tn.Annotations = map[string]string{}
	}
	tn.Annotations[types.OvnNetworkNameAnnotation] = tn.Network
	nad := &nadtypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tn.Name,
			Namespace:   tn.Namespace,
			Labels:      tn.Labels,
			Annotations: tn.Annotations,
		},
	}
	if strings.HasPrefix(tn.Network, types.CUDNPrefix) {
		ownerRef := *metav1.NewControllerRef(
			&metav1.ObjectMeta{Name: tn.Network},
			userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"),
		)
		nad.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ownerRef}
	}
	topology := tn.Topology
	switch {
	case tn.IsSecondary:
		nad.Spec.Config = fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"%s\", \"type\": \"%s\", \"topology\": \"%s\", \"netAttachDefName\": \"%s\", \"subnets\": \"%s\"}",
			tn.Network,
			config.CNI.Plugin,
			topology,
			tn.Namespace+"/"+tn.Name,
			tn.Subnet,
		)
	case tn.Topology != "":
		nad.Spec.Config = fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"%s\", \"type\": \"%s\", \"topology\": \"%s\", \"netAttachDefName\": \"%s\", \"role\": \"primary\", \"subnets\": \"%s\"}",
			tn.Network,
			config.CNI.Plugin,
			topology,
			tn.Namespace+"/"+tn.Name,
			tn.Subnet,
		)
	default:
		nad.Spec.Config = fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"%s\", \"type\": \"%s\"}", tn.Network, config.CNI.Plugin)
	}
	if tn.OwnUpdate {
		nad.ManagedFields = append(nad.ManagedFields, metav1.ManagedFieldsEntry{
			Manager: fieldManager,
			Time:    &metav1.Time{Time: time.Now()},
		})
	}
	return nad
}

type Fake interface {
	PrependReactor(verb, resource string, reaction ctesting.ReactionFunc)
}

var count = uint32(0)

// source
// https://stackoverflow.com/questions/68794562/kubernetes-fake-client-doesnt-handle-generatename-in-objectmeta/68794563#68794563
func addGenerateNameReactor[T Fake](client any) {
	fake := client.(Fake)
	fake.PrependReactor(
		"create",
		"*",
		func(action ctesting.Action) (handled bool, ret runtime.Object, err error) {
			ret = action.(ctesting.CreateAction).GetObject()
			meta, ok := ret.(metav1.Object)
			if !ok {
				return
			}

			if meta.GetName() == "" && meta.GetGenerateName() != "" {
				meta.SetName(meta.GetGenerateName() + fmt.Sprintf("%d", atomic.AddUint32(&count, 1)))
			}

			return
		},
	)
}

func init() {
	// set this once at the beginning to avoid races that happen because we
	// cannot stop the NAD informer properly (the api we use was generated with
	// an old codegen and the informer has no shutdown method)
	config.IPv4Mode = true
}

func TestController_reconcile(t *testing.T) {
	frrNamespace := "frrNamespace"
	tests := []struct {
		name                 string
		ra                   *testRA
		otherRAs             []*testRA
		frrConfigs           []*testFRRConfig
		nads                 []*testNAD
		nodes                []*testNode
		namespaces           []*testNamespace
		eips                 []*testEIP
		reconcile            string
		wantErr              bool
		expectAcceptedStatus metav1.ConditionStatus
		expectAcceptedMsg    string
		expectFRRConfigs     []*testFRRConfig
		expectNADAnnotations map[string]map[string]string
	}{
		{
			name: "reconciles pod+eip RouteAdvertisement for a single FRR config, node and default network and target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles dual-stack pod+eip RouteAdvertisement for a single FRR config, node and default network and target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
							{ASN: 1, Address: "fd02::ffff:100:64"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}"}},
			eips:                 []*testEIP{{Name: "eipv4", EIPs: map[string]string{"node": "1.0.1.1"}}, {Name: "eipv6", EIPs: map[string]string{"node": "fd03::ffff:100:101"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24", "fd01::/64", "fd03::ffff:100:101/128"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
							{ASN: 1, Address: "fd02::ffff:100:64", Advertise: []string{"fd01::/64", "fd03::ffff:100:101/128"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles pod RouteAdvertisement for a single FRR config, node, non default networks and default target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.1.0/24"}},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2", Subnet: "1.5.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_red\":\"1.2.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.2.0.0/24", "1.3.0.0/24", "1.4.0.0/16", "1.5.0.0/16"}, Imports: []string{"black", "blue", "green", "red"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.0.0/24", "1.3.0.0/24", "1.4.0.0/16", "1.5.0.0/16"}},
						}},
						{ASN: 1, VRF: "black", Imports: []string{"default"}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
						{ASN: 1, VRF: "green", Imports: []string{"default"}},
						{ASN: 1, VRF: "red", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "black": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "(layer3) reconciles eip RouteAdvertisement for a single FRR config, node, non default network and non default target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default"},
				{Name: "red", Namespace: "red", Network: "cluster_udn_red", Topology: "layer3", Subnet: "1.2.0.0/16"},
				{Name: "blue", Namespace: "blue", Network: "cluster_udn_blue", Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_red\":\"1.2.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
			},
			namespaces: []*testNamespace{
				{Name: "default", Labels: map[string]string{"selected": "default"}},
				{Name: "red", Labels: map[string]string{"selected": "red"}},
				{Name: "blue", Labels: map[string]string{"selected": "blue"}},
			},
			eips: []*testEIP{
				{Name: "eip1", EIPs: map[string]string{"node": "172.100.0.16"}, NamespaceSelector: map[string]string{"selected": "blue"}}, // secondary interface EIP also advertised
				{Name: "eip2", EIPs: map[string]string{"node": "1.0.1.2"}, NamespaceSelector: map[string]string{"selected": "red"}},       // namespace served by unselected network, ignored
				{Name: "eip3", EIPs: map[string]string{"node": "1.0.1.3"}, NamespaceSelector: map[string]string{"selected": "blue"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.1.3/32", "172.100.0.16/32"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.3/32", "172.100.0.16/32"}},
						}, Imports: []string{"blue"}},
						{ASN: 1, VRF: "blue", Imports: []string{"red"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "(layer2) fails to reconcile eip RouteAdvertisement for a single FRR config, node, non default networks and non default target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "green", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2", Subnet: "1.5.0.0/16"},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\""},
			},
			namespaces: []*testNamespace{
				{Name: "default", Labels: map[string]string{"selected": "default"}},
				{Name: "green", Labels: map[string]string{"selected": "green"}},
				{Name: "black", Labels: map[string]string{"selected": "black"}},
			},
			eips: []*testEIP{
				{Name: "eip1", EIPs: map[string]string{"node": "172.100.0.17"}, NamespaceSelector: map[string]string{"selected": "green"}}, // secondary interface EIP also advertised
				{Name: "eip2", EIPs: map[string]string{"node": "1.0.1.4"}, NamespaceSelector: map[string]string{"selected": "black"}},      // namespace served by unselected network, ignored
				{Name: "eip3", EIPs: map[string]string{"node": "1.0.1.5"}, NamespaceSelector: map[string]string{"selected": "green"}},
			},
			reconcile: "ra",
			// EgressIP advertisements for Layer2 UDNs is not supported yet.
			expectAcceptedStatus: metav1.ConditionFalse,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"green": {}},
		},
		{
			name: "reconciles a RouteAdvertisement updating the generated FRRConfigurations if needed",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"2.0.1.1", "2.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles a deleted RouteAdvertisement",
			frrConfigs: []*testFRRConfig{
				{
					Name:         "generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/default/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			reconcile: "ra",
		},
		{
			name: "successfully reconciles RouteAdvertisements with auto targetVRF and overlapping subnets across different networks",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "net1", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "net2", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"30.100.0.0/24\", \"cluster_udn_net2\":\"30.100.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "net1", Prefixes: []string{"30.100.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.100.0.0/24"}},
						}},
						{ASN: 1, VRF: "net2", Prefixes: []string{"30.100.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.100.0.0/24"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"net1": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"net2": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "successfully reconciles RouteAdvertisements with overlapping subnets in different VRFs",
			ra:   &testRA{Name: "ra2", TargetVRF: "blue", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "blue"}},
			otherRAs: []*testRA{
				{Name: "ra1", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "red"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.1.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net-red", Namespace: "ns-red", Network: util.GenerateCUDNNetworkName("net-red"), Topology: "layer3", Subnet: "30.1.0.0/16", Labels: map[string]string{"selected": "red"}},
				{Name: "net-blue", Namespace: "ns-blue", Network: util.GenerateCUDNNetworkName("net-blue"), Topology: "layer3", Subnet: "30.1.0.0/16", Labels: map[string]string{"selected": "blue"}}, // different VRF, should not conflict
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net-red\":\"30.1.0.0/24\", \"cluster_udn_net-blue\":\"30.1.0.0/24\"}"}},
			reconcile:            "ra2",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra2"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra2/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"30.1.0.0/24"}, Imports: []string{"net-blue"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.1.0.0/24"}},
						}},
						{ASN: 1, VRF: "net-blue", Imports: []string{"blue"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"net-blue": {types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
		},
		{
			name: "successfully reconciles RouteAdvertisements with auto targetVRF and overlapping subnets across different RAs",
			ra:   &testRA{Name: "ra1", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"ra1": "true"}},
			otherRAs: []*testRA{
				{Name: "ra2", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"ra2": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "net1", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "net2", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"ra1": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "20.100.50.0/24", Labels: map[string]string{"ra2": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"20.100.0.0/24\", \"cluster_udn_net2\":\"20.100.50.0/24\"}"}},
			reconcile:            "ra1",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra1"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra1/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "net1", Prefixes: []string{"20.100.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"20.100.0.0/24"}},
						}},
					},
				},
			},
		},
		{
			name: "succeed to reconcile RouteAdvertisement while unrelated RouteAdvertisements with subnet overlaps exist",
			ra:   &testRA{Name: "ra3", AdvertisePods: true, NetworkSelector: map[string]string{"ra3": "true"}},
			otherRAs: []*testRA{
				{Name: "ra1", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
				{Name: "ra2", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:         "frrConfig",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra3"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra3/frrConfig-node/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"selected": "true"}}, // overlaps with net1 on default VRF
				{Name: "net3", Namespace: "ns3", Network: util.GenerateCUDNNetworkName("net3"), Topology: "layer3", Subnet: "20.200.0.0/16", Labels: map[string]string{"ra3": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"20.100.0.0/24\", \"cluster_udn_net2\":\"20.100.0.0/24\", \"cluster_udn_net3\":\"20.200.0.0/24\"}"}},
			reconcile:            "ra3",
			expectAcceptedStatus: metav1.ConditionTrue,
		},
		{
			name: "reconciles a RouteAdvertisement for multiple selected FRR configs, nodes and networks on auto target VRF",
			ra: &testRA{
				Name:                     "ra",
				AdvertisePods:            true,
				TargetVRF:                "auto",
				FRRConfigurationSelector: map[string]string{"selected": "true"},
				NetworkSelector:          map[string]string{"selected": "true"},
				SelectsDefault:           true,
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3"}, // not selected
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2"}, // not selected
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:         "frrConfig-node1",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node1"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.2.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "frrConfig-node2",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node2"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "another-frrConfig-node2",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node2"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.2.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{ // not selected
					Name:      "another-frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 3, VRF: "blue", Prefixes: []string{"3.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 3, Address: "3.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{Name: "node1", Labels: map[string]string{"selected": "true", "node": "node1"}, SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_red\":\"1.2.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
				{Name: "node2", Labels: map[string]string{"selected": "true", "node": "node2"}, SubnetsAnnotation: "{\"default\":\"1.1.2.0/24\", \"cluster_udn_red\":\"1.2.2.0/24\", \"cluster_udn_blue\":\"1.3.2.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-node1/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.1.0/24"}},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.2.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.1.0/24"}},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-node2/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.2.0/24"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/another-frrConfig-node2/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.2.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.2.0/24"}},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles RouteAdvertisements status even when no other updates are required",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true, Status: ptr.To(metav1.ConditionFalse)},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
		},
		{
			name: "fails to reconcile a secondary network",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", IsSecondary: true, Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile an non-cluster UDN",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name:                 "fails to reconcile pod network if node selector is not empty",
			ra:                   &testRA{Name: "ra", AdvertisePods: true, NodeSelector: map[string]string{"selected": "true"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if no FRRConfiguration is selected for selected node",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NodeSelector: map[string]string{"selected-by": "RouteAdvertisements"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:         "frrConfig",
					Namespace:    frrNamespace,
					NodeSelector: map[string]string{"selected-by": "FRRConfiguration"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", Labels: map[string]string{"selected-by": "FRRConfiguration"}},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", Labels: map[string]string{"selected-by": "RouteAdvertisements"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile when subnet annotation is missing from node",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile when subnet annotation is missing for network",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"red\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if a selectd FRRConfiguration has no matching VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if not all VRFs were matched with 'auto' target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "cluster_udn_red", Topology: "layer3", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: "cluster_udn_blue", Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if EgressIP is advertised with 'auto' target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", Topology: "layer3", Labels: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"red\":\"1.1.0.0/24\""}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if DisableMP is unset",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", DisableMP: ptr.To(false)},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile RouteAdvertisements with fully overlapping subnets between different networks within same RA",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"30.100.0.0/24\", \"cluster_udn_net2\":\"30.100.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectAcceptedMsg:    "configuration error: overlapping CIDR detected within RouteAdvertisement \"ra\" in VRF \"\": [30.100.0.0/16 30.100.0.0/16]",
		},
		{
			name: "fails to reconcile RouteAdvertisements with partial subnet overlap between different networks within same RA",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "10.1.0.0/18", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"10.1.0.0/24\", \"cluster_udn_net2\":\"10.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectAcceptedMsg:    "configuration error: overlapping CIDR detected within RouteAdvertisement \"ra\" in VRF \"\": [10.1.0.0/16 10.1.0.0/18]",
		},
		{
			name: "successfully reconciles RouteAdvertisements with same network and identical subnets",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("shared-network"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("shared-network"), Topology: "layer3", Subnet: "30.100.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_shared-network\":\"30.100.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"30.100.0.0/24"}, Imports: []string{"shared-network"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.100.0.0/24"}},
						}},
						{ASN: 1, VRF: "shared-network", Imports: []string{"default"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"net1": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"net2": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "fails to reconcile RouteAdvertisements with overlapping subnets between different networks across different RAs (reconciling ra1)",
			ra:   &testRA{Name: "ra1", AdvertisePods: true, NetworkSelector: map[string]string{"ra1": "true"}},
			otherRAs: []*testRA{
				{Name: "ra2", AdvertisePods: true, NetworkSelector: map[string]string{"ra2": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"ra1": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"ra2": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"20.100.0.0/24\", \"cluster_udn_net2\":\"20.100.0.0/24\"}"}},
			reconcile:            "ra1",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectAcceptedMsg:    "configuration error: overlapping CIDR detected between RouteAdvertisements \"ra1\" and \"ra2\" in VRF \"\": [20.100.0.0/16 20.100.0.0/16]",
		},
		{
			name: "fails to reconcile RouteAdvertisements with overlapping subnets between different networks across different RAs (auto and fixed targetVRF)",
			ra:   &testRA{Name: "ra1", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			otherRAs: []*testRA{
				{Name: "ra2", TargetVRF: "net2", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "net1", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "net2", Prefixes: []string{"1.1.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "net1", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("net1"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "net2", Namespace: "ns2", Network: util.GenerateCUDNNetworkName("net2"), Topology: "layer3", Subnet: "20.100.0.0/16", Labels: map[string]string{"selected": "true"}}, // overlaps with net1 on net2 VRF
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_net1\":\"20.100.0.0/24\", \"cluster_udn_net2\":\"20.100.0.0/24\"}"}},
			reconcile:            "ra1",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectAcceptedMsg:    "configuration error: overlapping CIDR detected between RouteAdvertisements \"ra1\" and \"ra2\" in VRF \"net2\": [20.100.0.0/16 20.100.0.0/16]",
		},
		{
			name: "successfully reconciles two RouteAdvertisements selecting the same network and VRF",
			ra:   &testRA{Name: "ra1", AdvertisePods: true, NetworkSelector: map[string]string{"shared": "true"}},
			otherRAs: []*testRA{
				{Name: "ra2", AdvertisePods: true, NetworkSelector: map[string]string{"shared": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "shared-net", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("shared-net"), Topology: "layer3", Subnet: "30.1.0.0/16", Labels: map[string]string{"shared": "true"}}, // same network selected by both RAs
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_shared-net\":\"30.1.0.0/24\"}"}},
			reconcile:            "ra1",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra1"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra1/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"30.1.0.0/24"}, Imports: []string{"shared-net"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.1.0.0/24"}},
						}},
						{ASN: 1, VRF: "shared-net", Imports: []string{"default"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"shared-net": {types.OvnRouteAdvertisementsKey: "[\"ra1\"]"}},
		},
		{
			name: "successfully reconciles different RouteAdvertisements selecting same network with overlapping subnets",
			ra:   &testRA{Name: "ra2", AdvertisePods: true, NetworkSelector: map[string]string{"shared-net": "true"}},
			otherRAs: []*testRA{
				{Name: "ra1", AdvertisePods: true, NetworkSelector: map[string]string{"shared-net": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "shared-net", Namespace: "ns1", Network: util.GenerateCUDNNetworkName("shared-net"), Topology: "layer3", Subnet: "30.1.0.0/16", Labels: map[string]string{"shared-net": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_shared-net\":\"30.1.0.0/24\"}"}},
			reconcile:            "ra2",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra2"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra2/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"30.1.0.0/24"}, Imports: []string{"shared-net"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"30.1.0.0/24"}},
						}},
						{ASN: 1, VRF: "shared-net", Imports: []string{"default"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"shared-net": {types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
					HostSubnetLength: 24,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fd01::/48"),
					HostSubnetLength: 64,
				},
			}
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)

			// create test objects
			if tt.ra != nil {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			for _, ra := range tt.otherRAs {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, frrConfig := range tt.frrConfigs {
				_, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrConfig.Namespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			var defaultNAD *nadtypes.NetworkAttachmentDefinition
			for _, nad := range tt.nads {
				n, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if nad.Name == types.DefaultNetworkName && nad.Namespace == config.Kubernetes.OVNConfigNamespace {
					defaultNAD = n
				}
			}

			for _, node := range tt.nodes {
				_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, namespace := range tt.namespaces {
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace.Namespace(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, eip := range tt.eips {
				_, err := fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), eip.EgressIP(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			nm, err := networkmanager.NewForCluster(&nmtest.FakeControllerManager{}, wf, fakeClientset, nil)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c := NewController(nm.Interface(), wf, fakeClientset)

			// prime the default network NAD namespace
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: config.Kubernetes.OVNConfigNamespace,
				},
			}
			_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// prime the default network NAD
			if defaultNAD == nil {
				defaultNAD, err = util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				// update it with the annotation that network manager would set
				defaultNAD.Annotations = map[string]string{types.OvnNetworkNameAnnotation: types.DefaultNetworkName}
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// wait for caches to sync
			cache.WaitForCacheSync(
				context.Background().Done(),
				wf.RouteAdvertisementsInformer().Informer().HasSynced,
				wf.FRRConfigurationsInformer().Informer().HasSynced,
				wf.NADInformer().Informer().HasSynced,
				wf.NodeCoreInformer().Informer().HasSynced,
				wf.EgressIPInformer().Informer().HasSynced,
			)

			err = nm.Start()
			// some test cases start with a bad RA status, avoid asserting
			// initial sync in this case as it will fail
			if tt.ra == nil || tt.ra.Status == nil || *tt.ra.Status == metav1.ConditionTrue {
				g.Expect(err).ToNot(gomega.HaveOccurred())
			} else {
				g.Expect(err).To(gomega.HaveOccurred())
			}
			// we just need the inital sync
			nm.Stop()

			if err := c.reconcile(tt.reconcile); (err != nil) != tt.wantErr {
				t.Fatalf("Controller.reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}

			// verify RA status is set as expected
			if tt.ra != nil {
				ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), tt.reconcile, metav1.GetOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				accepted := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				g.Expect(accepted).NotTo(gomega.BeNil())
				g.Expect(accepted.Status).To(gomega.Equal(tt.expectAcceptedStatus), accepted.Message)
				if tt.expectAcceptedMsg != "" {
					g.Expect(accepted.Message).To(gomega.Equal(tt.expectAcceptedMsg))
				}
			}

			// verify FRRConfigurations have been created/updated/deleted as expected
			actualFRRConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			var actualFRRConfigKeys []string
			actualFRRConfigLabels := map[string]map[string]string{}
			actualFRRConfigSpecs := map[string]*frrapi.FRRConfigurationSpec{}
			for _, frrConfig := range actualFRRConfigs.Items {
				if _, generated := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]; generated {
					actualFRRConfigKeys = append(actualFRRConfigKeys, frrConfig.Annotations[types.OvnRouteAdvertisementsKey])
					actualFRRConfigLabels[frrConfig.Annotations[types.OvnRouteAdvertisementsKey]] = frrConfig.Labels
					actualFRRConfigSpecs[frrConfig.Annotations[types.OvnRouteAdvertisementsKey]] = &frrConfig.Spec
				}
			}

			var expectedRRConfigKeys []string
			expectedFRRConfigLabels := map[string]map[string]string{}
			expectedFRRConfigSpecs := map[string]*frrapi.FRRConfigurationSpec{}
			for _, frrConfig := range tt.expectFRRConfigs {
				expectedFRRConfig := frrConfig.FRRConfiguration()
				expectedRRConfigKeys = append(expectedRRConfigKeys, expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey])
				expectedFRRConfigLabels[expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]] = expectedFRRConfig.Labels
				expectedFRRConfigSpecs[expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]] = &expectedFRRConfig.Spec
			}

			g.Expect(actualFRRConfigKeys).To(gomega.ConsistOf(expectedRRConfigKeys))
			g.Expect(actualFRRConfigLabels).To(gomega.Equal(expectedFRRConfigLabels))
			g.Expect(actualFRRConfigSpecs).To(gomega.Equal(expectedFRRConfigSpecs))

			// verify NADs have been annotated as expected
			actualNADs, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.Background(), metav1.ListOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			actualNADAnnotations := map[string]map[string]string{}
			for _, actualNAD := range actualNADs.Items {
				if len(actualNAD.Annotations) != 0 {
					actualNADAnnotations[actualNAD.Name] = actualNAD.Annotations
				}
			}
			for nad, annotations := range tt.expectNADAnnotations {
				for k, v := range annotations {
					g.Expect(actualNADAnnotations[nad]).To(gomega.HaveKeyWithValue(k, v))
				}
			}
		})
	}
}

func TestUpdates(t *testing.T) {
	testRAs := []*testRA{
		{
			Name:                     "ra1",
			FRRConfigurationSelector: map[string]string{"select": "1"},
			NetworkSelector:          map[string]string{"select": "1"},
			AdvertiseEgressIPs:       true,
			AdvertisePods:            true,
		},
		{
			Name:                     "ra2",
			FRRConfigurationSelector: map[string]string{"select": "2"},
			NetworkSelector:          map[string]string{"select": "2"},
			NodeSelector:             map[string]string{"select": "2"},
		},
		{
			Name:                     "ra3",
			AdvertiseEgressIPs:       true,
			FRRConfigurationSelector: map[string]string{"select": "3"},
			NetworkSelector:          map[string]string{"select": "3"},
			NodeSelector:             map[string]string{"select": "3"},
		},
	}

	tests := []struct {
		name              string
		oldObject         any
		newObject         any
		expectedReconcile []string
	}{
		{
			name:              "reconciles all RAs when an FRRConfig gets created",
			newObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig gets deleted",
			oldObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig labels get updated",
			oldObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			newObject:         &testFRRConfig{Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig annotation changes",
			oldObject:         &testFRRConfig{Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "A"}},
			newObject:         &testFRRConfig{},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig spec changes",
			oldObject:         &testFRRConfig{Generation: 1},
			newObject:         &testFRRConfig{Generation: 2},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles a deleted RA referenced from FRRConfig",
			newObject:         &testFRRConfig{Labels: map[string]string{types.OvnRouteAdvertisementsKey: "ra4"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3", "ra4"},
		},
		{
			name:      "does not reconcile an irrelevant update of FRRConfig",
			oldObject: &testFRRConfig{Annotations: map[string]string{"irrelevant": "irrelevant"}},
			newObject: &testFRRConfig{Annotations: map[string]string{"irrelevant": "still-irrelevant"}},
		},
		{
			name:      "does not reconcile own update of FRRConfig",
			oldObject: &testFRRConfig{Generation: 1},
			newObject: &testFRRConfig{Generation: 2, OwnUpdate: true},
		},
		{
			name:      "does not reconcile own update of FRRConfig",
			oldObject: &testFRRConfig{Generation: 1},
			newObject: &testFRRConfig{Generation: 2, OwnUpdate: true},
		},
		{
			name:              "reconciles all RAs on new NAD",
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on deleted NAD",
			oldObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when NAD labels change",
			oldObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when NAD annotation changes",
			oldObject:         &testNAD{Name: "net", Namespace: "net", OwnUpdate: true, Labels: map[string]string{"select": "2"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles a deleted RA referenced from NAD",
			newObject:         &testNAD{Name: "net", Namespace: "net", Network: "net", Topology: "layer3", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra4\"]"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3", "ra4"},
		},
		{
			name:      "does not reconcile own update of NAD",
			oldObject: &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			newObject: &testNAD{Name: "net", Namespace: "net", OwnUpdate: true, Labels: map[string]string{"select": "2"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
		},
		{
			name:      "does not reconcile a new unsupported (secondary) NAD",
			newObject: &testNAD{Name: "net", Namespace: "net", Network: "net", IsSecondary: true, Topology: "layer3", Labels: map[string]string{"select": "2"}},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on new EIP with status",
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on deleted EIP with status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated EIP status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip2"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated EIP status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}, NamespaceSelector: map[string]string{"selected": "true"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated namespace labels",
			oldObject:         &testNamespace{Name: "ns1", Labels: map[string]string{"selected": "true"}},
			newObject:         &testNamespace{Name: "ns1"},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:      "does not reconcile RAs on new EIP with no status",
			newObject: &testEIP{Name: "eip"},
		},
		{
			// TODO shouldn't happen but needs FIX in controller utility which
			// does not call filter predicate on deletes
			name:              "reconciles all RAs that advertise EIPs on deleted EIP",
			oldObject:         &testEIP{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:      "does not reconcile RAs on updated EIP with no status update",
			oldObject: &testEIP{Name: "eip", Generation: 1, EIPs: map[string]string{"node": "ip"}},
			newObject: &testEIP{Name: "eip", Generation: 2, EIPs: map[string]string{"node": "ip"}},
		},
		{
			name:              "reconciles all RAs on new Node",
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on deleted Node",
			oldObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node labels",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node subnet annotation",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", SubnetsAnnotation: "subnets"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node primary address annotation",
			oldObject:         &testNode{Name: "eip", PrimaryAddressAnnotation: "old"},
			newObject:         &testNode{Name: "eip", PrimaryAddressAnnotation: "new"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:      "does not reconcile RAs on node irrelevant change",
			oldObject: &testNode{Name: "eip", Generation: 1},
			newObject: &testNode{Name: "eip", Generation: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			reconciled := []string{}
			reconciledMutex := sync.Mutex{}
			reconcile := func(ra string) error {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				reconciled = append(reconciled, ra)
				return nil
			}
			matchReconciledRAs := func(g gomega.Gomega, expected []string) {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				g.Expect(reconciled).To(gomega.ConsistOf(expected))
			}
			resetReconciles := func() {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				reconciled = []string{}
			}

			c := NewController(networkmanager.Default().Interface(), wf, fakeClientset)
			config := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
				RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
				Reconcile:      reconcile,
				Threadiness:    1,
				Informer:       wf.RouteAdvertisementsInformer().Informer(),
				Lister:         wf.RouteAdvertisementsInformer().Lister().List,
				ObjNeedsUpdate: raNeedsUpdate,
			}
			c.raController = controllerutil.NewController("", config)

			err = c.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer c.Stop()

			createObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Create(context.Background(), t.FRRConfiguration(), metav1.CreateOptions{})
				case *testNAD:
					_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Create(context.Background(), t.NAD(), metav1.CreateOptions{})
				case *testEIP:
					_, err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), t.EgressIP(), metav1.CreateOptions{})
				case *testNode:
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), t.Node(), metav1.CreateOptions{})
				case *testNamespace:
					_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), t.Namespace(), metav1.CreateOptions{})
				}
				return err
			}
			updateObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Update(context.Background(), t.FRRConfiguration(), metav1.UpdateOptions{})
				case *testNAD:
					_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Update(context.Background(), t.NAD(), metav1.UpdateOptions{})
				case *testEIP:
					_, err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Update(context.Background(), t.EgressIP(), metav1.UpdateOptions{})
				case *testNode:
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), t.Node(), metav1.UpdateOptions{})
				case *testNamespace:
					_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Update(context.Background(), t.Namespace(), metav1.UpdateOptions{})
				}
				return err
			}
			deleteObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNAD:
					err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testEIP:
					err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNode:
					err = fakeClientset.KubeClient.CoreV1().Nodes().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNamespace:
					err = fakeClientset.KubeClient.CoreV1().Namespaces().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				}
				return err
			}

			if tt.oldObject != nil {
				err = createObj(tt.oldObject)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// since we haven't created the RAs yet, this should not reconcile anything
			g.Consistently(matchReconciledRAs).WithArguments([]string{}).Should(gomega.Succeed())

			var raNames []string
			for _, t := range testRAs {
				raNames = append(raNames, t.Name)
				_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), t.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// creating the testRAs, should reconcile them
			g.Eventually(matchReconciledRAs).WithArguments(raNames).Should(gomega.Succeed())
			g.Consistently(matchReconciledRAs).WithArguments(raNames).Should(gomega.Succeed())
			// reset for the actual test
			resetReconciles()

			switch {
			case tt.newObject != nil && tt.oldObject == nil:
				err = createObj(tt.newObject)
			case tt.newObject != nil:
				err = updateObj(tt.newObject)
			default:
				err = deleteObj(tt.oldObject)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Eventually(matchReconciledRAs).WithArguments(tt.expectedReconcile).Should(gomega.Succeed())
			g.Consistently(matchReconciledRAs).WithArguments(tt.expectedReconcile).Should(gomega.Succeed())
		})
	}
}

// BenchmarkCrossRASubnetConflictCheck benchmarks the performance of RouteAdvertisements
// sync when checking for cross-RA subnet conflicts with varying numbers of existing RAs/CUDNs.
// This tests the scenario with no overlaps and compares how much more time it takes to sync
// an RA for a CUDN when no other RAs/CUDNs exist vs an increased number of other RAs/CUDNs existing.
func BenchmarkCrossRASubnetConflictCheck(b *testing.B) {
	// Define different scales for benchmarking
	scales := []struct {
		name        string
		numExisting int // number of existing RAs/CUDNs (1:1 ratio)
	}{
		{"NoExistingRAs", 0},
		{"With10ExistingRAs", 10},
		{"With50ExistingRAs", 50},
		{"With100ExistingRAs", 100},
		{"With500ExistingRAs", 500},
	}

	for _, scale := range scales {
		b.Run(scale.name, func(b *testing.B) {
			// Setup benchmark environment
			benchmarkEnv := setupBenchmarkEnvironment(b, scale.numExisting)

			for b.Loop() {
				err := benchmarkEnv.controller.checkSubnetOverlaps(benchmarkEnv.targetRAObj, benchmarkEnv.selectedNetworks)
				if err != nil {
					b.Errorf("Subnet overlap check failed: %v", err)
				}
				benchmarkEnv.controller.raNetworkCache = make(map[string]*raNetworkCacheEntry)
			}
		})
	}
}

// BenchmarkCrossRASubnetConflictCheck_WarmCache benchmarks the cross-RA subnet conflict check with pre-warmed cache.
// This measures pure cache hit + conflict checking performance, excluding cache building overhead.
// Compare with BenchmarkCrossRASubnetConflictCheck to see the cache effectiveness.
func BenchmarkCrossRASubnetConflictCheck_WarmCache(b *testing.B) {
	// Define different scales for benchmarking
	scales := []struct {
		name        string
		numExisting int // number of existing RAs/CUDNs (1:1 ratio)
	}{
		{"NoExistingRAs", 0},
		{"With10ExistingRAs", 10},
		{"With50ExistingRAs", 50},
		{"With100ExistingRAs", 100},
		{"With500ExistingRAs", 500},
	}

	for _, scale := range scales {
		b.Run(scale.name, func(b *testing.B) {
			// Setup benchmark environment
			benchmarkEnv := setupBenchmarkEnvironment(b, scale.numExisting)

			// Pre-warm the cache by building cache entries for all existing RAs
			warmBenchmarkCache(b, benchmarkEnv)

			for b.Loop() {
				err := benchmarkEnv.controller.checkSubnetOverlaps(benchmarkEnv.targetRAObj, benchmarkEnv.selectedNetworks)
				if err != nil {
					b.Errorf("Subnet overlap check failed: %v", err)
				}
			}
		})
	}
}

// benchmarkSetup sets up the test environment for the benchmark and returns the necessary objects
type benchmarkSetup struct {
	controller       *Controller
	targetRAObj      *ratypes.RouteAdvertisements
	selectedNetworks *selectedNetworks
}

// warmBenchmarkCache pre-warms the cache by building cache entries for all existing RAs.
// This allows measuring pure cache hit + conflict checking performance.
func warmBenchmarkCache(tb testing.TB, benchmarkEnv *benchmarkSetup) {
	tb.Helper()

	allRAs, err := benchmarkEnv.controller.raLister.List(labels.Everything())
	if err != nil {
		tb.Fatalf("Failed to list RAs for cache warming: %v", err)
	}

	for _, ra := range allRAs {
		if ra.Name == benchmarkEnv.targetRAObj.Name {
			continue // Skip the target RA itself
		}
		_, err := benchmarkEnv.controller.updateCachedRANetworkInfoEntry(ra)
		if err != nil {
			// Log but don't fail - some RAs might not have NADs
			tb.Logf("Failed to warm cache for RA %s: %v", ra.Name, err)
		}
	}
}

func setupBenchmarkEnvironment(tb testing.TB, numExistingRAs int) *benchmarkSetup {
	tb.Helper()
	fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.Kubernetes.OVNConfigNamespace = "ovn-kubernetes"

	// Configure cluster for dual-stack (IPv4 + IPv6)
	config.IPv4Mode = true
	config.IPv6Mode = true

	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR:             ovntest.MustParseIPNet("10.128.0.0/14"),
			HostSubnetLength: 24,
		},
		{
			CIDR:             ovntest.MustParseIPNet("fd01::/48"),
			HostSubnetLength: 64,
		},
	}

	// Create default namespace
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.Kubernetes.OVNConfigNamespace,
		},
	}
	_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create namespace: %v", err)
	}

	topology := types.Layer2Topology
	defaultNAD := &testNAD{
		Name:      types.DefaultNetworkName,
		Namespace: config.Kubernetes.OVNConfigNamespace,
		Network:   types.DefaultNetworkName,
		Topology:  topology,
	}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Create(context.Background(), defaultNAD.NAD(), metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create default NAD: %v", err)
	}

	node := &testNode{
		Name:              "benchmark-node",
		SubnetsAnnotation: generateNodeSubnetsAnnotation(numExistingRAs + 1), // +1 for the RA being benchmarked
	}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create node: %v", err)
	}

	// Create existing RAs and CUDNs (1:1 ratio, dualstack)
	for i := 0; i < numExistingRAs; i++ {
		ns := &testNamespace{
			Name: fmt.Sprintf("existing-ns-%d", i),
		}
		_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns.Namespace(), metav1.CreateOptions{})
		if err != nil {
			tb.Fatalf("Failed to create existing namespace %d: %v", i, err)
		}

		nad := &testNAD{
			Name:      fmt.Sprintf("existing-net-%d", i),
			Namespace: fmt.Sprintf("existing-ns-%d", i),
			Network:   util.GenerateCUDNNetworkName(fmt.Sprintf("existing-net-%d", i)),
			Topology:  topology,
			Subnet:    generateNonOverlappingSubnet(i),
			Labels:    map[string]string{fmt.Sprintf("existing-ra-%d", i): "true"},
		}
		_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
		if err != nil {
			tb.Fatalf("Failed to create existing NAD %d: %v", i, err)
		}

		ra := &testRA{
			Name:            fmt.Sprintf("existing-ra-%d", i),
			AdvertisePods:   true,
			NetworkSelector: map[string]string{fmt.Sprintf("existing-ra-%d", i): "true"},
		}
		_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
		if err != nil {
			tb.Fatalf("Failed to create existing RA %d: %v", i, err)
		}
	}

	targetNS := &testNamespace{
		Name: "benchmark-ns",
	}
	_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), targetNS.Namespace(), metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create target namespace: %v", err)
	}

	targetNAD := &testNAD{
		Name:      "benchmark-net",
		Namespace: "benchmark-ns",
		Network:   util.GenerateCUDNNetworkName("benchmark-net"),
		Topology:  topology,
		Subnet:    generateNonOverlappingSubnet(numExistingRAs),
		Labels:    map[string]string{"benchmark": "true"},
	}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(targetNAD.Namespace).Create(context.Background(), targetNAD.NAD(), metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create target NAD: %v", err)
	}

	targetRA := &testRA{
		Name:            "benchmark-ra",
		AdvertisePods:   true,
		NetworkSelector: map[string]string{"benchmark": "true"},
	}
	_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), targetRA.RouteAdvertisements(), metav1.CreateOptions{})
	if err != nil {
		tb.Fatalf("Failed to create target RA: %v", err)
	}

	// Create a simplified controller setup for benchmarking
	wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
	if err != nil {
		tb.Fatalf("Failed to create watch factory: %v", err)
	}

	if err := wf.Start(); err != nil {
		tb.Fatalf("Failed to start watch factory: %v", err)
	}
	cache.WaitForCacheSync(
		context.Background().Done(),
		wf.RouteAdvertisementsInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
	)

	nm, err := networkmanager.NewForCluster(&nmtest.FakeControllerManager{}, wf, fakeClientset, nil)
	if err != nil {
		tb.Fatalf("Failed to create network manager: %v", err)
	}

	if err := nm.Start(); err != nil {
		tb.Fatalf("Failed to start network manager: %v", err)
	}

	// Wait for network manager to process NADs
	time.Sleep(50 * time.Millisecond)

	controller := NewController(nm.Interface(), wf, fakeClientset)

	targetRAObj := targetRA.RouteAdvertisements()
	selectedNADs := []*nadtypes.NetworkAttachmentDefinition{targetNAD.NAD()}

	selectedNetworks, err := controller.buildSelectedNetworkInfo(selectedNADs, sets.New(targetRAObj.Spec.Advertisements...))
	if err != nil {
		tb.Fatalf("Failed to build selected network info: %v", err)
	}

	// Ensure we have subnets for meaningful benchmarking
	if len(selectedNetworks.subnets) == 0 {
		tb.Fatalf("No subnets found in selected networks - network manager may not have processed NADs")
	}

	return &benchmarkSetup{
		controller:       controller,
		targetRAObj:      targetRAObj,
		selectedNetworks: selectedNetworks,
	}
}

// generateNodeSubnetsAnnotation generates a node subnets annotation for the given number of networks
func generateNodeSubnetsAnnotation(numNetworks int) string {
	subnets := make(map[string][]string)
	subnets["default"] = []string{"10.128.0.0/24", "fd01::/64"}

	for i := 0; i < numNetworks-1; i++ {
		networkName := util.GenerateCUDNNetworkName(fmt.Sprintf("existing-net-%d", i))
		dualStackSubnet := generateNonOverlappingNodeSubnet(i)
		subnetParts := strings.Split(dualStackSubnet, ",")
		subnets[networkName] = subnetParts
	}

	benchmarkNetworkName := util.GenerateCUDNNetworkName("benchmark-net")
	dualStackSubnet := generateNonOverlappingNodeSubnet(numNetworks - 1)
	subnetParts := strings.Split(dualStackSubnet, ",")
	subnets[benchmarkNetworkName] = subnetParts

	var pairs []string
	for network, subnetList := range subnets {
		subnetJSON := "[\"" + strings.Join(subnetList, "\",\"") + "\"]"
		pairs = append(pairs, fmt.Sprintf("\"%s\":%s", network, subnetJSON))
	}
	return fmt.Sprintf("{%s}", strings.Join(pairs, ", "))
}

// generateNonOverlappingSubnet generates a unique dualstack subnet for the given index to avoid overlaps
// Returns both IPv4 and IPv6 subnets separated by comma
func generateNonOverlappingSubnet(index int) string {
	// Use 10.x.y.0/24 to support larger ranges
	// This allows for up to 65536 different subnets (256 * 256)
	thirdOctet := index / 256
	fourthOctet := index % 256
	ipv4 := fmt.Sprintf("10.%d.%d.0/24", thirdOctet, fourthOctet)

	ipv6 := fmt.Sprintf("fd00:%x::/64", index)

	return fmt.Sprintf("%s,%s", ipv4, ipv6)
}

// generateNonOverlappingNodeSubnet generates a unique dualstack node subnet (smaller than the network subnet)
func generateNonOverlappingNodeSubnet(index int) string {
	thirdOctet := index / 256
	fourthOctet := index % 256
	ipv4 := fmt.Sprintf("10.%d.%d.0/26", thirdOctet, fourthOctet)

	ipv6 := fmt.Sprintf("fd00:%x::/66", index)

	return fmt.Sprintf("%s,%s", ipv4, ipv6)
}
