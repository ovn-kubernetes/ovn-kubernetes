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
	"k8s.io/apimachinery/pkg/runtime"
	ctesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
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
		frrConfigs           []*testFRRConfig
		nads                 []*testNAD
		nodes                []*testNode
		namespaces           []*testNamespace
		eips                 []*testEIP
		reconcile            string
		wantErr              bool
		expectAcceptedStatus metav1.ConditionStatus
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
			name: "reconciles pod RouteAdvertisement with targetVRF 'default' matching router with empty VRF string",
			ra:   &testRA{Name: "ra", TargetVRF: "default", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						// Router with empty VRF (default VRF in FRR)
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_red\":\"1.2.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						// Router in default VRF (empty string) with imports from red VRF
						{ASN: 1, Prefixes: []string{"1.2.0.0/24"}, Imports: []string{"red"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.0.0/24"}},
						}},
						// Router in red VRF with imports from default VRF
						{ASN: 1, VRF: "red", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
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

			nm, err := networkmanager.NewForCluster(&nmtest.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
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
