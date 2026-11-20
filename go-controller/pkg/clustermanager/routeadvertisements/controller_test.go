// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	corelisters "k8s.io/client-go/listers/core/v1"
	ctesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// failingEIPLister injects an operational failure after VTEP validation.
type failingEIPLister struct {
	eiplisters.EgressIPLister
	err error
}

// List injects a retryable cache failure while leaving other lister behavior intact.
func (l failingEIPLister) List(labels.Selector) ([]*eiptypes.EgressIP, error) {
	return nil, l.err
}

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
		"node":  "1.0.1.100/24",
		"node1": "1.0.1.101/24",
		"node2": "1.0.2.100/24",
	}
	nodePrimaryAddrIPv6 = map[string]string{
		"node":  "fd03::ffff:0100:0050/64",
		"node1": "fd03::ffff:0100:0051/64",
		"node2": "fd03::ffff:0200:0050/64",
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
	Name                      string
	Generation                int
	Labels                    map[string]string
	PrimaryAddressAnnotation  string
	InternalIPs               []string
	OmitPrimaryAddress        bool
	DPUHostAddressAnnotation  string
	SubnetsAnnotation         string
	L3GatewayConfigAnnotation string
	ChassisIDAnnotation       string
	VTEPIPs                   map[string][]string
	RawVTEPAnnotation         *string
}

// Node builds a node fixture with independently controlled primary and internal
// addresses, allowing tests to distinguish managed and user-provided peering.
func (tn testNode) Node() *corev1.Node {
	primaryAddressAnnotation := tn.PrimaryAddressAnnotation
	if primaryAddressAnnotation == "" {
		primaryAddressAnnotation = "{\"ipv4\":\"" + nodePrimaryAddr[tn.Name] + "\", \"ipv6\":\"" + nodePrimaryAddrIPv6[tn.Name] + "\"}"
	}
	annotations := map[string]string{
		util.OvnNodeIfAddr: primaryAddressAnnotation,
	}
	if tn.SubnetsAnnotation != "" {
		annotations[types.NodeSubnetsAnnotation] = tn.SubnetsAnnotation
	}
	if tn.DPUHostAddressAnnotation != "" {
		annotations[util.OVNNodePrimaryDPUHostAddr] = tn.DPUHostAddressAnnotation
	}
	if tn.L3GatewayConfigAnnotation != "" {
		annotations[util.OvnNodeL3GatewayConfig] = tn.L3GatewayConfigAnnotation
		annotations[util.OvnNodeChassisID] = "chassis-" + tn.Name
	}
	if tn.ChassisIDAnnotation != "" {
		annotations[util.OvnNodeChassisID] = tn.ChassisIDAnnotation
	}
	if tn.RawVTEPAnnotation != nil {
		annotations[util.OVNNodeVTEPs] = *tn.RawVTEPAnnotation
	} else if len(tn.VTEPIPs) > 0 {
		vteps := make(map[string]util.VTEPNodeAnnotation, len(tn.VTEPIPs))
		for vtepName, ips := range tn.VTEPIPs {
			vteps[vtepName] = util.VTEPNodeAnnotation{IPs: ips}
		}
		vtepJSON, _ := json.Marshal(vteps)
		annotations[util.OVNNodeVTEPs] = string(vtepJSON)
	}
	if tn.OmitPrimaryAddress {
		delete(annotations, util.OvnNodeIfAddr)
	}
	var addresses []corev1.NodeAddress
	for _, ip := range tn.InternalIPs {
		addresses = append(addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	nodeLabels := map[string]string{corev1.LabelHostname: tn.Name}
	maps.Copy(nodeLabels, tn.Labels)
	return &corev1.Node{
		Status: corev1.NodeStatus{Addresses: addresses},
		ObjectMeta: metav1.ObjectMeta{
			Name:        tn.Name,
			Labels:      nodeLabels,
			Generation:  int64(tn.Generation),
			Annotations: annotations,
		},
	}
}

// TestDeleteIncompleteVTEPConfigurations checks that partial deletion failures remain retryable
// without preventing cleanup of other rejected nodes.
func TestDeleteIncompleteVTEPConfigurations(t *testing.T) {
	g := gomega.NewWithT(t)
	client := frrfake.NewSimpleClientset()
	for _, name := range []string{"source", "fail-a", "fail-b", "delete", "keep"} {
		cfg := generatedFRRConfig(name, "invalid-node", true).FRRConfiguration()
		cfg.Namespace = "test"
		if name == "source" {
			cfg.Annotations = nil
		}
		if name == "keep" {
			cfg.Annotations[types.OvnRouteAdvertisementsKey] = "ra/frrConfig/valid-node"
			cfg.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"] = "valid-node"
		}
		_, err := client.ApiV1beta1().FRRConfigurations("test").Create(context.Background(), cfg, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "creating FRRConfiguration %s", name)
	}
	failureA, failureB := errors.New("delete A failed"), errors.New("delete B failed")
	client.PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
		switch action.(ctesting.DeleteAction).GetName() {
		case "fail-a":
			return true, nil, failureA
		case "fail-b":
			return true, nil, failureB
		}
		return false, nil, nil
	})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	cached, err := client.ApiV1beta1().FRRConfigurations("test").List(context.Background(), metav1.ListOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "listing configurations for the cache")
	for i := range cached.Items {
		g.Expect(indexer.Add(&cached.Items[i])).To(gomega.Succeed())
	}
	frrLister := frrlisters.NewFRRConfigurationLister(indexer)
	c := &Controller{pendingFRR: newPendingWrites(indexer), frrClient: client, frrLister: frrLister}
	updated, err := c.deleteIncompleteVTEPConfigurations(context.Background(), "ra", sets.New("invalid-node"))
	g.Expect(updated).To(gomega.BeTrue(), "successful deletion must be recorded despite other failures")
	// The sample still counts the object deleted in this pass until the retry.
	expectFRRMetrics(t, "ra", 4, 1, 0)
	g.Expect(errors.Is(err, failureA)).To(gomega.BeTrue(), "cleanup must retain the first deletion error")
	g.Expect(errors.Is(err, failureB)).To(gomega.BeTrue(), "cleanup must retain the second deletion error")
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "cleanup failures must trigger retries")
	objects, listErr := client.ApiV1beta1().FRRConfigurations("test").List(context.Background(), metav1.ListOptions{})
	g.Expect(listErr).NotTo(gomega.HaveOccurred(), "listing configurations after cleanup")
	names := []string{}
	for _, cfg := range objects.Items {
		names = append(names, cfg.Name)
	}
	g.Expect(names).To(gomega.ConsistOf("source", "fail-a", "fail-b", "keep"), "cleanup must delete only eligible configurations whose deletion succeeds")
}

// TestConflictingVTEPNodes covers ownership continuity, ambiguous claims, and
// idempotence after rejected configurations are removed.
func TestConflictingVTEPNodes(t *testing.T) {
	const prefix = "100.64.0.1/32"
	config := func(ra, node, vrf, ip string) *frrapi.FRRConfiguration {
		return &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:        generateName + node,
				Labels:      map[string]string{types.OvnRouteAdvertisementsKey: ra},
				Annotations: map[string]string{types.OvnRouteAdvertisementsKey: ra + "/source/" + node},
			},
			Spec: frrapi.FRRConfigurationSpec{
				NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": node}},
				BGP:          frrapi.BGPConfig{Routers: []frrapi.Router{{VRF: vrf, Prefixes: []string{ip}}}},
			},
		}
	}
	mismatchedAnnotation := config("other", "a", "", prefix)
	mismatchedAnnotation.Labels[types.OvnRouteAdvertisementsKey] = "ra"

	for _, tt := range []struct {
		name        string
		claims      map[string]sets.Set[string]
		otherClaims map[string]sets.Set[string]
		existing    []*frrapi.FRRConfiguration
		rejected    []string
		message     string
	}{
		{name: "cross RA bootstrap excludes local claimant", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, rejected: []string{"a"}, message: "no unique established advertiser"},
		{name: "preserves owner from another RA", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("other", "b", "", prefix)}, rejected: []string{"a"}, message: "preserving established advertiser b"},
		{name: "preserves local owner against another RA", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, message: "preserving established advertiser a"},
		{name: "cross RA ambiguous established claims", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("other", "b", "", prefix)}, rejected: []string{"a"}, message: "no unique established advertiser"},
		{name: "cross RA IPv6 ownership", claims: map[string]sets.Set[string]{"a": sets.New("fd00::1/128")}, otherClaims: map[string]sets.Set[string]{"b": sets.New("fd00::1/128")}, existing: []*frrapi.FRRConfiguration{config("other", "b", "", "fd00::1/128")}, rejected: []string{"a"}, message: "preserving established advertiser b"},
		{name: "same node shared across RAs", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"a": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("other", "a", "", prefix)}},
		{name: "deleted RA is not ownership evidence", existing: []*frrapi.FRRConfiguration{config("deleted", "a", "", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "remote-only conflict does not reject local advertisements", claims: map[string]sets.Set[string]{"a": sets.New("100.64.0.2/32")}, otherClaims: map[string]sets.Set[string]{"a": sets.New(prefix), "b": sets.New(prefix)}},
		{name: "bootstrap excludes both", rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "preserves established node", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, rejected: []string{"b"}, message: "preserving established advertiser a"},
		{name: "multiple configurations for one node count once", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("ra", "a", "", prefix)}, rejected: []string{"b"}, message: "preserving established advertiser a"},
		{name: "both established excludes both", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("ra", "b", "", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "mismatched RA annotation is not ownership evidence", existing: []*frrapi.FRRConfiguration{mismatchedAnnotation}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "non-default VRF is not ownership evidence", existing: []*frrapi.FRRConfiguration{config("ra", "a", "blue", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "former claimant is not an owner", claims: map[string]sets.Set[string]{"b": sets.New(prefix), "c": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, rejected: []string{"b", "c"}, message: "no unique established advertiser"},
		{name: "handover after old claim disappears", claims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}},
		{name: "IPv6 ownership", claims: map[string]sets.Set[string]{"a": sets.New("fd00::1/128"), "b": sets.New("fd00::1/128")}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", "fd00::1/128")}, rejected: []string{"b"}, message: "preserving established advertiser a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := tt.claims
			if claims == nil {
				claims = map[string]sets.Set[string]{"a": sets.New(prefix), "b": sets.New(prefix)}
			}
			g := gomega.NewWithT(t)
			rejected, err := conflictingVTEPNodes("ra", map[string]map[string]sets.Set[string]{"ra": claims, "other": tt.otherClaims}, tt.existing)
			g.Expect(rejected.UnsortedList()).To(gomega.ConsistOf(tt.rejected), "rejected nodes for claims %v", claims)
			if tt.message == "" {
				g.Expect(err).NotTo(gomega.HaveOccurred(), "non-conflicting claims must be accepted")
			} else {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.message)), "conflict must identify the ownership decision")
			}
			// Simulate deletion of rejected nodes' configurations and a second
			// reconciliation with unchanged claims (including after restart).
			retained := []*frrapi.FRRConfiguration{}
			for _, cfg := range tt.existing {
				if !rejected.Has(cfg.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"]) {
					retained = append(retained, cfg)
				}
			}
			again, _ := conflictingVTEPNodes("ra", map[string]map[string]sets.Set[string]{"ra": claims, "other": tt.otherClaims}, retained)
			g.Expect(again).To(gomega.Equal(rejected), "cleanup must not change the rejection decision on the next reconcile")
		})
	}
}

// TestGetNodeVTEPPrefixes checks selection isolation, canonical host prefixes,
// and enforcement of each VTEP's configured CIDRs.
func TestGetNodeVTEPPrefixes(t *testing.T) {
	cidrs := map[string][]*net.IPNet{
		"vtep":  {ovntest.MustParseIPNet("100.64.0.0/16"), ovntest.MustParseIPNet("fd00::/64")},
		"other": {ovntest.MustParseIPNet("192.0.2.0/24")},
	}
	for _, tt := range []struct {
		name      string
		raw       string
		ips       []string
		prefixes  []string
		wantError bool
	}{
		{name: "canonical dual-stack host prefixes", ips: []string{"100.64.0.1", "fd00:0:0:0::1"}, prefixes: []string{"100.64.0.1/32", "fd00::1/128"}},
		{name: "wrong-shaped unselected entry", raw: `{"vtep":{"ips":["100.64.0.1"]},"unselected":"invalid"}`, prefixes: []string{"100.64.0.1/32"}},
		{name: "wrong-shaped unselected IPs", raw: `{"vtep":{"ips":["100.64.0.1"]},"unselected":{"ips":42}}`, prefixes: []string{"100.64.0.1/32"}},
		{name: "wrong-shaped selected entry", raw: `{"vtep":"invalid"}`, wantError: true},
		{name: "wrong-shaped selected IPs", raw: `{"vtep":{"ips":42}}`, wantError: true},
		{name: "malformed annotation JSON", raw: `{"vtep":`, wantError: true},
		{name: "IPv4 outside the CIDRs", ips: []string{"198.51.100.1"}, wantError: true},
		{name: "IPv6 outside the CIDRs", ips: []string{"fd01::1"}, wantError: true},
		{name: "address in another VTEP's CIDR", ips: []string{"192.0.2.1"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := (&testNode{Name: "node", VTEPIPs: map[string][]string{"vtep": tt.ips, "unselected": {"not-an-ip"}}}).Node()
			if tt.raw != "" {
				node.Annotations[util.OVNNodeVTEPs] = tt.raw
			}
			prefixes, err := getNodeVTEPPrefixes(node, cidrs)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError {
				gomega.NewWithT(t).Expect(prefixes.UnsortedList()).To(gomega.ConsistOf(tt.prefixes), "advertised prefixes for annotation %s", node.Annotations[util.OVNNodeVTEPs])
			}
		})
	}
}

func TestGetDPUHostGatewayNextHopsUsesDPUHostAddressAnnotation(t *testing.T) {
	g := gomega.NewWithT(t)
	previousGatewayMode := config.Gateway.Mode
	t.Cleanup(func() { config.Gateway.Mode = previousGatewayMode })
	config.Gateway.Mode = config.GatewayModeShared

	c := &Controller{}
	selected := &selectedNetworks{networkUplinks: map[string]string{}}

	nextHops, err := c.getDPUHostGatewayNextHops(&corev1.Node{}, selected, types.DefaultNetworkName)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(nextHops).To(gomega.BeNil(), "a node without the annotation is not a DPU host")

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Annotations: map[string]string{util.OVNNodePrimaryDPUHostAddr: "invalid"},
	}}
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errPending)).To(gomega.BeTrue(), "wait for valid DPU-host annotation state")
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse())

	node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"not-a-cidr"}`
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errPending)).To(gomega.BeTrue(), "wait for a valid DPU-host CIDR")
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse())

	for _, annotation := range []string{
		`{"ipv4":"fd00::1/64"}`,
		`{"ipv6":"192.0.2.1/24"}`,
	} {
		node.Annotations[util.OVNNodePrimaryDPUHostAddr] = annotation
		_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
		g.Expect(errors.Is(err, errPending)).To(gomega.BeTrue(),
			"wait for the node to publish a CIDR in the correct address-family field: %s", annotation)
		g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse())
	}

	node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"172.18.255.254/16"}`
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errPending)).To(gomega.BeTrue(), "a DPU host waits for its gateway state")
}

type testPod struct {
	Name      string
	Namespace string
	Node      string
}

func (tp testPod) Pod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tp.Name,
			Namespace: tp.Namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: tp.Node,
		},
	}
}

type testPrefixSelector struct {
	Prefix string
	LE     uint32
	GE     uint32
}

type testNeighbor struct {
	ASN                    uint32
	Address                string
	Interface              string
	DualStackAddressFamily *bool
	Advertise              []string
	NextHopV4              string
	NextHopV6              string
	Receive                []testPrefixSelector
}

func (tn testNeighbor) Neighbor() frrapi.Neighbor {
	n := frrapi.Neighbor{
		ASN:       tn.ASN,
		Address:   tn.Address,
		Interface: tn.Interface,
		ToAdvertise: frrapi.Advertise{
			Allowed: frrapi.AllowedOutPrefixes{
				Mode:     frrapi.AllowRestricted,
				Prefixes: tn.Advertise,
			},
			NextHop: frrapi.NextHop{
				IPv4: tn.NextHopV4,
				IPv6: tn.NextHopV6,
			},
		},
	}
	if tn.DualStackAddressFamily != nil {
		n.DualStackAddressFamily = *tn.DualStackAddressFamily
	}
	if len(tn.Receive) > 0 {
		prefixSelectors := make([]frrapi.PrefixSelector, 0, len(tn.Receive))
		for _, ps := range tn.Receive {
			prefixSelectors = append(prefixSelectors, frrapi.PrefixSelector{
				Prefix: ps.Prefix,
				LE:     ps.LE,
				GE:     ps.GE,
			})
		}
		n.ToReceive = frrapi.Receive{
			Allowed: frrapi.AllowedInPrefixes{
				Mode:     frrapi.AllowRestricted,
				Prefixes: prefixSelectors,
			},
		}
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
	RawConfig    string
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
	rawConfig := tf.RawConfig
	if rawConfig == "" {
		rawConfig = tf.generateUnicastRawConfig()
	}
	if rawConfig != "" {
		f.Spec.Raw.Config = rawConfig
		f.Spec.Raw.Priority = rawConfigPriority
	}
	if tf.OwnUpdate {
		f.ManagedFields = append(f.ManagedFields, metav1.ManagedFieldsEntry{
			Manager: fieldManager,
			Time:    &metav1.Time{Time: time.Now()},
		})
	}
	return f
}

func (tf testFRRConfig) generateUnicastRawConfig() string {
	var buf strings.Builder
	routers := make(map[string]*testRouter, len(tf.Routers))
	for _, r := range tf.Routers {
		routers[r.VRF] = r
	}
	for _, vrf := range slices.Sorted(maps.Keys(routers)) {
		r := routers[vrf]
		if len(r.Neighbors) == 0 {
			continue
		}
		if r.VRF == "" {
			fmt.Fprintf(&buf, "router bgp %d\n", r.ASN)
		} else {
			fmt.Fprintf(&buf, "router bgp %d vrf %s\n", r.ASN, r.VRF)
		}
		for _, n := range r.Neighbors {
			switch {
			case n.Address == "" && n.Interface != "":
				// unnumbered neighbors are included in both address families
				fmt.Fprintf(&buf, " address-family ipv4 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Interface)
				fmt.Fprintf(&buf, " address-family ipv6 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Interface)
			case utilnet.IsIPv6String(n.Address):
				fmt.Fprintf(&buf, " address-family ipv6 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Address)
			default:
				fmt.Fprintf(&buf, " address-family ipv4 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Address)
			}
		}
		buf.WriteString("exit\n!\n")
	}
	return buf.String()
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
	Name                  string
	Namespace             string
	Network               string
	Subnet                string
	Labels                map[string]string
	Annotations           map[string]string
	IsSecondary           bool
	Topology              string
	Transport             string
	OwnUpdate             bool
	EVPNVTEPName          string
	EVPNMACVRFVNI         int32
	EVPNMACVRFRouteTarget string
	EVPNIPVRFVNI          int32
	EVPNIPVRFRouteTarget  string
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

	// Build the config as a map to properly marshal EVPN config
	cniConfig := map[string]interface{}{
		"cniVersion":       "1.1.0",
		"name":             tn.Network,
		"type":             config.CNI.Plugin,
		"netAttachDefName": tn.Namespace + "/" + tn.Name,
	}

	if tn.Topology != "" {
		cniConfig["topology"] = tn.Topology
	}
	if tn.Subnet != "" {
		cniConfig["subnets"] = tn.Subnet
	}
	if tn.Topology != "" && !tn.IsSecondary {
		cniConfig["role"] = "primary"
	}
	if tn.Transport != "" {
		cniConfig["transport"] = tn.Transport
	}

	// Add EVPN configuration if present
	if tn.EVPNMACVRFVNI != 0 || tn.EVPNIPVRFVNI != 0 {
		evpnConfig := map[string]interface{}{}
		if tn.EVPNVTEPName != "" {
			evpnConfig["vtep"] = tn.EVPNVTEPName
		}
		if tn.EVPNMACVRFVNI != 0 {
			macvrf := map[string]interface{}{
				"vni": tn.EVPNMACVRFVNI,
			}
			if tn.EVPNMACVRFRouteTarget != "" {
				macvrf["routeTarget"] = tn.EVPNMACVRFRouteTarget
			}
			evpnConfig["macVRF"] = macvrf
		}
		if tn.EVPNIPVRFVNI != 0 {
			ipvrf := map[string]interface{}{
				"vni": tn.EVPNIPVRFVNI,
			}
			if tn.EVPNIPVRFRouteTarget != "" {
				ipvrf["routeTarget"] = tn.EVPNIPVRFRouteTarget
			}
			evpnConfig["ipVRF"] = ipvrf
		}
		cniConfig["evpn"] = evpnConfig
	}

	configBytes, _ := json.Marshal(cniConfig)
	nad.Spec.Config = string(configBytes)

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
	config.Layer2UsesTransitRouter = true

	// Set feature flags before any test calls RegisterClusterManagerFunctional().
	// The sync.Once in registration captures whichever flags are set on the first call.
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEVPN = true
	config.Metrics.EnableScaleMetrics = true

	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
}

// TestController_reconcile checks generated configuration, cleanup, NAD
// references, and status against selected networks and nodes.
func TestController_reconcile(t *testing.T) {
	metrics.RegisterClusterManagerFunctional()

	frrNamespace := "frrNamespace"
	previousExport := func(node, prefix string) *testFRRConfig {
		cfg := generatedFRRConfig(generateName+"old", node, true)
		cfg.Routers[0].Prefixes = []string{prefix}
		cfg.Routers[0].Neighbors[0].Advertise = []string{prefix}
		return cfg
	}
	type reconcileCase struct {
		fixtureID            string
		name                 string
		ra                   *testRA
		otherRAs             []*testRA
		frrConfigs           []*testFRRConfig
		nads                 []*testNAD
		nodes                []*testNode
		namespaces           []*testNamespace
		pods                 []*testPod
		eips                 []*testEIP
		vteps                []*vtepv1.VTEP
		reconcile            string
		transport            string
		gatewayMode          config.GatewayMode
		dynamicUDN           bool
		ipv6                 bool
		wantErr              bool
		eipListError         error
		expectAcceptedStatus metav1.ConditionStatus
		expectDegradedStatus metav1.ConditionStatus
		// expectAcceptedMessageContains, when set, is a substring the Accepted
		// condition message must contain
		expectAcceptedMessageContains string
		expectDegradedMessageContains string
		expectFRRConfigs              []*testFRRConfig
		expectNADAnnotations          map[string]map[string]string
	}
	tests := []reconcileCase{
		{
			name: "adds the validated marker to a generated FRRConfiguration that predates input validation",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
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
					Name:         generateName + "existing",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
		},
		{
			fixtureID: "defaultVRF",
			name:      "reconciles pod+eip RouteAdvertisement for a single FRR config, node and default network and target VRF",
			ra:        &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
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
			expectDegradedStatus: metav1.ConditionFalse,
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
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]", types.OvnRouteAdvertisementsIntentKey: `{"ra":{"targetVRF":"","egressIP":true}}`}},
		},
		{
			name: "reconciles pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles dual-stack pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24", "fd01::/64"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles IPv6-only pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"fd01::/64\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"fd01::/64"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile a neighbor with neither address nor interface",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "neither address nor interface",
		},
		{
			name: "fails to reconcile a neighbor with neither address nor interface in an IPv6-only cluster",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"fd01::/64\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "neither address nor interface",
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
			expectDegradedStatus: metav1.ConditionFalse,
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
			expectDegradedStatus: metav1.ConditionFalse,
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
			fixtureID: "tenantVRF",
			name:      "(layer3) reconciles eip RouteAdvertisement for a single FRR config, node, non default network and non default target VRF",
			ra:        &testRA{Name: "ra", TargetVRF: "red", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
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
			expectDegradedStatus: metav1.ConditionFalse,
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
			expectDegradedStatus: metav1.ConditionTrue,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"green": {types.OvnRouteAdvertisementsKey: "", types.OvnRouteAdvertisementsIntentKey: ""}},
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
					Name:         generateName + "existing",
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
			expectDegradedStatus: metav1.ConditionFalse,
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
			fixtureID: "autoVRF",
			name:      "reconciles a RouteAdvertisement for multiple selected FRR configs, nodes and networks on auto target VRF",
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
			expectDegradedStatus: metav1.ConditionFalse,
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
					Name:         generateName + "existing",
					Namespace:    frrNamespace,
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
			expectDegradedStatus: metav1.ConditionFalse,
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
		},
		{
			name:      "reconciles pod RouteAdvertisement for default network in no-overlay mode with ToReceive routes",
			ra:        &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			transport: types.NetworkTransportNoOverlay,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Receive: []testPrefixSelector{{Prefix: "1.2.0.0/16"}}},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}, Receive: []testPrefixSelector{{Prefix: "1.1.0.0/16", LE: 24, GE: 24}}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:        "reconciles pod RouteAdvertisement for DPU host default network with next-hop",
			ra:          &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			gatewayMode: config.GatewayModeShared,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                      "node",
					DPUHostAddressAnnotation:  `{"ipv4":"172.18.255.254/16"}`,
					SubnetsAnnotation:         "{\"default\":\"1.1.0.0/24\"}",
					L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16"],"next-hops":["172.18.0.1"]}}`,
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}, NextHopV4: "172.18.255.254"},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:        "reconciles dual-stack pod RouteAdvertisement for DPU host with an unnumbered interface neighbor and next-hops",
			ra:          &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			gatewayMode: config.GatewayModeShared,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                      "node",
					DPUHostAddressAnnotation:  `{"ipv4":"172.18.255.254/16","ipv6":"fc00:f853:ccd:e793::4/64"}`,
					SubnetsAnnotation:         "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}",
					L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16","fc00:f853:ccd:e793::4/64"],"next-hops":["172.18.0.1","fc00:f853:ccd:e793::1"]}}`,
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24", "fd01::/64"}, NextHopV4: "172.18.255.254", NextHopV6: "fc00:f853:ccd:e793::4"},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:      "reconciles pod RouteAdvertisement for CUDN in no-overlay mode with ToReceive routes including CUDN pod subnets",
			ra:        &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"selected": "true"}},
			transport: types.NetworkTransportNoOverlay,
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue-ns", Network: types.CUDNPrefix + "blue", Topology: "layer3", Subnet: "10.10.0.0/16/24", Transport: types.NetworkTransportNoOverlay, Labels: map[string]string{"selected": "true"}},
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
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\",\"" + types.CUDNPrefix + "blue\":\"10.10.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "10.10.1.0/24"}, VRF: "", Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							// ToReceive should include pod subnets from all no-overlay networks in alphabetical order: cluster-udn-blue (10.10.0.0/16) then default (1.1.0.0/16)
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24", "10.10.1.0/24"}, Receive: []testPrefixSelector{
								{Prefix: "10.10.0.0/16", LE: 24, GE: 24},
								{Prefix: "1.1.0.0/16", LE: 24, GE: 24},
							}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{
				"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"blue":    {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "fails to reconcile a secondary network",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", IsSecondary: true, Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name: "fails to reconcile an non-cluster UDN",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name:                          "rejects a malformed EgressIP node selector",
			ra:                            &testRA{Name: "ra", SelectsDefault: true, AdvertiseEgressIPs: true, NodeSelector: map[string]string{"invalid key": "true"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid nodeSelector",
		},
		{
			name:                          "rejects a malformed cluster UDN selector",
			ra:                            &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"invalid key": "true"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid clusterUserDefinedNetworkSelector.networkSelector",
		},
		{
			name:                          "rejects a node selector for pod network advertisements",
			ra:                            &testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, NodeSelector: map[string]string{"selected": "true"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: `node selector has to select all nodes if pod network is advertised`,
		},
		{
			name: "degrades when the new EgressIP owner has no peering template",
			ra:   &testRA{Name: "ra", SelectsDefault: true, AdvertiseEgressIPs: true},
			frrConfigs: []*testFRRConfig{
				previousExport("node1", "192.0.2.10/32"),
				{
					Name:         "frrConfig",
					Namespace:    frrNamespace,
					NodeSelector: map[string]string{corev1.LabelHostname: "node1"},
					Routers:      []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth0"}}}},
				},
			},
			nodes:                         []*testNode{{Name: "node1"}, {Name: "node2"}},
			eips:                          []*testEIP{{Name: "eip", EIPs: map[string]string{"node2": "192.0.2.10"}}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: `node "node2": configuration error: no peering template selects the assigned EgressIP owner`,
		},
		{
			name: "degrades when the node has no subnet annotation",
			ra:   &testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true},
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
			nodes:                         []*testNode{{Name: "node"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: `could not find "k8s.ovn.org/node-subnets" annotation`,
		},
		{
			name: "degrades when the subnet annotation omits the selected network",
			ra:   &testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true},
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
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"red\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: `node "node" has no "k8s.ovn.org/node-subnets" annotation for network default`,
		},
		{
			name: "degrades when the selected FRRConfiguration has no matching VRF",
			ra:   &testRA{Name: "ra", SelectsDefault: true, TargetVRF: "red", AdvertisePods: true},
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
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: `has no VRF matching the RouteAdvertisements target VRF`,
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
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "missing [cluster_udn_blue]",
			expectFRRConfigs: []*testFRRConfig{{
				Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
				Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
				NodeSelector: map[string]string{corev1.LabelHostname: "node"},
				Routers: []*testRouter{{ASN: 1, VRF: "red", Prefixes: []string{"1.1.0.0/24"},
					Neighbors: []*testNeighbor{{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}}}}},
			}},
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
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name: "degrades when a peer enables DualStackAddressFamily",
			ra:   &testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", DualStackAddressFamily: ptr.To(true)},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: `DualStackAddressFamily==true`,
		},
		{
			name:    "retries managed peering without InternalIP addresses",
			wantErr: true,
			ra:      &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{{Name: types.ManagedBGPConfigurationName, Namespace: frrNamespace,
				Routers: []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Address: "10.0.0.2"}}}},
			}},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: `{ "default": "1.1.0.0/24" }`}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "waiting for InternalIP addresses",
		},

		{
			name: "rejects mismatched template VRF even while node subnets are unavailable",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{{Name: types.ManagedBGPConfigurationName, Namespace: frrNamespace,
				Routers: []*testRouter{{ASN: 1, VRF: "wrong-vrf", Neighbors: []*testNeighbor{{ASN: 1, Address: "10.0.0.2"}}}},
			}},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: `{}`}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "has no VRF matching",
		},

		{
			fixtureID: "missingInternalIP",
			name:      "rejects invalid peers even while nodes lack InternalIP addresses",
			ra:        &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{{Name: types.ManagedBGPConfigurationName, Namespace: frrNamespace,
				Routers: []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Address: "10.0.0.2", DualStackAddressFamily: ptr.To(true)}}}},
			}},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: `{ "default": "1.1.0.0/24" }`}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "DualStackAddressFamily",
		},

		{
			name: "excludes self-neighbor from per-node FRRConfiguration",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      types.ManagedBGPConfigurationName,
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1"}, // self for node
							{ASN: 1, Address: "10.0.0.2"}, // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					InternalIPs:              []string{"10.0.0.1", "fd00::1"},
					SubnetsAnnotation:        "{\"default\":\"1.1.0.0/24\"}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"192.0.2.1/24\"}",
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/" + types.ManagedBGPConfigurationName + "/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.2", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "preserves user-defined peers matching the primary address",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "user-defined",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1"}, // self for node
							{ASN: 1, Address: "10.0.0.2"}, // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					OmitPrimaryAddress:       false,
					InternalIPs:              []string{"10.0.0.1", "fd00::1"},
					SubnetsAnnotation:        "{\"default\":\"1.1.0.0/24\"}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"10.0.0.1/24\"}",
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/" + "user-defined" + "/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig:    "router bgp 1\n address-family ipv4 unicast\n  neighbor 10.0.0.1 allowas-in origin\n  neighbor 10.0.0.2 allowas-in origin\n exit-address-family\nexit\n!\n",
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1", Advertise: []string{"1.1.0.0/24"}},
							{ASN: 1, Address: "10.0.0.2", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "generates user-defined peering without the primary address annotation",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "user-defined",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1"}, // self for node
							{ASN: 1, Address: "10.0.0.2"}, // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					OmitPrimaryAddress:       true,
					SubnetsAnnotation:        "{\"default\":\"1.1.0.0/24\"}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"192.0.2.1/24\"}",
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/" + "user-defined" + "/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig:    "router bgp 1\n address-family ipv4 unicast\n  neighbor 10.0.0.1 allowas-in origin\n  neighbor 10.0.0.2 allowas-in origin\n exit-address-family\nexit\n!\n",
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1", Advertise: []string{"1.1.0.0/24"}},
							{ASN: 1, Address: "10.0.0.2", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "excludes self-neighbor with a non-canonical IPv6 address from per-node FRRConfiguration",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      types.ManagedBGPConfigurationName,
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "FD00:0:0:0:0:0:0:1"}, // self for node
							{ASN: 1, Address: "fd00::2"},            // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					InternalIPs:              []string{"10.0.0.1", "fd00::1"},
					SubnetsAnnotation:        "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"10.0.0.1/24\",\"ipv6\":\"fd00::1/64\"}",
				},
			},
			ipv6:                 true,
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/" + types.ManagedBGPConfigurationName + "/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "fd00::2", Advertise: []string{"fd01::/64"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile EVPN-enabled network to default VRF",
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
				{Name: "evpn-net", Namespace: "test", Network: util.GenerateCUDNNetworkName("evpn-net"),
					Topology: "layer2", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name: "rejects EVPN MAC-VRF network with a newline in the route target",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1\n"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid MAC-VRF route target",
		},
		{
			name: "rejects EVPN IP-VRF network with a VNI above the maximum",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer3", Subnet: "10.1.0.0/16/24", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: maxVNI + 1},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_red\":\"10.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid IP-VRF VNI",
		},
		{
			name: "rejects EVPN MAC-VRF network with a negative VNI",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: -1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid MAC-VRF VNI",
		},
		{
			name: "rejects EVPN network whose VRF name is not a valid interface name",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red\nx"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid VRF name",
		},
		{
			name: "rejects RouteAdvertisements with a newline in the target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red\nx", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red\nx", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectAcceptedMessageContains: "invalid TargetVRF",
		},
		{
			name: "rejects FRRConfiguration neighbor with a newline in the interface name",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "eth0\nx"},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "invalid neighbor",
		},
		{
			name: "reconciles EVPN MAC-VRF l2 network with a specific target VRF without a VRF router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles EVPN MAC-VRF l2 network with auto target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			fixtureID: "evpnAutoVRF",
			name:      "reconciles EVPN IP-VRF network with auto target and creates a router",
			ra:        &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			fixtureID: "evpnInheritedASN",
			name:      "reconciles EVPN IP-VRF with router ASN from another FRRConfiguration",
			ra:        &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfigGlobal",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
				{
					Name:      "frrConfigVRF",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65100, VRF: "blue"},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\",\"cluster_udn_blue\":\"10.2.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigGlobal/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
`,
					Routers: []*testRouter{
						{ASN: 65000, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigVRF/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `vrf blue
 vni 2000
exit-vrf
!
router bgp 65100 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65100, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile MACVRF EVPN without global router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name: "fails to reconcile IPVRF EVPN without global router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
		},
		{
			name: "fails to reconcile EVPN with global router but no neighbors",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "has no VRF matching",
		},
		{
			name: "reconciles EVPN when global router is in a different FRRConfiguration than VRF router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfigGlobal",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						// Global router with neighbors - provides ASN and neighbors for EVPN
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
				{
					Name:      "frrConfigVRF",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						// VRF-specific router - matches the target VRF
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigGlobal/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigVRF/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"10.1.0.0/16"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			fixtureID: "evpnUnderlay",
			name:      "advertises VTEP IP /32 in default-VRF router prefixes for EVPN IP-VRF",
			ra:        &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises dual-stack VTEP IPs to an unnumbered interface neighbor for EVPN IP-VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16", "fd64::/48"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1", "fd64::1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor enp4s0f0np0 activate
  neighbor enp4s0f0np0 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32", "fd64::1/128"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Interface: "enp4s0f0np0", Advertise: []string{"100.64.0.1/32", "fd64::1/128"},
								Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}, {Prefix: "fd64::/48", LE: 128, GE: 128}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises VTEP IP /128 in default-VRF router prefixes for IPv6 EVPN IP-VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "fd00::1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue6", Namespace: "blue6", Network: util.GenerateCUDNNetworkName("blue6"),
					Topology: "layer3", Subnet: "fd02::/48", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep6", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep6"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"fd64::/64"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue6\":\"fd02::100/64\"}", VTEPIPs: map[string][]string{"my-vtep6": {"fd64::1"}}}},
			reconcile:            "ra",
			ipv6:                 true,
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv6 unicast
  neighbor fd00::1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor fd00::1 activate
  neighbor fd00::1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue6
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue6
 address-family l2vpn evpn
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue6", Prefixes: []string{"fd02::/64"}},
						{ASN: 65000, Prefixes: []string{"fd64::1/128"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "fd00::1", Advertise: []string{"fd64::1/128"}, Receive: []testPrefixSelector{{Prefix: "fd64::/64", LE: 128, GE: 128}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue6": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile EVPN target VRF when cloned default-VRF neighbor has dual-stack address family set",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", DualStackAddressFamily: ptr.To(true)},
						}},
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionTrue,
			expectDegradedStatus:          metav1.ConditionTrue,
			expectDegradedMessageContains: "DualStackAddressFamily",
		},
		{
			name: "advertises VTEP IP /32 in default-VRF router prefixes for EVPN MAC-VRF with target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
router bgp 65000 vrf red
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"10.1.0.0/16"}},
						}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "deduplicates VTEP IPs when multiple EVPN networks reference the same VTEP",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "shared-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"),
					Topology: "layer3", Subnet: "10.3.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "shared-vtep", EVPNIPVRFVNI: 3000, EVPNIPVRFRouteTarget: "65000:3000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "shared-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\",\"cluster_udn_green\":\"10.3.1.0/24\"}", VTEPIPs: map[string][]string{"shared-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
vrf green
 vni 3000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
router bgp 65000 vrf green
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:3000
  route-target export 65000:3000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, VRF: "green", Prefixes: []string{"10.3.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"blue":  {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			fixtureID: "multiNodeVTEP",
			name:      "advertises VTEP IPs per node across multiple nodes",
			ra:        &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
				{Name: "node2", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.2.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.2"}}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.2.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.2/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.2/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises both VTEP IPs when EVPN networks reference different VTEPs",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "vtep-a", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"),
					Topology: "layer3", Subnet: "10.3.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "vtep-b", EVPNIPVRFVNI: 3000, EVPNIPVRFRouteTarget: "65000:3000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vtep-a"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vtep-b"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"200.10.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\",\"cluster_udn_green\":\"10.3.1.0/24\"}", VTEPIPs: map[string][]string{"vtep-a": {"100.64.0.1"}, "vtep-b": {"200.10.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
vrf green
 vni 3000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
router bgp 65000 vrf green
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:3000
  route-target export 65000:3000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, VRF: "green", Prefixes: []string{"10.3.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32", "200.10.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32", "200.10.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}, {Prefix: "200.10.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"blue":  {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "skips node without VTEP annotation and succeeds for node with annotation",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
				{Name: "node2", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.2.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.2.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile when VTEP annotation is malformed",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", RawVTEPAnnotation: ptr.To("not-json")},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectDegradedStatus: metav1.ConditionTrue,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: ""}},
		},
		{
			name:       "with dynamic UDN allocation, ignores nodes where the selected network is not allocated and advertises dual-stack subnets where it is",
			dynamicUDN: true,
			ipv6:       true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
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
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16,fd01:3::/48", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network and has subnets
				// allocated for it, "node2" does not and must not block the
				// RouteAdvertisements from being accepted, not even with its
				// stale empty subnet entry for the network
				{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"], \"cluster_udn_blue\":[\"1.3.0.0/24\",\"fd01:3::/64\"]}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":[\"1.1.1.0/24\",\"fd01:0:0:1::/64\"], \"cluster_udn_blue\":[]}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.3.0.0/24", "fd01:3::/64"}, Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
							{ASN: 1, Address: "fd02::ffff:100:64", Advertise: []string{"fd01:3::/64"}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			fixtureID:  "dynamicUDN",
			name:       "with dynamic UDN allocation and 'auto' target VRF, only requires matching the networks allocated on each node",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}, {Name: "red"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" is only active for network blue: with 'auto' target
				// VRF, network red not matching any router VRF on it must not
				// block the RouteAdvertisements from being accepted
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.3.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation, removes network configuration from nodes where it is no longer active even if still allocated",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				previousExport("node2", "1.3.1.0/24"),
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
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network; "node2" does not
				// but still has a subnet allocated for it, as during the
				// deletion grace period after its last workload left: only
				// "node" must be advertised
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.3.0.0/24"}, Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation and transit router, advertises an active layer2 network without a tunnel ID allocation and skips inactive nodes",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
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
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "green"}},
			pods:       []*testPod{{Name: "pod", Namespace: "green", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network; "node2" does not.
				// Neither has a tunnel ID allocation, and only "node" must be
				// advertised.
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.4.0.0/16"}, Imports: []string{"green"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
						{ASN: 1, VRF: "green", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation and 'auto' target VRF, skips FRRConfigurations that only match networks not allocated on the node",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig-blue",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:      "frrConfig-red",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}, {Name: "red"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" is only active for network blue: the source
				// FRRConfiguration carrying only network red's VRF must be
				// skipped for it instead of blocking the RouteAdvertisements
				// from being accepted
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectDegradedStatus: metav1.ConditionFalse,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-blue/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.3.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
	}
	fixtures := map[string]reconcileCase{}
	for _, tc := range tests {
		if tc.fixtureID != "" {
			fixtures[tc.fixtureID] = tc
		}
	}
	fixture := func(id string) reconcileCase {
		t.Helper()
		tc, ok := fixtures[id]
		if !ok {
			t.Fatalf("missing route advertisement test fixture %q", id)
		}
		return tc
	}
	// Seed generated configurations to verify that invalid updates remove
	// existing advertisements while preserving configurations for valid nodes.
	{
		base := fixture("multiNodeVTEP")
		movingEIP := base
		movingEIP.name = "removes moved EgressIP prefixes with unchanged networks while another node has invalid VTEP"
		movingRA := *base.ra
		movingRA.AdvertiseEgressIPs = true
		movingRA.TargetVRF = "blue"
		movingEIP.ra = &movingRA
		movingNAD := *base.nads[0]
		movingNAD.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: `["ra"]`}
		movingEIP.nads = []*testNAD{&movingNAD}
		movingEIP.nodes = append(slices.Clone(base.nodes), &testNode{
			Name: "node3", SubnetsAnnotation: `{"cluster_udn_blue":"10.2.3.0/24"}`, RawVTEPAnnotation: ptr.To("not-json"),
		})
		movingEIP.namespaces = []*testNamespace{{Name: "blue", Labels: map[string]string{"selected": "blue"}}}
		movingEIP.eips = []*testEIP{{Name: "eip", EIPs: map[string]string{"node2": "192.0.2.10"}, NamespaceSelector: map[string]string{"selected": "blue"}}}
		movingEIP.expectAcceptedStatus = metav1.ConditionFalse
		movingEIP.expectDegradedStatus = metav1.ConditionTrue
		movingEIP.expectAcceptedMessageContains = "node3"
		movingSource := *base.frrConfigs[0]
		movingSource.Routers = append(slices.Clone(movingSource.Routers), &testRouter{
			ASN: 65000, VRF: "blue", Neighbors: []*testNeighbor{{ASN: 65000, Address: "192.168.2.1"}},
		})
		movingEIP.frrConfigs = []*testFRRConfig{&movingSource}
		movingEIP.expectFRRConfigs = nil
		for i, generated := range base.expectFRRConfigs {
			buildRouters := func(ownsEgressIP bool) []*testRouter {
				routers := slices.Clone(generated.Routers)
				router := *routers[0]
				router.Prefixes = slices.Clone(router.Prefixes)
				if ownsEgressIP {
					router.Prefixes = append(router.Prefixes, "192.0.2.10/32")
				}
				router.Neighbors = []*testNeighbor{{ASN: 65000, Address: "192.168.2.1", Advertise: slices.Clone(router.Prefixes)}}
				routers[0] = &router
				return routers
			}
			// Both nodes keep their pod routes while the EgressIP moves from node1 to node2.
			node := generated.NodeSelector[corev1.LabelHostname]
			existing := *generated
			existing.Routers = buildRouters(node == "node1")
			existing.Name = fmt.Sprintf("%sexisting-%d", generateName, i)
			existing.Namespace = frrNamespace
			existing.Annotations = maps.Clone(generated.Annotations)
			existing.Annotations[types.OvnRouteAdvertisementsValidatedKey] = "true"
			movingEIP.frrConfigs = append(movingEIP.frrConfigs, &existing)
			expected := *generated
			expected.Routers = buildRouters(node == "node2")
			expected.RawConfig = strings.Replace(expected.RawConfig, "router bgp 65000 vrf blue\n", "router bgp 65000 vrf blue\n address-family ipv4 unicast\n  neighbor 192.168.2.1 allowas-in origin\n exit-address-family\n", 1)
			movingEIP.expectFRRConfigs = append(movingEIP.expectFRRConfigs, &expected)
		}
		tests = append(tests, movingEIP)
		invalidOther := base
		invalidOther.name = "advertises VTEPs when another RA has a malformed network selector"
		invalidOther.otherRAs = []*testRA{{Name: "other", AdvertisePods: true, NetworkSelector: map[string]string{"invalid key": "true"}}}
		tests = append(tests, invalidOther)
		for _, scenario := range []struct{ selected, established bool }{{}, {selected: true}, {selected: true, established: true}} {
			cross := base
			cross.name = fmt.Sprintf("cross RA different VTEPs selected=%t established=%t", scenario.selected, scenario.established)
			cross.nodes = nil
			for i, node := range base.nodes {
				copy := *node
				copy.VTEPIPs = maps.Clone(node.VTEPIPs)
				if i == 1 {
					copy.VTEPIPs["other-vtep"] = []string{"100.64.0.1"}
				}
				cross.nodes = append(cross.nodes, &copy)
			}
			otherNAD := *base.nads[0]
			otherNAD.Name, otherNAD.Namespace = "red", "red"
			otherNAD.Network = util.GenerateCUDNNetworkName("red")
			otherNAD.Labels = map[string]string{"selected": "other"}
			otherNAD.EVPNVTEPName = "other-vtep"
			cross.nads = append(slices.Clone(base.nads), &otherNAD)
			otherVTEP := base.vteps[0].DeepCopy()
			otherVTEP.Name = "other-vtep"
			cross.vteps = append(slices.Clone(base.vteps), otherVTEP)
			if scenario.selected {
				cross.otherRAs = []*testRA{{Name: "other", AdvertisePods: true, TargetVRF: "auto", NetworkSelector: map[string]string{"selected": "other"}}}
				cross.expectAcceptedStatus = metav1.ConditionFalse
				cross.expectDegradedStatus = metav1.ConditionTrue
				cross.expectAcceptedMessageContains = "no unique established advertiser"
				cross.expectFRRConfigs = base.expectFRRConfigs[1:]
				cross.expectNADAnnotations = base.expectNADAnnotations
			}
			if scenario.established {
				owner := &testFRRConfig{
					Name: generateName + "other-owner", Namespace: frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "other"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "other/frrConfig/node2", types.OvnRouteAdvertisementsValidatedKey: "true"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers:      []*testRouter{{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}}},
				}
				cross.frrConfigs = append(slices.Clone(base.frrConfigs), owner)
				cross.expectFRRConfigs = append(slices.Clone(cross.expectFRRConfigs), owner)
				cross.expectAcceptedMessageContains = "preserving established advertiser node2"
			}
			tests = append(tests, cross)
		}
		// A conflicting claim outside an EgressIP RA's node selection must
		// not suppress its selected node's VTEP advertisement.
		subset := base
		subset.name = "ignores VTEP collision outside RA node selection"
		ra := *base.ra
		ra.AdvertisePods = false
		ra.AdvertiseEgressIPs = true
		ra.TargetVRF = "blue"
		ra.NodeSelector = map[string]string{"selected": "true"}
		subset.ra = &ra
		source := *base.frrConfigs[0]
		source.Routers = append(slices.Clone(source.Routers), &testRouter{ASN: 65000, VRF: "blue"})
		subset.frrConfigs = []*testFRRConfig{&source}
		subset.nodes = nil
		for i, node := range base.nodes {
			copy := *node
			copy.RawVTEPAnnotation = ptr.To(`{"my-vtep":{"ips":["100.64.0.1"]}}`)
			if i == 0 {
				copy.Labels = map[string]string{"selected": "true"}
			}
			subset.nodes = append(subset.nodes, &copy)
		}
		subset.namespaces = []*testNamespace{{Name: "blue", Labels: map[string]string{"selected": "blue"}}}
		subset.eips = []*testEIP{{Name: "eip", EIPs: map[string]string{"node1": "192.0.2.1"}, NamespaceSelector: map[string]string{"selected": "blue"}}}
		expected := *base.expectFRRConfigs[0]
		expected.Routers = slices.Clone(expected.Routers)
		router := *expected.Routers[0]
		router.Prefixes = []string{"192.0.2.1/32"}
		expected.Routers[0] = &router
		subset.expectFRRConfigs = []*testFRRConfig{&expected}
		tests = append(tests, subset)
		crossSubset := subset
		crossSubset.name = "detects VTEP collision across disjoint EgressIP RA node selections"
		otherRA := *subset.ra
		otherRA.Name = "other"
		otherRA.NodeSelector = map[string]string{"selected": "false"}
		crossSubset.otherRAs = []*testRA{&otherRA}
		crossSubset.nodes = slices.Clone(subset.nodes)
		otherNode := *subset.nodes[1]
		otherNode.Labels = map[string]string{"selected": "false"}
		crossSubset.nodes[1] = &otherNode
		crossSubset.expectAcceptedStatus = metav1.ConditionFalse
		crossSubset.expectDegradedStatus = metav1.ConditionTrue
		crossSubset.expectAcceptedMessageContains = "no unique established advertiser"
		crossSubset.expectFRRConfigs = nil
		crossSubset.expectNADAnnotations = map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: ""}}
		tests = append(tests, crossSubset)
		for _, invalid := range []struct {
			fixtureID        string
			name             string
			annotation       string
			generationError  bool
			operationalError bool
			noExisting       bool
			ambiguous        bool
			unsafeInput      bool
		}{
			{fixtureID: "unsafeVTEP", name: "removes invalid VTEP configuration and unvalidated configurations on unsafe input", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`, unsafeInput: true},
			{name: "removes malformed VTEP advertisements", annotation: "not-json"},
			{fixtureID: "vtepLookupFailure", name: "removes invalid node configuration and retries operational generation failure", annotation: "not-json", operationalError: true},
			{name: "withdraws both invalid VTEP and unavailable node configurations", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`, generationError: true},
			{name: "removes extra same-family advertisements", annotation: `{"my-vtep":{"ips":["100.64.0.1","100.64.0.99"]}}`},
			{name: "removes invalid IP advertisements", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`},
			{name: "removes injected prefix advertisements", annotation: `{"my-vtep":{"ips":["100.64.0.1/24"]}}`},
			{name: "removes out-of-range advertisements", annotation: `{"my-vtep":{"ips":["192.0.2.1"]}}`},
			{name: "bootstrap collision excludes both nodes", annotation: `{"my-vtep":{"ips":["100.64.0.2"]}}`, noExisting: true},
			{name: "ambiguous established collision removes configurations for both nodes", annotation: `{"my-vtep":{"ips":["100.64.0.1"]}}`, ambiguous: true},
			{fixtureID: "duplicateVTEP", name: "preserves established advertiser on duplicate VTEP", annotation: `{"my-vtep":{"ips":["100.64.0.2"]}}`},
		} {
			tt := base
			tt.name = invalid.name
			tt.expectAcceptedStatus = metav1.ConditionFalse
			tt.expectDegradedStatus = metav1.ConditionTrue
			tt.expectNADAnnotations = map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: ""}}
			tt.nodes = nil
			for _, node := range base.nodes {
				copy := *node
				tt.nodes = append(tt.nodes, &copy)
			}
			tt.nodes[0].RawVTEPAnnotation = ptr.To(invalid.annotation)
			tt.expectFRRConfigs = base.expectFRRConfigs[1:]
			if invalid.operationalError {
				ra := *base.ra
				ra.AdvertiseEgressIPs = true
				ra.TargetVRF = "blue"
				tt.ra = &ra
				tt.eipListError = errors.New("EgressIP cache lookup failed")
				tt.wantErr = true
			}
			// Seed the references that existed before the failing update.
			if invalid.generationError || invalid.unsafeInput || invalid.operationalError || (!invalid.noExisting && !invalid.ambiguous) {
				tt.expectNADAnnotations = base.expectNADAnnotations
				if invalid.generationError {
					tt.nodes[1].SubnetsAnnotation = "not-json"
				}
				tt.expectFRRConfigs = base.expectFRRConfigs[1:]
				tt.nads = nil
				for _, nad := range base.nads {
					copy := *nad
					copy.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: `["ra"]`}
					tt.nads = append(tt.nads, &copy)
				}
			}
			if invalid.generationError {
				tt.expectFRRConfigs = nil
				tt.expectNADAnnotations = map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: `[]`}}
			}
			if strings.Contains(invalid.name, "preserves established advertiser") {
				tt.expectAcceptedMessageContains = "preserving established advertiser node2, excluding nodes [node1]"
			}
			if invalid.noExisting || invalid.ambiguous {
				tt.expectFRRConfigs = nil
				tt.expectAcceptedMessageContains = "no unique established advertiser"
			}
			if invalid.ambiguous {
				tt.nodes[1].RawVTEPAnnotation = ptr.To(`{"my-vtep":{"ips":["100.64.0.1"]}}`)
			}
			tt.frrConfigs = slices.Clone(base.frrConfigs)
			if invalid.unsafeInput {
				source := *base.frrConfigs[0]
				source.Routers = []*testRouter{{ASN: 65000, Neighbors: []*testNeighbor{{ASN: 65000, Interface: unsafeInputValue}}}}
				tt.frrConfigs[0] = &source
				unvalidated := *base.expectFRRConfigs[1]
				unvalidated.Name = generateName + "unvalidated"
				unvalidated.Namespace = frrNamespace
				unvalidated.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: "ra/old-source/node2"}
				tt.frrConfigs = append(tt.frrConfigs, &unvalidated)
				tt.expectAcceptedMessageContains = "invalid neighbor"
				tt.expectFRRConfigs = nil
				tt.expectNADAnnotations = map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: `[]`}}
			}
			for i, generated := range base.expectFRRConfigs {
				if invalid.noExisting {
					continue
				}
				copy := *generated
				if invalid.ambiguous && i == 1 {
					copy.Routers = slices.Clone(generated.Routers)
					router := *copy.Routers[1]
					router.Prefixes = []string{"100.64.0.1/32"}
					copy.Routers[1] = &router
				}
				copy.Annotations = maps.Clone(copy.Annotations)
				copy.Annotations[types.OvnRouteAdvertisementsValidatedKey] = "true"
				copy.Name = fmt.Sprintf("%sexisting-%d", generateName, i)
				copy.Namespace = frrNamespace
				tt.frrConfigs = append(tt.frrConfigs, &copy)
			}
			if invalid.fixtureID != "" {
				fixtures[invalid.fixtureID] = tt
			}
			tests = append(tests, tt)
		}
	}

	{
		base := fixture("dynamicUDN")
		for _, invalid := range []struct {
			name, message string
			neighbor      *testNeighbor
		}{
			{"dual-stack", "DualStackAddressFamily==true", &testNeighbor{ASN: 1, Address: "192.0.2.1", DualStackAddressFamily: ptr.To(true)}},
			{"unsafe interface", "invalid neighbor", &testNeighbor{ASN: 1, Interface: unsafeInputValue}},
			{"missing address and interface", "neither address nor interface", &testNeighbor{ASN: 1}},
		} {
			for _, scenario := range []string{"inactive network", "active network", "active network missing subnets", "network active only outside template nodes"} {
				tt := base
				tt.name = "dynamic neighbor validation: " + invalid.name + "/" + scenario
				source := *base.frrConfigs[0]
				source.Routers = slices.Clone(source.Routers)
				routerIndex := 0 // blue is active on node
				if scenario == "inactive network" {
					routerIndex = 1 // red is selected but inactive on every node
				}
				router := *source.Routers[routerIndex]
				router.Neighbors = []*testNeighbor{invalid.neighbor}
				source.Routers[routerIndex] = &router
				tt.frrConfigs = []*testFRRConfig{&source}
				switch scenario {
				case "network active only outside template nodes":
					source.Name = "inactive-template"
					source.NodeSelector = map[string]string{corev1.LabelHostname: "node2"}
					tt.frrConfigs = append(slices.Clone(base.frrConfigs), &source)
					node := *base.nodes[1]
					node.Labels = map[string]string{corev1.LabelHostname: "node2"}
					tt.nodes = []*testNode{base.nodes[0], &node}
				case "active network", "active network missing subnets":
					tt.expectAcceptedStatus = metav1.ConditionTrue
					tt.expectDegradedStatus = metav1.ConditionTrue
					tt.expectDegradedMessageContains = invalid.message
					tt.expectFRRConfigs = nil
					tt.expectNADAnnotations = nil
					if scenario == "active network missing subnets" {
						node := *base.nodes[0]
						node.SubnetsAnnotation = ""
						tt.nodes = []*testNode{&node, base.nodes[1]}
					}
				}
				tests = append(tests, tt)
			}
		}
	}

	{
		base := fixture("evpnUnderlay")
		invalid := base
		invalid.name = "invalid explicit VRF template leaves the underlay template's inherited ASN in use"
		invalid.frrConfigs = append(slices.Clone(base.frrConfigs), &testFRRConfig{
			Name: "invalid-explicit", Namespace: frrNamespace, Labels: base.frrConfigs[0].Labels,
			Routers: []*testRouter{{ASN: 65002, VRF: "blue", Neighbors: []*testNeighbor{{ASN: 65002, Interface: unsafeInputValue}}}},
		})
		invalid.expectDegradedStatus = metav1.ConditionTrue
		invalid.expectDegradedMessageContains = "invalid-explicit"
		tests = append(tests, invalid)
		for _, missingSubnets := range []bool{false, true} {
			tt := base
			tt.name = fmt.Sprintf("EVPN explicit target VRF fallback with missing subnets=%t", missingSubnets)
			ra := *base.ra
			ra.TargetVRF = "blue"
			tt.ra = &ra
			if missingSubnets {
				tt.nodes = append(slices.Clone(base.nodes), &testNode{Name: "node1"})
				tt.expectDegradedStatus = metav1.ConditionTrue
				tt.expectDegradedMessageContains = "node1"
			}
			tests = append(tests, tt)
		}
	}

	// Unused peers must not invalidate the generated configuration. Exercise
	// each target VRF mode with the same invalid inputs as the early validator.
	for _, base := range []reconcileCase{fixture("defaultVRF"), fixture("tenantVRF"), fixture("autoVRF"), fixture("dynamicUDN")} {
		for _, invalid := range []struct {
			name     string
			neighbor *testNeighbor
		}{
			{"dual-stack", &testNeighbor{ASN: 1, Address: "192.0.2.1", DualStackAddressFamily: ptr.To(true)}},
			{"unsafe interface", &testNeighbor{ASN: 1, Interface: unsafeInputValue}},
			{"missing address and interface", &testNeighbor{ASN: 1}},
		} {
			scopes := []string{"unrelated VRF", "unmatched template"}
			if base.ra.TargetVRF == "auto" && !base.ra.SelectsDefault {
				scopes = append(scopes, "unselected default VRF")
			}
			for _, scope := range scopes {
				tt := base
				tt.name += " ignores " + invalid.name + " peer in " + scope
				source := *base.frrConfigs[0]
				router := &testRouter{ASN: 1, VRF: "unrelated", Neighbors: []*testNeighbor{invalid.neighbor}}
				tt.frrConfigs = slices.Clone(base.frrConfigs)
				if scope != "unmatched template" {
					if scope == "unselected default VRF" {
						router.VRF = ""
					}
					source.Routers = append(slices.Clone(source.Routers), router)
					tt.frrConfigs[0] = &source
				} else {
					source.Name = "unmatched-template"
					source.NodeSelector = map[string]string{corev1.LabelHostname: "unselected-node"}
					router.VRF = base.frrConfigs[0].Routers[0].VRF
					source.Routers = []*testRouter{router}
					tt.frrConfigs = append(tt.frrConfigs, &source)
				}
				tests = append(tests, tt)
			}
		}
	}

	for _, base := range []reconcileCase{fixture("missingInternalIP"), fixture("evpnAutoVRF")} {
		tt := base
		tt.name = "rejects used dual-stack peers before subnet allocation: " + base.name
		source := *base.frrConfigs[0]
		router := *source.Routers[0]
		router.Neighbors = []*testNeighbor{{ASN: 1, Address: "192.0.2.1", DualStackAddressFamily: ptr.To(true)}}
		source.Routers = append([]*testRouter{&router}, source.Routers[1:]...)
		tt.frrConfigs = append([]*testFRRConfig{&source}, base.frrConfigs[1:]...)
		tt.nodes = nil
		for _, node := range base.nodes {
			copy := *node
			copy.SubnetsAnnotation = ""
			tt.nodes = append(tt.nodes, &copy)
		}
		tt.expectAcceptedStatus = metav1.ConditionTrue
		tt.expectDegradedStatus = metav1.ConditionTrue
		tt.expectDegradedMessageContains = "DualStackAddressFamily==true"
		tt.expectFRRConfigs = nil
		tt.expectNADAnnotations = nil
		tests = append(tests, tt)
	}

	// Hostname labels select the FRR daemon, but VTEP ownership and cleanup
	// must use the node API name stored in the generated annotation.
	for _, tt := range []reconcileCase{fixture("evpnAutoVRF"), fixture("duplicateVTEP"), fixture("vtepLookupFailure"), fixture("unsafeVTEP")} {
		tt.name += " with distinct hostname labels"
		tt.nodes = slices.Clone(tt.nodes)
		for i, node := range tt.nodes {
			copy := *node
			copy.Labels = map[string]string{}
			maps.Copy(copy.Labels, node.Labels)
			copy.Labels[corev1.LabelHostname] = "host-" + node.Name
			tt.nodes[i] = &copy
		}
		withHostnames := func(configs []*testFRRConfig) []*testFRRConfig {
			configs = slices.Clone(configs)
			for i, cfg := range configs {
				copy := *cfg
				copy.NodeSelector = maps.Clone(cfg.NodeSelector)
				if node := copy.NodeSelector[corev1.LabelHostname]; node != "" {
					copy.NodeSelector[corev1.LabelHostname] = "host-" + node
				}
				configs[i] = &copy
			}
			return configs
		}
		tt.frrConfigs = withHostnames(tt.frrConfigs)
		tt.expectFRRConfigs = withHostnames(tt.expectFRRConfigs)
		tests = append(tests, tt)
	}

	{
		tt := fixture("unsafeVTEP")
		tt.name = "cleans up deselected network with invalid VTEP and unsafe input"
		tt.nads = append(slices.Clone(tt.nads), &testNAD{
			Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: types.Layer3Topology, Subnet: "10.200.0.0/16",
			Annotations: map[string]string{types.OvnRouteAdvertisementsKey: `["ra","other"]`},
		})
		tt.expectNADAnnotations = maps.Clone(tt.expectNADAnnotations)
		tt.expectNADAnnotations["red"] = map[string]string{types.OvnRouteAdvertisementsKey: `["other"]`}
		tt.frrConfigs = slices.Clone(tt.frrConfigs)
		for i, cfg := range tt.frrConfigs {
			if cfg.Annotations[types.OvnRouteAdvertisementsValidatedKey] != "true" {
				continue
			}
			copy := *cfg
			copy.Routers = append(slices.Clone(cfg.Routers), &testRouter{ASN: 65000, VRF: "red", Prefixes: []string{"10.200.0.0/24"}})
			tt.frrConfigs[i] = &copy
		}
		tests = append(tests, tt)

		base := tt
		for _, invalid := range []struct {
			name, message string
			peer          *testNeighbor
		}{
			{"dual-stack peer", "DualStackAddressFamily==true", &testNeighbor{ASN: 65000, Address: "192.168.1.1", DualStackAddressFamily: ptr.To(true)}},
			{"peer without address or interface", "neither address nor interface", &testNeighbor{ASN: 65000}},
		} {
			tt := base
			tt.name = "cleans up deselected network with invalid VTEP and " + invalid.name
			tt.expectAcceptedMessageContains = invalid.message
			tt.frrConfigs = nil
			for _, cfg := range base.frrConfigs {
				if cfg.Name == generateName+"unvalidated" {
					// Only unsafe input requires deleting legacy configurations.
					continue
				}
				if cfg.Annotations[types.OvnRouteAdvertisementsKey] == "" {
					copy := *cfg
					copy.Routers = []*testRouter{{ASN: 65000, Neighbors: []*testNeighbor{invalid.peer}}}
					cfg = &copy
				}
				tt.frrConfigs = append(tt.frrConfigs, cfg)
			}
			tests = append(tests, tt)
		}
	}

	{
		tt := fixture("evpnInheritedASN")
		// The first node has both templates. Its peers cannot carry blue's routes for node2.
		split := tt
		split.name = "degrades an active EVPN IP-VRF whose default router selects another node"
		split.nodes = append(slices.Clone(tt.nodes), &testNode{Name: "node2", SubnetsAnnotation: `{"default":"1.1.1.0/24","cluster_udn_blue":"10.2.2.0/24"}`})
		global := *tt.frrConfigs[0]
		global.NodeSelector = map[string]string{corev1.LabelHostname: "node"}
		split.frrConfigs = []*testFRRConfig{&global, tt.frrConfigs[1]}
		tenant := *tt.expectFRRConfigs[1]
		tenant.NodeSelector = map[string]string{corev1.LabelHostname: "node2"}
		tenant.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigVRF/node2"}
		tenant.Routers = []*testRouter{{ASN: 65100, VRF: "blue", Prefixes: []string{"10.2.2.0/24"}}}
		split.expectFRRConfigs = append(slices.Clone(tt.expectFRRConfigs), &tenant)
		split.expectDegradedStatus = metav1.ConditionTrue
		split.expectDegradedMessageContains = `node "node2": configuration error: EVPN IP-VRF requires a default-VRF router with EVPN peers`
		tests = append(tests, split)
	}
	// Exercise EVPN coverage before prefixes are available, keeping the
	// expected configuration for the healthy node.
	for _, tt := range []reconcileCase{fixture("evpnAutoVRF"), fixture("evpnInheritedASN")} {
		tt.name += " with a node missing subnets"
		tt.nodes = append(slices.Clone(tt.nodes), &testNode{Name: "node1"})
		tt.expectDegradedStatus = metav1.ConditionTrue
		tt.expectDegradedMessageContains = "node1"
		tests = append(tests, tt)
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
			previousManagedBGP := config.ManagedBGP
			config.ManagedBGP.FRRNamespace = frrNamespace
			t.Cleanup(func() { config.ManagedBGP = previousManagedBGP })
			config.Default.Transport = tt.transport
			if tt.ipv6 {
				config.IPv6Mode = true
				defer func() { config.IPv6Mode = false }()
			}
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableEVPN = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = tt.dynamicUDN
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = tt.dynamicUDN
			// satisfy EVPN LGW restriction, otherwise no effect
			config.Gateway.Mode = config.GatewayModeLocal
			if tt.gatewayMode != "" {
				config.Gateway.Mode = tt.gatewayMode
			}

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)

			// create test objects
			if tt.ra != nil {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, otherRA := range tt.otherRAs {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), otherRA.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred(), "creating competing RouteAdvertisements %s", otherRA.Name)
			}

			for _, frrConfig := range tt.frrConfigs {
				_, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrConfig.Namespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			defaultNADName := config.Default.ClusterDefaultNetworkNAD
			var defaultNAD *nadtypes.NetworkAttachmentDefinition
			for _, nad := range tt.nads {
				n, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if nad.Name == defaultNADName.Name && nad.Namespace == defaultNADName.Namespace {
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

			for _, pod := range tt.pods {
				_, err := fakeClientset.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.Pod(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, eip := range tt.eips {
				_, err := fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), eip.EgressIP(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, vtep := range tt.vteps {
				_, err := fakeClientset.VTEPClient.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c := NewController(nm.Interface(), wf, fakeClientset)

			// prime the default network NAD namespace
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: config.Default.ClusterDefaultNetworkNAD.Namespace,
				},
			}
			_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// prime the default network NAD
			if defaultNAD == nil {
				defaultNAD, err = util.EnsureDefaultNetworkNAD(context.Background(), c.nadLister, c.nadClient)
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
			hasSynced := []cache.InformerSynced{
				wf.RouteAdvertisementsInformer().Informer().HasSynced,
				wf.FRRConfigurationsInformer().Informer().HasSynced,
				wf.NADInformer().Informer().HasSynced,
				wf.NodeCoreInformer().Informer().HasSynced,
				wf.EgressIPInformer().Informer().HasSynced,
			}
			if config.Gateway.Mode == config.GatewayModeLocal {
				hasSynced = append(hasSynced, wf.VTEPInformer().Informer().HasSynced)
			}
			cache.WaitForCacheSync(context.Background().Done(), hasSynced...)

			err = nm.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// we just need the inital sync
			nm.Stop()

			// Clean up condition metric timeseries from the global Prometheus registry
			// so they don't leak into subsequent subtests.
			t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition(tt.reconcile) })

			if tt.eipListError != nil {
				c.eipLister = failingEIPLister{EgressIPLister: c.eipLister, err: tt.eipListError}
			}
			reconcileErr := c.reconcile(context.Background(), tt.reconcile)
			if (reconcileErr != nil) != tt.wantErr {
				t.Fatalf("Controller.reconcile() error = %v, wantErr %v", reconcileErr, tt.wantErr)
			}
			if tt.eipListError != nil {
				g.Expect(errors.Is(reconcileErr, tt.eipListError)).To(gomega.BeTrue(), "operational cause must remain retryable")
				g.Expect(errors.Is(reconcileErr, errConfig)).To(gomega.BeFalse(), "validation must not mask the operational failure")
			}

			// verify RA status and condition metric are set as expected
			if tt.ra != nil && tt.eipListError == nil {
				ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), tt.reconcile, metav1.GetOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				accepted := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				g.Expect(accepted).NotTo(gomega.BeNil())
				g.Expect(accepted.Status).To(gomega.Equal(tt.expectAcceptedStatus), accepted.Message)
				if tt.expectAcceptedMessageContains != "" {
					g.Expect(accepted.Message).To(gomega.ContainSubstring(tt.expectAcceptedMessageContains))
				}
				degraded := meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded)
				g.Expect(degraded).NotTo(gomega.BeNil())
				g.Expect(degraded.Status).To(gomega.Equal(tt.expectDegradedStatus), "Degraded: %s", degraded.Message)
				if tt.expectDegradedMessageContains != "" {
					g.Expect(degraded.Message).To(gomega.ContainSubstring(tt.expectDegradedMessageContains))
				}
				// verify condition metric is set as expected
				expectedTrue := 0.0
				expectedFalse := 1.0
				if tt.expectAcceptedStatus == metav1.ConditionTrue {
					expectedTrue = 1.0
					expectedFalse = 0.0
				}
				valTrue, found := getRAConditionMetricValue(tt.reconcile, "Accepted", "true")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=true should exist")
				g.Expect(valTrue).To(gomega.Equal(expectedTrue))
				valFalse, found := getRAConditionMetricValue(tt.reconcile, "Accepted", "false")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(gomega.Equal(expectedFalse))
			} else {
				_, foundTrue := getRAConditionMetricValue(tt.reconcile, "Accepted", "true")
				_, foundFalse := getRAConditionMetricValue(tt.reconcile, "Accepted", "false")
				g.Expect(foundTrue || foundFalse).To(gomega.BeFalse(), "condition metrics should not exist for deleted RA")
			}

			// verify FRRConfigurations have been created/updated/deleted as expected
			actualFRRConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			var actualFRRConfigKeys []string
			actualFRRConfigLabels := map[string]map[string]string{}
			actualFRRConfigSpecs := map[string]*frrapi.FRRConfigurationSpec{}
			for _, frrConfig := range actualFRRConfigs.Items {
				if _, generated := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]; generated {
					g.Expect(frrConfig.Annotations).To(gomega.HaveKeyWithValue(types.OvnRouteAdvertisementsValidatedKey, "true"),
						"generated FRRConfiguration %s must carry the validated marker", frrConfig.Name)
					actualFRRConfigKeys = append(actualFRRConfigKeys, frrConfig.Annotations[types.OvnRouteAdvertisementsKey])
					// Preserved pre-upgrade fixtures may lack a node-watch label.
					// Every freshly generated configuration must have one.
					if _, labeled := frrConfig.Labels[types.OvnRouteAdvertisementsNodeKey]; labeled || frrConfig.GenerateName == generateName {
						key := strings.Split(frrConfig.Annotations[types.OvnRouteAdvertisementsKey], "/")
						g.Expect(key).To(gomega.HaveLen(3))
						g.Expect(frrConfig.Labels).To(gomega.HaveKeyWithValue(types.OvnRouteAdvertisementsNodeKey, util.RouteAdvertisementsNodeLabel(key[2])), "node watch must see %s", frrConfig.Name)
						delete(frrConfig.Labels, types.OvnRouteAdvertisementsNodeKey)
						delete(frrConfig.Labels, types.OvnRouteAdvertisementsUIDKey)
					}
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
					if v == "" {
						g.Expect(actualNADAnnotations[nad]).NotTo(gomega.HaveKey(k))
						continue
					}
					g.Expect(actualNADAnnotations[nad]).To(gomega.HaveKeyWithValue(k, v))
				}
			}
		})
	}
}

// frrReconcileTest runs reconciles explicitly against real informer caches.
// Mutations must reach those caches before the next reconcile uses them.
type frrReconcileTest struct {
	t             *testing.T
	g             *gomega.WithT
	fakeClientset *util.OVNClusterManagerClientset
	c             *Controller
	// failDelete makes deleting the FRRConfiguration "ovnk-generated-unvalidated"
	// fail, failNADPatch makes patching NADs fail
	failDelete, failNADPatch atomic.Bool
	// frrConfigNames returns the names of all FRRConfigurations
	frrConfigNames        func() []string
	defaultNADAnnotations func() map[string]string
}

const unsafeInputValue = "eth0\nx"

func newUnsafeInputTest(t *testing.T, g *gomega.WithT, generated ...*testFRRConfig) *frrReconcileTest {
	t.Helper()
	return newFRRReconcileTest(t, g, unsafeInputValue, generated...)
}

func newFRRReconcileTest(t *testing.T, g *gomega.WithT, peerInterface string, generated ...*testFRRConfig) *frrReconcileTest {
	t.Helper()
	previousDefault, previousFeatures, previousGateway := config.Default, config.OVNKubernetesFeature, config.Gateway
	t.Cleanup(func() {
		config.Default, config.OVNKubernetesFeature, config.Gateway = previousDefault, previousFeatures, previousGateway
	})
	frrNamespace := "frrNamespace"
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
			HostSubnetLength: 24,
		},
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEVPN = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Gateway.Mode = config.GatewayModeLocal

	tt := &frrReconcileTest{t: t, g: g, fakeClientset: util.GetOVNClientset().GetClusterManagerClientset()}
	fakeClientset := tt.fakeClientset
	addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)
	fakeClientset.FRRClient.(Fake).PrependReactor("delete", "frrconfigurations",
		func(action ctesting.Action) (bool, runtime.Object, error) {
			if tt.failDelete.Load() && action.(ctesting.DeleteAction).GetName() == "ovnk-generated-unvalidated" {
				return true, nil, errors.New("injected delete failure")
			}
			return false, nil, nil
		})
	fakeClientset.NetworkAttchDefClient.(Fake).PrependReactor("patch", "network-attachment-definitions",
		func(_ ctesting.Action) (bool, runtime.Object, error) {
			if tt.failNADPatch.Load() {
				return true, nil, errors.New("injected NAD patch failure")
			}
			return false, nil, nil
		})

	ra := &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	source := &testFRRConfig{
		Name:      "frrConfig",
		Namespace: frrNamespace,
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Interface: peerInterface},
			}},
		},
	}
	_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).Create(context.Background(), source.FRRConfiguration(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	for _, frrConfig := range generated {
		_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	node := &testNode{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: config.Default.ClusterDefaultNetworkNAD.Namespace}}, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tt.c = NewController(nm.Interface(), wf, fakeClientset)

	// prime the default network NAD, annotated as advertised by this
	// RouteAdvertisements and by another one
	defaultNAD, err := util.EnsureDefaultNetworkNAD(context.Background(), tt.c.nadLister, tt.c.nadClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defaultNAD.Annotations = map[string]string{
		types.OvnNetworkNameAnnotation:  types.DefaultNetworkName,
		types.OvnRouteAdvertisementsKey: "[\"ra\",\"other\"]",
	}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(wf.Shutdown)

	cache.WaitForCacheSync(
		context.Background().Done(),
		wf.RouteAdvertisementsInformer().Informer().HasSynced,
		wf.FRRConfigurationsInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
		wf.EgressIPInformer().Informer().HasSynced,
		wf.NamespaceInformer().Informer().HasSynced,
	)

	t.Cleanup(func() { metrics.DeleteRouteAdvertisementMetrics("ra") })

	tt.frrConfigNames = func() []string {
		frrConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		var names []string
		for _, frrConfig := range frrConfigs.Items {
			names = append(names, frrConfig.Name)
		}
		return names
	}
	tt.defaultNADAnnotations = func() map[string]string {
		nad, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Get(context.Background(), defaultNAD.Name, metav1.GetOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return nad.Annotations
	}
	return tt
}

func (tt *frrReconcileTest) generatedConfigs(ra string) map[string]*frrapi.FRRConfiguration {
	tt.t.Helper()
	list, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace").List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{types.OvnRouteAdvertisementsKey: ra}).String(),
	})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "list generated configurations for RA %s", ra)
	configs := map[string]*frrapi.FRRConfiguration{}
	for i := range list.Items {
		cfg := &list.Items[i]
		key := cfg.Annotations[types.OvnRouteAdvertisementsKey]
		tt.g.Expect(key).NotTo(gomega.BeEmpty(), "configuration %s must identify its RA/template/node key", cfg.Name)
		tt.g.Expect(configs).NotTo(gomega.HaveKey(key), "configuration %s duplicates key %s", cfg.Name, key)
		configs[key] = cfg
	}
	return configs
}

// Compare objects, not just counts: a replacement can leave the count unchanged
// while the cache still contains the deleted object.
func (tt *frrReconcileTest) waitForFRRCache() {
	tt.t.Helper()
	list, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "list API configurations before waiting for the FRR cache")
	want := make([]*frrapi.FRRConfiguration, len(list.Items))
	for i := range list.Items {
		want[i] = &list.Items[i]
	}
	tt.g.Eventually(func() ([]*frrapi.FRRConfiguration, error) {
		return tt.c.frrLister.List(labels.Everything())
	}, 5*time.Second).Should(gomega.ConsistOf(want), "FRR cache must contain the current API objects")
}

func (tt *frrReconcileTest) setDefaultNADReferences(refs string) {
	tt.t.Helper()
	key := config.Default.ClusterDefaultNetworkNAD
	k := kube.KubeOVN{NADClient: tt.c.nadClient}
	err := k.SetAnnotationsOnNAD(tt.t.Context(), key.Namespace, key.Name, map[string]string{types.OvnRouteAdvertisementsKey: refs}, fieldManager)
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "seed default NAD references")
	tt.waitForDefaultNADCache()
}

func (tt *frrReconcileTest) waitForDefaultNADCache() {
	tt.t.Helper()
	key := config.Default.ClusterDefaultNetworkNAD
	current, err := tt.c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(key.Namespace).Get(tt.t.Context(), key.Name, metav1.GetOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "read the default network's NAD")
	tt.g.Eventually(func() (*nadtypes.NetworkAttachmentDefinition, error) {
		return tt.c.nadLister.NetworkAttachmentDefinitions(key.Namespace).Get(key.Name)
	}, 5*time.Second).Should(gomega.Equal(current), "observe the latest default network intent")
}

func (tt *frrReconcileTest) frrCacheWithout(name string) cache.Indexer {
	tt.t.Helper()
	stale := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	configs, err := tt.c.frrLister.List(labels.Everything())
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "construct the lagging FRR cache")
	for _, cfg := range configs {
		if cfg.Name != name {
			tt.g.Expect(stale.Add(cfg)).To(gomega.Succeed(), "cache configuration %s", cfg.Name)
		}
	}
	return stale
}

func (tt *frrReconcileTest) updateNodeSubnets(name, subnets string) {
	tt.t.Helper()
	node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "get node %s before updating subnets", name)
	node.Annotations[types.NodeSubnetsAnnotation] = subnets
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "update subnets on node %s", name)
	tt.g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(name) }, 5*time.Second).Should(gomega.Equal(node), "cache must observe new subnets on node %s", name)
}

func (tt *frrReconcileTest) updateRA(ra *ratypes.RouteAdvertisements) {
	tt.t.Helper()
	updated, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(context.Background(), ra, metav1.UpdateOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "update RA %s", ra.Name)
	tt.g.Eventually(func() (*ratypes.RouteAdvertisements, error) {
		return tt.c.raLister.Get(ra.Name)
	}, 5*time.Second).Should(gomega.Equal(updated), "cache must observe the update to RA %s", ra.Name)
}

func (tt *frrReconcileTest) expectRAStatus(accepted, degraded bool) *ratypes.RouteAdvertisements {
	tt.t.Helper()
	ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), "ra", metav1.GetOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "get RA status after reconciliation")
	for name, want := range map[string]bool{conditionTypeAccepted: accepted, conditionTypeDegraded: degraded} {
		condition := meta.FindStatusCondition(ra.Status.Conditions, name)
		tt.g.Expect(condition).NotTo(gomega.BeNil(), "RA %s must report condition %s", ra.Name, name)
		status := metav1.ConditionFalse
		if want {
			status = metav1.ConditionTrue
		}
		tt.g.Expect(condition.Status).To(gomega.Equal(status), "RA %s condition %s: %s", ra.Name, name, condition.Message)
		tt.g.Expect(condition.ObservedGeneration).To(gomega.Equal(ra.Generation), "RA %s condition %s must reflect the current generation", ra.Name, name)
	}
	return ra
}

func (tt *frrReconcileTest) addNetwork(nm *networkmanager.FakeNetworkManager, nad *testNAD) {
	tt.t.Helper()
	object := nad.NAD()
	network, err := util.ParseNADInfo(object)
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "parse network from NAD %s/%s", nad.Namespace, nad.Name)
	nm.PrimaryNetworks[nad.Namespace] = network
	_, err = tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), object, metav1.CreateOptions{})
	tt.g.Expect(err).NotTo(gomega.HaveOccurred(), "create NAD %s/%s", nad.Namespace, nad.Name)
	tt.g.Eventually(func() (*nadtypes.NetworkAttachmentDefinition, error) {
		return tt.c.nadLister.NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
	}, 5*time.Second).Should(gomega.Equal(object), "cache must observe NAD %s/%s", nad.Namespace, nad.Name)
}

// generatedFRRConfig returns an FRRConfiguration generated for the test
// RouteAdvertisements on the given node, carrying the validated marker
// annotation or not.
func generatedFRRConfig(name, node string, validated bool) *testFRRConfig {
	annotations := map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/" + node}
	if validated {
		annotations[types.OvnRouteAdvertisementsValidatedKey] = "true"
	}
	return &testFRRConfig{
		Name:         name,
		Namespace:    "frrNamespace",
		Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
		Annotations:  annotations,
		NodeSelector: map[string]string{"kubernetes.io/hostname": node},
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Interface: "eth0", Advertise: []string{"1.1.0.0/24"}},
			}},
		},
		RawConfig: "router bgp 1\n address-family ipv4 unicast\n  neighbor eth0 allowas-in origin\n exit-address-family\nexit\n!\n",
	}
}

// A bad peer withdraws its old configurations. If cleanup and NAD publication
// both fail, the retry must retain both diagnostics and eventually publish intent.
func TestController_reconcileDeletesFRRConfigurationsOnUnsafeInput(t *testing.T) {
	g := gomega.NewWithT(t)
	tt := newUnsafeInputTest(t, g,
		generatedFRRConfig("ovnk-generated-unvalidated", "node2", false),
		generatedFRRConfig("ovnk-generated-validated", "node", true),
	)

	tt.setDefaultNADReferences(`["other"]`)
	tt.failDelete.Store(true)
	tt.failNADPatch.Store(true)
	err := tt.c.reconcile(context.Background(), "ra")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("injected delete failure"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("ovnk-generated-unvalidated"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid neighbor"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("injected NAD patch failure"))
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", "ovnk-generated-unvalidated"))
	g.Expect(tt.defaultNADAnnotations()).NotTo(gomega.HaveKey(types.OvnRouteAdvertisementsIntentKey), "the rejected patch cannot publish intent")

	// deletion succeeds on retry but publishing the intent on the NAD
	// fails: the error is returned for a retry and names the validation
	// reason, the generated FRRConfigurations are gone
	tt.failDelete.Store(false)
	err = tt.c.reconcile(context.Background(), "ra")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("injected NAD patch failure"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid neighbor"))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "a failed NAD update must be retried")
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig"))
	annotations := tt.defaultNADAnnotations()
	g.Expect(annotations).NotTo(gomega.HaveKey(types.OvnRouteAdvertisementsIntentKey), "withdrawal alone cannot publish intent")
	g.Expect(annotations[types.OvnRouteAdvertisementsKey]).To(gomega.MatchJSON(`["other"]`), "retain the existing RA reference while the patch is retried")

	tt.failNADPatch.Store(false)
	g.Expect(tt.c.reconcile(context.Background(), "ra")).To(gomega.Succeed())
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig"))
	annotations = tt.defaultNADAnnotations()
	g.Expect(annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"targetVRF":""}}`), "publish the selected network's intent on retry")
	refs, _, err := parseNADAdvertisements(annotations)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "read the updated RA references")
	g.Expect(refs).To(gomega.ConsistOf("ra", "other"), "add the new reference without removing the other RA")
	status := tt.expectRAStatus(true, true)
	g.Expect(meta.FindStatusCondition(status.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("invalid neighbor"))
}

// TestController_reconcileOnNetworkActivity verifies that the controller
// reconciles on network activity changes notified by the network manager: a
// network going active on a node that still has its subnet allocated, as
// after a pod is recreated during the deletion grace period, updates no
// object the controller watches.
func TestController_reconcileOnNetworkActivity(t *testing.T) {
	g := gomega.NewWithT(t)

	frrNamespace := "frrNamespace"
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
			HostSubnetLength: 24,
		},
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEVPN = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.Gateway.Mode = config.GatewayModeLocal

	fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
	addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)

	ra := &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	frrConfig := &testFRRConfig{
		Name:      "frrConfig",
		Namespace: frrNamespace,
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Address: "1.0.0.100"},
			}},
		},
	}
	_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrConfig.Namespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nad := &testNAD{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// the node still has a subnet allocated for the network, as during the
	// deletion grace period, but no workloads attached to it
	node := &testNode{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	for _, namespace := range []string{"blue", config.Default.ClusterDefaultNetworkNAD.Namespace} {
		_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
	g.Expect(err).ToNot(gomega.HaveOccurred())

	c := NewController(nm.Interface(), wf, fakeClientset)

	// prime the default network NAD
	defaultNAD, err := util.EnsureDefaultNetworkNAD(context.Background(), c.nadLister, c.nadClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defaultNAD.Annotations = map[string]string{types.OvnNetworkNameAnnotation: types.DefaultNetworkName}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	cache.WaitForCacheSync(
		context.Background().Done(),
		wf.RouteAdvertisementsInformer().Informer().HasSynced,
		wf.FRRConfigurationsInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
		wf.EgressIPInformer().Informer().HasSynced,
	)

	// keep the network manager running so that pod events reach its trackers
	err = nm.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer nm.Stop()

	err = c.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer c.Stop()

	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition("ra") })

	generatedFRRConfigs := func() []string {
		frrConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		var generated []string
		for _, frrConfig := range frrConfigs.Items {
			if key, ok := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]; ok {
				generated = append(generated, key)
			}
		}
		return generated
	}

	// the network is not active on the node: nothing is advertised
	raAccepted := func() bool {
		ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), "ra", metav1.GetOptions{})
		if err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(ra.Status.Conditions, "Accepted")
	}
	g.Eventually(raAccepted, 5*time.Second).Should(gomega.BeTrue())
	g.Expect(generatedFRRConfigs()).To(gomega.BeEmpty())

	// the initial reconcile cached which networks the RouteAdvertisements
	// selects
	networkName := util.GenerateCUDNNetworkName("blue")
	g.Expect(c.getRAsForNetwork(networkName)).To(gomega.ConsistOf("ra"))
	g.Expect(c.getRAsForNetwork(util.GenerateCUDNNetworkName("red"))).To(gomega.BeEmpty())

	// scheduling a pod on the node activates the network there without
	// updating any object the controller watches: the network manager
	// notification must trigger the reconcile that advertises the node
	pod := &testPod{Name: "pod", Namespace: "blue", Node: "node"}
	_, err = fakeClientset.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.Pod(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	g.Eventually(generatedFRRConfigs, 5*time.Second).Should(gomega.ConsistOf("ra/frrConfig/node"))

	// deleting the RouteAdvertisements removes it from the cache
	err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Delete(context.Background(), "ra", metav1.DeleteOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() []string { return c.getRAsForNetwork(networkName) }, 5*time.Second).Should(gomega.BeEmpty())
}

// TestCrossRASelectionEvents checks that claims are reevaluated on RA changes
// without relying on a generated FRRConfiguration event to wake other RAs.
func TestCrossRASelectionEvents(t *testing.T) {
	g := gomega.NewWithT(t)
	oldFeatures, oldMode := config.OVNKubernetesFeature, config.Gateway.Mode
	t.Cleanup(func() { config.OVNKubernetesFeature, config.Gateway.Mode = oldFeatures, oldMode })
	config.Gateway.Mode = config.GatewayModeLocal
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEVPN = true
	clients := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(clients)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating the watch factory")
	g.Expect(wf.Start()).To(gomega.Succeed(), "starting informers for RA selection events")
	t.Cleanup(wf.Shutdown)
	var mu sync.Mutex
	seen := sets.New[string]()
	c := &Controller{}
	c.raController = controllerutil.NewController("cross RA events", &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsUpdate,
		OnDelete:       c.onRADelete,
		Reconcile: func(_ context.Context, name string) error {
			mu.Lock()
			defer mu.Unlock()
			seen.Insert(name)
			return nil
		},
	})
	g.Expect(controllerutil.Start(c.raController)).To(gomega.Succeed(), "starting the RA event controller")
	t.Cleanup(func() { controllerutil.Stop(c.raController) })
	readSeen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return sets.List(seen)
	}
	reset := func() {
		mu.Lock()
		defer mu.Unlock()
		seen = sets.New[string]()
	}
	client := clients.RouteAdvertisementsClient.K8sV1().RouteAdvertisements()
	_, err = client.Create(context.Background(), (&testRA{Name: "a"}).RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating initial RouteAdvertisements a")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a"}), "initial RA creation must enqueue reconciliation")
	reset()
	b, err := client.Create(context.Background(), (&testRA{Name: "b"}).RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating competing RouteAdvertisements b")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "new claims must wake existing RAs")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.Equal([]string{"a", "b"}), "both affected RAs must remain in the observed reconciliation set")
	reset()
	b.Generation++ // The fake client does not increment generation for spec changes.
	b.Spec.NodeSelector.MatchLabels = map[string]string{"selected": "true"}
	b, err = client.Update(context.Background(), b, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "changing RouteAdvertisements b node selection")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "selection changes must wake other RAs")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.Equal([]string{"a", "b"}), "both affected RAs must remain in the observed reconciliation set")
	reset()
	b.Status.Status = "Accepted"
	b.Status.Conditions = []metav1.Condition{{Type: conditionTypeDegraded, Status: metav1.ConditionTrue, Reason: "NodeConfigurationFailed"}}
	b, err = client.UpdateStatus(context.Background(), b, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish b's degraded condition")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"b"}), "only b needs to recheck its condition")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.Equal([]string{"b"}), "a condition change cannot affect other EVPN claims")
	reset()
	b.Status.Conditions[0].Message = "the template is still invalid"
	_, err = client.UpdateStatus(context.Background(), b, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "update b's diagnostics")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.BeEmpty(), "a new diagnostic with the same condition status must not bypass backoff")
	g.Expect(client.Delete(context.Background(), "b", metav1.DeleteOptions{})).To(gomega.Succeed(), "deleting the competing RA")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "deleting a claim must wake surviving RAs")
}

// TestUpdates verifies which informer changes enqueue dependent RAs and which
// controller-owned or irrelevant updates must remain quiet.
func TestUpdates(t *testing.T) {
	oldFeatures, oldMode := config.OVNKubernetesFeature, config.Gateway.Mode
	t.Cleanup(func() { config.OVNKubernetesFeature, config.Gateway.Mode = oldFeatures, oldMode })
	config.Gateway.Mode = config.GatewayModeLocal
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEVPN = true
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
		initialReconciles []string
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
			name:              "reconciles all RAs on own generated FRRConfig update",
			oldObject:         &testFRRConfig{Name: generateName + "owner", Generation: 1},
			newObject:         &testFRRConfig{Name: generateName + "owner", Generation: 2, OwnUpdate: true},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:      "ignores status-only own generated FRRConfig update",
			oldObject: &testFRRConfig{Name: generateName + "owner", Generation: 1},
			newObject: &testFRRConfig{Name: generateName + "owner", Generation: 1, OwnUpdate: true},
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
			initialReconciles: []string{"ra2"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles a deleted RA referenced from NAD",
			newObject:         &testNAD{Name: "net", Namespace: "net", Network: "net", Topology: "layer3", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra4\"]"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3", "ra4"},
		},
		{
			name:              "reconciles only the newly referenced RA on own NAD update",
			oldObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			newObject:         &testNAD{Name: "net", Namespace: "net", OwnUpdate: true, Labels: map[string]string{"select": "2"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
			expectedReconcile: []string{"ra2"},
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
			name:              "reconciles all RAs on updated Node InternalIP addresses",
			oldObject:         &testNode{Name: "node"},
			newObject:         &testNode{Name: "node", InternalIPs: []string{"192.0.2.1"}},
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
			name:      "ignores updated Node primary address annotation",
			oldObject: &testNode{Name: "eip", PrimaryAddressAnnotation: "old"},
			newObject: &testNode{Name: "eip", PrimaryAddressAnnotation: "new"},
		},
		{
			name:              "reconciles all RAs when the Node becomes a DPU host",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", DPUHostAddressAnnotation: `{"ipv4":"172.18.255.254/16"}`},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when the Node stops being a DPU host",
			oldObject:         &testNode{Name: "eip", DPUHostAddressAnnotation: `{"ipv4":"172.18.255.254/16"}`},
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name: "reconciles all RAs on updated Node L3 gateway annotation",
			oldObject: &testNode{
				Name:                      "eip",
				L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16"],"next-hops":["172.18.0.1"]}}`,
			},
			newObject: &testNode{
				Name:                      "eip",
				L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:d4:ac:00","ip-addresses":["172.18.255.253/16"],"next-hops":["172.18.0.1"]}}`,
			},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node chassis ID annotation",
			oldObject:         &testNode{Name: "eip", ChassisIDAnnotation: "old"},
			newObject:         &testNode{Name: "eip", ChassisIDAnnotation: "new"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on new Node VTEP annotation",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on removed Node VTEP annotation",
			oldObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on changed Node VTEP annotation",
			oldObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			newObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.2"}}},
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

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			reconciled := []string{}
			reconciledMutex := sync.Mutex{}
			reconcile := func(_ context.Context, ra string) error {
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
				ObjNeedsUpdate: raSelectionChanged,
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

			// A startup event can reference an RA that no longer exists.
			g.Eventually(matchReconciledRAs, 5*time.Second).WithArguments(tt.initialReconciles).Should(gomega.Succeed())
			g.Consistently(matchReconciledRAs, 100*time.Millisecond).WithArguments(tt.initialReconciles).Should(gomega.Succeed())
			resetReconciles()

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

func getRAConditionMetricValue(nameLabel, conditionLabel, statusLabel string) (float64, bool) {
	metricName := prometheus.BuildFQName(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemClusterManager, "route_advertisement_condition")
	return ovntest.GetConditionMetricValue(metricName, nameLabel, conditionLabel, statusLabel)
}

// TestController_updateRAStatus verifies that the 'Accepted' condition is
// refreshed when consecutive reconciles fail for different reasons, so that
// the status message never gets stale.
func TestController_updateRAStatus(t *testing.T) {
	g := gomega.NewWithT(t)

	fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
	c := &Controller{raClient: fakeClientset.RouteAdvertisementsClient}

	raName := "ra"
	tra := &testRA{Name: raName}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition(raName) })

	getRA := func() *ratypes.RouteAdvertisements {
		ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), raName, metav1.GetOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return ra
	}

	// first reconcile fails with error A
	err = c.updateRAStatus(context.Background(), getRA(), false, frrGenerationResult{err: fmt.Errorf("%w: error A", errConfig)})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "first reconcile with error A should update the status")
	accepted := meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should be set after the first reconcile")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should be false after error A")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"), "errConfig should be reported as ConfigurationError")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error A"), "the message should report error A")

	// rewind the condition transition time one hour into the past, so we can
	// tell whether subsequent refreshes preserve or bump it: condition
	// timestamps have one-second resolution and the whole test runs within a
	// single second, so against a "now" timestamp an unchanged and a wrongly
	// re-stamped transition time would look identical
	rewound := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	ra := getRA()
	meta.FindStatusCondition(ra.Status.Conditions, "Accepted").LastTransitionTime = rewound
	_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "rewinding the condition transition time should succeed")

	// second reconcile fails with error B for the same reason: the message
	// must be refreshed
	err = c.updateRAStatus(context.Background(), getRA(), false, frrGenerationResult{err: fmt.Errorf("%w: error B", errConfig)})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "second reconcile with error B should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after error B")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should stay false after error B")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"), "errConfig should still be reported as ConfigurationError")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error B"), "the message should be refreshed to error B")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a message-only refresh should preserve the transition time")

	// third reconcile fails with a different reason: reason and message must
	// be refreshed
	err = c.updateRAStatus(context.Background(), getRA(), false, frrGenerationResult{err: fmt.Errorf("%w: error C", errPending)})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "third reconcile with error C should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after error C")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should stay false after error C")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationPending"), "errPending should be reported as ConfigurationPending")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error C"), "the message should be refreshed to error C")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a reason/message refresh should preserve the transition time")

	// fourth reconcile fails with the same error: the status must not be
	// applied again
	countStatusPatches := func() int {
		patches := 0
		for _, action := range fakeClientset.RouteAdvertisementsClient.(*rafake.Clientset).Actions() {
			if action.GetVerb() == "patch" {
				patches++
			}
		}
		return patches
	}
	patches := countStatusPatches()
	err = c.updateRAStatus(context.Background(), getRA(), false, frrGenerationResult{err: fmt.Errorf("%w: error C", errPending)})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a reconcile with an unchanged error should succeed")
	g.Expect(countStatusPatches()).To(gomega.Equal(patches), "an unchanged status must not be re-applied")

	// fifth reconcile fails with the same error but had FRRConfig/NAD updates:
	// the status must be applied even though the condition is unchanged
	err = c.updateRAStatus(context.Background(), getRA(), true, frrGenerationResult{err: fmt.Errorf("%w: error C", errPending)})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a reconcile with updates should update the status")
	g.Expect(countStatusPatches()).To(gomega.Equal(patches+1), "hadUpdates should apply the status even when the condition is unchanged")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a hadUpdates apply should preserve the transition time of an unchanged condition")

	// sixth reconcile succeeds: the condition status flips and the transition
	// time must be bumped
	err = c.updateRAStatus(context.Background(), getRA(), false, frrGenerationResult{})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a successful reconcile should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after a successful reconcile")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionTrue), "the Accepted condition should be true after a successful reconcile")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally(">", rewound.Time), "a status flip should bump the transition time")
}

// TestManagedPeeringMissingInternalIPs verifies healthy nodes reconcile while
// missing addresses are retried, and generation resumes when addresses return.
func TestManagedPeeringMissingInternalIPs(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	previousManaged := config.ManagedBGP
	config.ManagedBGP.FRRNamespace = "frrNamespace"
	t.Cleanup(func() { config.ManagedBGP = previousManaged })
	tt := newUnsafeInputTest(t, g)
	client := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace")
	g.Expect(client.Delete(ctx, "frrConfig", metav1.DeleteOptions{})).To(gomega.Succeed(), "remove unsafe fixture template")
	source := (&testFRRConfig{Name: types.ManagedBGPConfigurationName, Namespace: "frrNamespace", Routers: []*testRouter{
		{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Address: "10.0.0.1"}, {ASN: 1, Address: "10.0.0.2"}}},
	}}).FRRConfiguration()
	_, err := client.Create(ctx, source, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create managed template")
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, (&testNode{Name: "node1", InternalIPs: []string{"10.0.0.2"}, SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}).Node(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create healthy node")
	g.Eventually(func() bool {
		_, oldErr := tt.c.frrLister.FRRConfigurations("frrNamespace").Get("frrConfig")
		_, newErr := tt.c.frrLister.FRRConfigurations("frrNamespace").Get(source.Name)
		_, nodeErr := tt.c.nodeLister.Get("node1")
		return apierrors.IsNotFound(oldErr) && newErr == nil && nodeErr == nil
	}, 5*time.Second).Should(gomega.BeTrue(), "observe managed template and healthy node")
	err = tt.c.reconcile(context.Background(), "ra")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("waiting for InternalIP addresses")))
	g.Expect(errors.Is(err, errPending)).To(gomega.BeFalse(), "missing addresses must not enter pending cleanup")
	// The queue reports the last error after exhausting retries. Node status
	// was already published and must remain accepted/degraded at that boundary.
	g.Expect(tt.c.handleReconcileError(ctx, "ra", err)).To(gomega.Succeed())
	ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "read initial status")
	g.Expect(meta.IsStatusConditionTrue(ra.Status.Conditions, "Accepted")).To(gomega.BeTrue(), "valid input remains accepted")
	g.Expect(meta.IsStatusConditionTrue(ra.Status.Conditions, "Degraded")).To(gomega.BeTrue(), "report incomplete node generation")
	g.Expect(slices.Collect(maps.Keys(tt.generatedConfigs("ra")))).To(gomega.ConsistOf("ra/"+source.Name+"/node1"), "publish only the healthy node before the missing InternalIP arrives")
	setAddresses := func(addresses []corev1.NodeAddress) {
		t.Helper()
		node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read node before address change")
		node.Status.Addresses = addresses
		_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "update node InternalIPs")
		g.Eventually(func() []corev1.NodeAddress {
			cached, err := tt.c.nodeLister.Get("node")
			if err != nil {
				return nil
			}
			return cached.Status.Addresses
		}, 5*time.Second).Should(gomega.Equal(addresses), "observe address update")
	}
	addresses := []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}
	setAddresses(addresses)
	tt.waitForFRRCache()
	g.Expect(tt.c.reconcile(context.Background(), "ra")).To(gomega.Succeed(), "generate both nodes after addresses appear")
	before, err := client.List(ctx, metav1.ListOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "snapshot node configurations")
	g.Expect(before.Items).To(gomega.HaveLen(3), "template and two generated configurations")
	ra, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "snapshot accepted status")
	g.Expect(meta.IsStatusConditionTrue(ra.Status.Conditions, "Accepted")).To(gomega.BeTrue())
	nadBefore := tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]
	setAddresses(nil)
	for i := 0; i < 2; i++ {
		g.Expect(tt.c.reconcile(context.Background(), "ra")).To(gomega.MatchError(gomega.ContainSubstring("waiting for InternalIP addresses")), "retry %d", i)
		after, err := client.List(ctx, metav1.ListOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read configurations after failed generation")
		expected := slices.DeleteFunc(slices.Clone(before.Items), func(cfg frrapi.FRRConfiguration) bool {
			return cfg.Annotations[types.OvnRouteAdvertisementsKey] == "ra/"+types.ManagedBGPConfigurationName+"/node"
		})
		g.Expect(after.Items).To(gomega.ConsistOf(expected), "withdraw only the addressless node's configuration")
		current, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read retained status")
		g.Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Accepted")).To(gomega.BeTrue(), "valid input remains accepted")
		g.Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Degraded")).To(gomega.BeTrue(), "report incomplete node generation")
		g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(nadBefore), "preserve NAD references")
	}
	// Change the prefixes too, proving recovery performs a new configuration update.
	node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "read recovering node")
	node.Annotations["k8s.ovn.org/node-subnets"] = `{"default":"1.1.2.0/24"}`
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "update recovering node subnet")
	setAddresses(addresses)
	g.Eventually(func() error {
		if err := tt.c.reconcile(context.Background(), "ra"); err != nil {
			return err
		}
		configs, err := client.List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, cfg := range configs.Items {
			if cfg.Annotations[types.OvnRouteAdvertisementsKey] == "ra/"+source.Name+"/node" && slices.Contains(cfg.Spec.BGP.Routers[0].Prefixes, "1.1.2.0/24") {
				return nil
			}
		}
		return errors.New("recovered node configuration has not updated")
	}, 5*time.Second).Should(gomega.Succeed(), "regenerate after InternalIPs recover")
}

func TestRenderedNodeCoverageEVPN(t *testing.T) {
	for _, tc := range []struct {
		name                                                                 string
		ipVRF, macVRF, neighbors, explicitRouter, routerElsewhere, wantError bool
		asn                                                                  uint32
	}{
		{name: "IP-VRF inherits global ASN", ipVRF: true, asn: 65000},
		{name: "IP-VRF requires an ASN", ipVRF: true, wantError: true},
		{name: "explicit IP-VRF in another source", ipVRF: true, asn: 65000, neighbors: true, explicitRouter: true},
		{name: "IP-VRF on unselected node prevents ASN fallback", ipVRF: true, asn: 65000, neighbors: true, routerElsewhere: true, wantError: true},
		{name: "MAC-VRF uses global peers", macVRF: true, asn: 65000, neighbors: true},
		{name: "MAC-VRF requires peers", macVRF: true, asn: 65000, wantError: true},
		{name: "MAC-VRF requires an ASN", macVRF: true, neighbors: true, wantError: true},
		{name: "combined IP and MAC VRF inherits ASN", ipVRF: true, macVRF: true, asn: 65000, neighbors: true},
		{name: "MAC-VRF cannot hide missing IP-VRF coverage", ipVRF: true, macVRF: true, asn: 65000, neighbors: true, routerElsewhere: true, wantError: true},
		{name: "non-EVPN still requires matching VRF", asn: 65000, neighbors: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			selected := &selectedNetworks{
				networks: []string{"blue"}, networkVRFs: map[string]string{"blue": "blue"},
			}
			if tc.ipVRF {
				selected.ipVRFConfigs = []*ipVRFConfig{{vrfConfig: vrfConfig{networkName: "blue", vni: 2000}, vrfName: "blue"}}
			}
			if tc.macVRF {
				selected.macVRFConfigs = []*vrfConfig{{networkName: "blue", vni: 1000}}
			}
			router := &testRouter{ASN: tc.asn}
			if tc.neighbors {
				router.Neighbors = []*testNeighbor{{ASN: 65001, Address: "192.0.2.1"}}
			}
			sources := []*frrapi.FRRConfiguration{(&testFRRConfig{Name: "global", Routers: []*testRouter{router}}).FRRConfiguration()}
			allVRFs := sets.New[string]("")
			if tc.explicitRouter || tc.routerElsewhere {
				allVRFs.Insert("blue")
			}
			if tc.explicitRouter {
				sources = append(sources, (&testFRRConfig{Name: "explicit", Routers: []*testRouter{{ASN: 65002, VRF: "blue"}}}).FRRConfiguration())
			}
			var templates []*frrTemplate
			for _, source := range sources {
				template := prepareFRRTemplate("auto", source, selected)
				template.resolveEVPNRouters(selected, allVRFs)
				templates = append(templates, template)
			}
			ra := (&testRA{TargetVRF: "auto"}).RouteAdvertisements()
			node := (&testNode{Name: "node"}).Node()
			selected.hostNetworkSubnets = map[string][]string{"blue": {"10.0.0.0/24"}}
			covered := sets.New[string]()
			valid, errs := selectNodeTemplates(ra, templates, selected, []string{"blue"})
			for _, template := range valid {
				_, err := (&Controller{}).generateFRRConfiguration(ra, template, node, selected, covered)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "render the selected template")
			}
			g.Expect(covered.Has("blue")).To(gomega.Equal(!tc.wantError), "coverage from the rendered templates")
			if !tc.wantError {
				g.Expect(errs).To(gomega.BeEmpty(), "valid EVPN templates must be selected")
			}
		})
	}
}

func TestController_nodeOutsidePeeringTemplate(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	frr := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace")
	source, err := frr.Get(ctx, "frrConfig", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "get peering template")
	source.Spec.NodeSelector.MatchLabels = map[string]string{corev1.LabelHostname: "node"}
	_, err = frr.Update(ctx, source, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "restrict the template to the healthy node")
	tt.waitForFRRCache()
	other := (&testNode{Name: "other"}).Node()
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, other, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create a node without allocated subnets")
	g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(other.Name) }, 5*time.Second).Should(gomega.Equal(other))
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	ra := tt.expectRAStatus(true, false)
	g.Expect(tt.generatedConfigs("ra")).To(gomega.HaveLen(1), "only the node selected by the template needs a configuration")

	ra.Spec.TargetVRF = "auto"
	tt.updateRA(ra)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	ra = tt.expectRAStatus(true, true)
	g.Expect(meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("don't match all selected networks"), "auto still requires coverage for the active default network on the other node")
}

func TestController_reconcileHostname(t *testing.T) {
	longName := strings.Repeat("a", 63) + ".example.org"
	for _, tc := range []struct{ name, hostname string }{{longName, "worker"}, {longName, ""}, {"worker", ""}} {
		t.Run(tc.name+"/hostname="+tc.hostname, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tt := newFRRReconcileTest(t, g, "eth0")
			ctx := context.Background()
			ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true}).RouteAdvertisements()
			ra.UID = "original-ra"
			tt.updateRA(ra)
			name, hostname := tc.name, tc.hostname
			node := (&testNode{Name: name, OmitPrimaryAddress: true,
				SubnetsAnnotation: `{"default":"1.1.1.0/24"}`,
				Labels:            map[string]string{corev1.LabelHostname: hostname},
			}).Node()
			if hostname == "" {
				delete(node.Labels, corev1.LabelHostname)
			}
			_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create node %s", name)
			g.Eventually(func() error { _, err := tt.c.nodeLister.Get(name); return err }, 5*time.Second).Should(gomega.Succeed())
			g.Expect(tt.c.reconcile(context.Background(), "ra")).To(gomega.Succeed())
			generated := tt.generatedConfigs("ra")
			g.Expect(generated).To(gomega.HaveKey("ra/frrConfig/node"), "healthy nodes must still be configured")
			tt.expectRAStatus(true, hostname == "")
			if hostname == "" {
				g.Expect(generated).To(gomega.HaveLen(1))
				return
			}
			g.Expect(generated).To(gomega.HaveKey("ra/frrConfig/"+name), "annotation must retain the raw node name")
			cfg := generated["ra/frrConfig/"+name]
			g.Expect(cfg.Labels[types.OvnRouteAdvertisementsNodeKey]).To(gomega.Equal(util.RouteAdvertisementsNodeLabel(name)))
			for _, value := range cfg.Labels {
				g.Expect(validation.IsValidLabelValue(value)).To(gomega.BeEmpty())
			}
			g.Expect(cfg.Spec.NodeSelector.MatchLabels).To(gomega.Equal(map[string]string{corev1.LabelHostname: hostname}))
			selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(selector.Matches(labels.Set(node.Labels))).To(gomega.BeTrue())

			// Even when the routes are unchanged, a missing label can hide
			// the export from the node or prevent it from identifying the RA.
			client := tt.c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace)
			for _, key := range []string{types.OvnRouteAdvertisementsNodeKey, types.OvnRouteAdvertisementsUIDKey} {
				delete(cfg.Labels, key)
				_, err = client.Update(ctx, cfg, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "seed the existing export without %s", key)
				tt.waitForFRRCache()
				g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "backfill %s", key)
				cfg, err = client.Get(ctx, cfg.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "read the same export after backfill")
				g.Expect(cfg.Labels).To(gomega.HaveKeyWithValue(types.OvnRouteAdvertisementsNodeKey, util.RouteAdvertisementsNodeLabel(name)))
				g.Expect(cfg.Labels).To(gomega.HaveKeyWithValue(types.OvnRouteAdvertisementsUIDKey, string(ra.UID)))
			}
		})
	}
}

func TestController_reconcileHostnameCollision(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	nodes := tt.fakeClientset.KubeClient.CoreV1().Nodes()
	tt.updateRA((&testRA{Name: "ra", SelectsDefault: true, AdvertiseEgressIPs: true, NodeSelector: map[string]string{"advertise": "yes"}}).RouteAdvertisements())
	for _, name := range []string{"worker-a", "worker-b"} {
		node := (&testNode{Name: name, Labels: map[string]string{"advertise": "yes"}}).Node()
		_, err := nodes.Create(ctx, node, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "create selected node %s", name)
		g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(name) }, 5*time.Second).Should(gomega.Equal(node), "cache selected node %s", name)
	}
	eip := (&testEIP{Name: "eip", EIPs: map[string]string{"worker-a": "192.0.2.10", "worker-b": "192.0.2.11"}}).EgressIP()
	_, err := tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, eip, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "assign an EgressIP to each selected node")
	g.Eventually(func() (*eiptypes.EgressIP, error) { return tt.c.eipLister.Get(eip.Name) }, 5*time.Second).Should(gomega.Equal(eip), "cache both EgressIP assignments")

	const keyA, keyB = "ra/frrConfig/worker-a", "ra/frrConfig/worker-b"
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "publish both advertisements before the collision")
	g.Expect(slices.Collect(maps.Keys(tt.generatedConfigs("ra")))).To(gomega.ConsistOf(keyA, keyB), "both selected nodes initially advertise their EgressIPs")
	// The RA excludes this third node. Giving it worker-a's hostname makes
	// FRR on both nodes match the existing export.
	other, err := nodes.Get(ctx, "node", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "get the unselected node")
	for _, hostname := range []string{"worker-a", "node"} {
		other.Labels[corev1.LabelHostname] = hostname
		other, err = nodes.Update(ctx, other, metav1.UpdateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "set the unselected node's hostname to %s", hostname)
		g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(other.Name) }, 5*time.Second).Should(gomega.Equal(other), "cache hostname %s", hostname)
		tt.waitForFRRCache()
		g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "reconcile hostname %s", hostname)
		configs := tt.generatedConfigs("ra")
		g.Expect(configs).To(gomega.HaveKey(keyB), "worker-b keeps its export throughout")
		collision := hostname == "worker-a"
		ra := tt.expectRAStatus(true, collision)
		if collision {
			g.Expect(configs).NotTo(gomega.HaveKey(keyA), "withdraw the selector matching both nodes")
			g.Expect(meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("kubernetes.io/hostname=worker-a matches 2 nodes"), "identify the ambiguous selector")
		} else {
			g.Expect(configs).To(gomega.HaveKey(keyA), "restore worker-a once its hostname is unique")
			g.Expect(configs[keyA].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("192.0.2.10/32"), "restore worker-a's EgressIP")
		}
	}
}

func TestLateGeneratedConfigQueuesOwner(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	old := generatedFRRConfig("ovnk-generated-late", "node", true)
	old.Annotations[types.OvnRouteAdvertisementsKey] = "ra/deselected/node"
	tt := newFRRReconcileTest(t, g, "eth0")
	config.OVNKubernetesFeature.EnableEVPN = false
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "publish the initial network intent")
	tt.waitForDefaultNADCache()
	tt.waitForFRRCache()
	lister := tt.c.frrLister
	tt.c.frrLister = frrlisters.NewFRRConfigurationLister(tt.frrCacheWithout(old.Name))
	_, err := tt.c.frrClient.ApiV1beta1().FRRConfigurations(old.Namespace).Create(ctx, old.FRRConfiguration(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the obsolete export before its watch event arrives")
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "reconcile after deselection with the generated config still missing from the cache")
	before := tt.generatedConfigs("ra")
	g.Expect(slices.Collect(maps.Keys(before))).To(gomega.ConsistOf("ra/deselected/node", "ra/frrConfig/node"), "the old and current objects are both in the API")

	tt.c.frrLister = lister
	tt.waitForFRRCache()
	queue := &controllerutil.FakeController{}
	tt.c.raController = queue
	event := before["ra/deselected/node"].DeepCopy()
	now := metav1.Now()
	event.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: fieldManager, Time: &now}}
	g.Expect(tt.c.frrConfigurationNeedsUpdate(nil, event)).To(gomega.BeFalse(), "the add needs no cluster-wide reconcile")
	g.Expect(tt.c.frrConfigurationNeedsUpdate(event, event)).To(gomega.BeFalse(), "resyncs must not enqueue the owner again")
	g.Expect(queue.Reconciles).To(gomega.Equal([]string{"Reconcile:ra"}), "the delayed create must wake its owner once")
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "process the queued RA")
	g.Expect(tt.generatedConfigs("ra")).To(gomega.Equal(map[string]*frrapi.FRRConfiguration{
		"ra/frrConfig/node": before["ra/frrConfig/node"],
	}), "withdraw the deselected object and keep the current one intact")

	// The same object can change twice before the first spec update reaches
	// the cache. The rollback sees the original prefix and skips its write.
	tt.waitForFRRCache()
	tt.c.frrLister = frrlisters.NewFRRConfigurationLister(tt.frrCacheWithout(""))
	tt.updateNodeSubnets("node", `{"default":"1.1.1.0/24"}`)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "publish the replacement prefix")
	tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24"}`)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "roll back with the original spec still cached")
	updated := tt.generatedConfigs("ra")["ra/frrConfig/node"]
	g.Expect(updated.Spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"1.1.1.0/24"}), "the pending watch event carries the replacement")
	tt.c.frrLister = lister
	tt.waitForFRRCache()
	original := before["ra/frrConfig/node"]
	updated.Generation = original.Generation + 1 // The fake client does not increment generation.
	updated.ManagedFields = event.ManagedFields
	queue.Reconciles = nil
	g.Expect(tt.c.frrConfigurationNeedsUpdate(original, updated)).To(gomega.BeFalse(), "a generated spec update needs only its owner")
	g.Expect(queue.Reconciles).To(gomega.Equal([]string{"Reconcile:ra"}), "a delayed spec update must wake the owner")
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "recheck the RA against the updated FRR cache")
	g.Expect(tt.generatedConfigs("ra")["ra/frrConfig/node"].Spec).To(gomega.Equal(original.Spec), "restore the original routes")
}

func TestController_reconcileSameNamedTemplatesAcrossNamespaces(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := t.Context()
	tt := newFRRReconcileTest(t, g, "eth0")
	source := (&testFRRConfig{Name: "frrConfig", Namespace: "staging", Routers: []*testRouter{
		{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth1"}}},
	}}).FRRConfiguration()
	staging := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(source.Namespace)
	_, err := staging.Create(ctx, source, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the same-named staging template")
	tt.waitForFRRCache()
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "generate both configurations")
	const key = "ra/frrConfig/node"
	healthy := tt.generatedConfigs("ra")
	g.Expect(healthy).To(gomega.HaveKey(key), "publish the primary namespace's configuration")
	generated := metav1.ListOptions{LabelSelector: labels.Set{types.OvnRouteAdvertisementsKey: "ra"}.String()}
	before, err := staging.List(ctx, generated)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "list generated staging configurations")
	g.Expect(before.Items).To(gomega.HaveLen(1), "publish the staging configuration")

	source.Spec.BGP.Routers[0].Neighbors[0].Interface = unsafeInputValue
	_, err = staging.Update(ctx, source, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "invalidate only the staging template")
	tt.waitForFRRCache()
	tt.updateNodeSubnets("node", `{"default":"1.1.2.0/24"}`)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "reconcile the subnet change despite the invalid template")
	_, err = staging.Get(ctx, before.Items[0].Name, metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "withdraw the failed staging configuration")
	current := tt.generatedConfigs("ra")
	g.Expect(current).To(gomega.HaveKey(key), "keep the healthy configuration")
	g.Expect(current[key].Name).To(gomega.Equal(healthy[key].Name), "update the healthy configuration in place")
	g.Expect(current[key].Spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"1.1.2.0/24"}), "advertise the updated subnet")
	tt.expectRAStatus(true, true)
}

func TestController_reconcilePartialNodeFailure(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			g := gomega.NewWithT(t)
			old := generatedFRRConfig("ovnk-generated-old", "node1", true)
			old.Routers[0].Prefixes = []string{"1.1.1.0/24"}
			old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
			obsolete := generatedFRRConfig("ovnk-generated-deleted", "deleted-node", true)
			removedSource := generatedFRRConfig("ovnk-generated-removed-source", "node1", true)
			removedSource.Annotations[types.OvnRouteAdvertisementsKey] = "ra/removed-source/node1"
			tt := newFRRReconcileTest(t, g, "eth0", old, obsolete, removedSource)
			config.Gateway.Mode = config.GatewayModeShared
			ctx := context.Background()
			node := (&testNode{Name: "node1", SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}).Node()
			if pending {
				node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"172.18.0.1/24"}`
			} else {
				node.Annotations[types.NodeSubnetsAnnotation] = "invalid"
			}
			healthy := (&testNode{Name: "node2", SubnetsAnnotation: `{"default":"1.1.2.0/24"}`, OmitPrimaryAddress: true}).Node()
			for _, object := range []*corev1.Node{node, healthy} {
				_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, object, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(object.Name) }, 5*time.Second).Should(gomega.Equal(object), "cache must observe initial node %s", object.Name)
			}
			failDelete := true
			deleteErr := errors.New("injected cleanup failure")
			tt.fakeClientset.FRRClient.(Fake).PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
				if failDelete && action.(ctesting.DeleteAction).GetName() == old.Name {
					return true, nil, deleteErr
				}
				return false, nil, nil
			})
			// Both a new config and a later subnet update must reach healthy
			// nodes while the failed node's old config cannot be deleted.
			var reconcileErr error
			for _, subnet := range []string{"1.1.0.0/24", "1.1.3.0/24"} {
				tt.updateNodeSubnets("node", fmt.Sprintf(`{"default":%q}`, subnet))
				reconcileErr = tt.c.reconcile(ctx, "ra")
				g.Expect(errors.Is(reconcileErr, deleteErr)).To(gomega.BeTrue(), "retry the failed node's withdrawal")
				status := tt.expectRAStatus(true, true)
				degraded := meta.FindStatusCondition(status.Status.Conditions, conditionTypeDegraded)
				g.Expect(degraded.Reason).To(gomega.Equal("NodeConfigurationFailed"))
				g.Expect(degraded.Message).To(gomega.ContainSubstring(deleteErr.Error()), "report the failed deletion while healthy nodes progress")
				configs := tt.generatedConfigs("ra")
				g.Expect(slices.Collect(maps.Keys(configs))).To(gomega.ConsistOf("ra/frrConfig/node", "ra/frrConfig/node1", "ra/frrConfig/node2"))
				g.Expect(configs["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{subnet}))
				g.Expect(configs["ra/frrConfig/node1"].Spec).To(gomega.Equal(old.FRRConfiguration().Spec))
				annotations := tt.defaultNADAnnotations()
				g.Expect(annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra","other"]`), "retain references until withdrawal succeeds")
				g.Expect(annotations).NotTo(gomega.HaveKey(types.OvnRouteAdvertisementsIntentKey), "defer the first intent until the failed withdrawal succeeds")
				tt.waitForFRRCache()
			}
			g.Expect(tt.c.handleReconcileError(ctx, "ra", reconcileErr)).To(gomega.Succeed(), "handle retry exhaustion for the failed deletion")
			tt.expectRAStatus(true, true)
			failDelete = false
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"targetVRF":""}}`), "publish the first intent once the old export is withdrawn")
			g.Expect(slices.Collect(maps.Keys(tt.generatedConfigs("ra")))).To(gomega.ConsistOf("ra/frrConfig/node", "ra/frrConfig/node2"))
			_, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace").Get(ctx, old.Name, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "withdraw the failed node's complete configuration")
			ra := tt.expectRAStatus(true, true)
			g.Expect(meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("node1"))

			// Recover the node, then verify its configuration is recreated.
			delete(node.Annotations, util.OVNNodePrimaryDPUHostAddr)
			node.Annotations[types.NodeSubnetsAnnotation] = `{"default":"1.1.1.0/24"}`
			_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() (*corev1.Node, error) { return tt.c.nodeLister.Get(node.Name) }, 5*time.Second).Should(gomega.Equal(node), "cache must observe recovered node %s", node.Name)
			tt.waitForFRRCache()
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			configs := tt.generatedConfigs("ra")
			g.Expect(slices.Collect(maps.Keys(configs))).To(gomega.ConsistOf("ra/frrConfig/node", "ra/frrConfig/node1", "ra/frrConfig/node2"))
			updated := configs["ra/frrConfig/node1"]
			g.Expect(updated.Spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"1.1.1.0/24"}))
			tt.expectRAStatus(true, false)
		})
	}
}

type failingEgressIPLister struct {
	eiplisters.EgressIPLister
	err   error
	calls int
}

func (l *failingEgressIPLister) List(labels.Selector) ([]*eiptypes.EgressIP, error) {
	l.calls++
	return nil, l.err
}

type failingNamespaceLister struct {
	corelisters.NamespaceLister
	err   error
	calls int
}

func (l *failingNamespaceLister) List(labels.Selector) ([]*corev1.Namespace, error) {
	l.calls++
	return nil, l.err
}

func TestController_reconcileEgressIPLookupFailure(t *testing.T) {
	for _, lookup := range []string{"egressIPs", "namespaces"} {
		t.Run(lookup, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			tt := newFRRReconcileTest(t, g, "eth0")
			ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, AdvertiseEgressIPs: true}).RouteAdvertisements()
			tt.updateRA(ra)
			_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx,
				(&testNode{Name: "node1", SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}).Node(), metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create second node")
			g.Eventually(func() error { _, err := tt.c.nodeLister.Get("node1"); return err }, 5*time.Second).Should(gomega.Succeed())
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "publish pod routes before assigning an EgressIP")
			published := tt.expectRAStatus(true, false)
			g.Eventually(func() (*ratypes.RouteAdvertisements, error) { return tt.c.raLister.Get("ra") }, 5*time.Second).
				Should(gomega.Equal(published), "wait for the controller to observe the healthy RA")
			configs := tt.generatedConfigs("ra")
			g.Expect(slices.Collect(maps.Keys(configs))).To(gomega.ConsistOf("ra/frrConfig/node", "ra/frrConfig/node1"), "publish routes for both nodes")
			owner := configs["ra/frrConfig/node"]
			g.Expect(owner.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24"), "only the pod subnet is advertised initially")

			frr := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace")
			before, err := frr.List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			beforeNAD := tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]

			_, err = tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx,
				(&testEIP{Name: "eip", EIPs: map[string]string{"node": "192.0.2.10"}}).EgressIP(), metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create EgressIP assignment")
			g.Eventually(func() error { _, err := tt.c.eipLister.Get("eip"); return err }, 5*time.Second).Should(gomega.Succeed())
			injected := errors.New("shared lookup failure")
			eipLister, namespaceLister := tt.c.eipLister, tt.c.namespaceLister
			failedEIPs := &failingEgressIPLister{EgressIPLister: eipLister, err: injected}
			failedNamespaces := &failingNamespaceLister{NamespaceLister: namespaceLister, err: injected}
			if lookup == "egressIPs" {
				tt.c.eipLister = failedEIPs
			} else {
				tt.c.namespaceLister = failedNamespaces
			}
			err = tt.c.reconcile(ctx, "ra")
			g.Expect(errors.Is(err, injected)).To(gomega.BeTrue(), "shared failure must be returned for retry")
			var nodeErr *nodeGenerationError
			g.Expect(errors.As(err, &nodeErr)).To(gomega.BeFalse(), "shared failures must not be classified as node failures")
			g.Expect(failedEIPs.calls+failedNamespaces.calls).To(gomega.Equal(1), "do not repeat the shared lookup for each node")
			after, err := frr.List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(after.Items).To(gomega.ConsistOf(before.Items), "retain complete existing configurations and readiness markers")
			g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(beforeNAD))
			current, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(current.Status).To(gomega.Equal(published.Status), "shared lookup failure must preserve the published status")

			tt.c.eipLister, tt.c.namespaceLister = eipLister, namespaceLister
			tt.waitForFRRCache()
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "retry succeeds after the shared lookup recovers")
			recovered := tt.generatedConfigs("ra")
			g.Expect(recovered).To(gomega.HaveLen(len(configs)), "recovery must keep one generated export per node")
			updated := recovered["ra/frrConfig/node"]
			g.Expect(updated).NotTo(gomega.BeNil(), "the original EgressIP owner still has an export")
			g.Expect(updated.Name).To(gomega.Equal(owner.Name), "update the original owner's export in place")
			g.Expect(updated.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "192.0.2.10/32"))
		})
	}
}

func TestController_reconcileEgressIPOwnerChange(t *testing.T) {
	for _, tc := range []struct {
		name, ip             string
		moved, advertisePods bool
	}{
		{name: "IPv4 removed", ip: "192.0.2.10", advertisePods: true},
		{name: "IPv4 moved", ip: "192.0.2.10", moved: true, advertisePods: true},
		{name: "IPv6 removed", ip: "2001:db8::10", advertisePods: true},
		{name: "IPv6 moved", ip: "2001:db8::10", moved: true, advertisePods: true},
		{name: "EgressIP only moves between healthy nodes", ip: "192.0.2.10", moved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			egressPrefix := tc.ip + util.GetIPFullMaskString(tc.ip)
			old := generatedFRRConfig("ovnk-generated-old", "node1", true)
			old.Routers[0].Prefixes = []string{egressPrefix}
			formerOwner := &testNode{Name: "node1", SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}
			var wantPrefixes []string
			if tc.advertisePods {
				formerOwner.SubnetsAnnotation = "invalid"
				old.Routers[0].Prefixes = append(old.Routers[0].Prefixes, "1.1.1.0/24")
				wantPrefixes = append(wantPrefixes, "1.1.0.0/24")
			}
			old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
			tt := newFRRReconcileTest(t, g, "eth0", old)
			tt.updateRA((&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: tc.advertisePods, AdvertiseEgressIPs: true}).RouteAdvertisements())
			_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, formerOwner.Node(), metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create former EgressIP owner")
			if tc.moved {
				wantPrefixes = append(wantPrefixes, egressPrefix)
				eip := (&testEIP{Name: "eip", EIPs: map[string]string{"node": tc.ip}}).EgressIP()
				_, err = tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, eip, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "move address to healthy node")
				g.Eventually(func() error { _, err := tt.c.eipLister.Get("eip"); return err }, 5*time.Second).Should(gomega.Succeed())
			}
			g.Eventually(func() error { _, err := tt.c.nodeLister.Get("node1"); return err }, 5*time.Second).Should(gomega.Succeed())

			// The new owner must wait for the old advertisement's withdrawal
			// even when both nodes generated successfully.
			failDelete := true
			deleteErr := errors.New("injected cleanup failure")
			tt.fakeClientset.FRRClient.(Fake).PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
				if failDelete && action.(ctesting.DeleteAction).GetName() == old.Name {
					return true, nil, deleteErr
				}
				return false, nil, nil
			})
			g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), deleteErr)).To(gomega.BeTrue(), "retry the former owner's failed withdrawal")
			if tc.moved {
				g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", old.Name), "a reassigned address must wait for the former owner's withdrawal")
			} else {
				configs := tt.generatedConfigs("ra")
				g.Expect(configs).To(gomega.HaveKey("ra/frrConfig/node1"), "the former owner's export is still present")
				g.Expect(configs).To(gomega.HaveKey("ra/frrConfig/node"), "publish pod routes on the independent node")
				g.Expect(configs["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(wantPrefixes), "only the pod subnet is advertised on the healthy node")
			}
			failDelete = false
			tt.waitForFRRCache()
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "withdraw the old configuration and publish the healthy node's prefixes")
			configs := tt.generatedConfigs("ra")
			g.Expect(slices.Collect(maps.Keys(configs))).To(gomega.ConsistOf("ra/frrConfig/node"), "the former owner's configuration must be gone")
			g.Expect(configs["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(wantPrefixes))
			tt.expectRAStatus(true, tc.advertisePods)
		})
	}
}

func TestControllerReconcileSwappedEgressIPs(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	const first = "192.0.2.10/32"
	const second = "2001:db8::10/128"
	seed := func(node, subnet, eip string) *testFRRConfig {
		cfg := generatedFRRConfig("ovnk-generated-"+node, node, true)
		cfg.Routers[0].Prefixes = []string{subnet, eip}
		cfg.Routers[0].Neighbors[0].Advertise = cfg.Routers[0].Prefixes
		return cfg
	}
	tt := newFRRReconcileTest(t, g, "eth0", seed("node", "1.1.0.0/24", first), seed("node1", "1.1.1.0/24", second))
	tt.updateRA((&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, AdvertiseEgressIPs: true}).RouteAdvertisements())
	node := (&testNode{Name: "node1", SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}).Node()
	_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the second owner")
	eip := (&testEIP{Name: "swap", EIPs: map[string]string{"node": "2001:db8::10", "node1": "192.0.2.10"}}).EgressIP()
	_, err = tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, eip, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "swap the assigned addresses")
	g.Eventually(func() error { _, err := tt.c.eipLister.Get(eip.Name); return err }, 5*time.Second).Should(gomega.Succeed())
	g.Eventually(func() error { _, err := tt.c.nodeLister.Get(node.Name); return err }, 5*time.Second).Should(gomega.Succeed())
	failure := errors.New("former owner update failed")
	fail := true
	tt.fakeClientset.FRRClient.(Fake).PrependReactor("update", "frrconfigurations", func(a ctesting.Action) (bool, runtime.Object, error) {
		if fail && a.(ctesting.UpdateAction).GetObject().(*frrapi.FRRConfiguration).Name == "ovnk-generated-node1" {
			return true, nil, failure
		}
		return false, nil, nil
	})
	check := func(node string, prefixes ...string) {
		t.Helper()
		cfg := tt.generatedConfigs("ra")["ra/frrConfig/"+node]
		g.Expect(cfg).NotTo(gomega.BeNil(), "retain the pod advertisement on %s", node)
		router := cfg.Spec.BGP.Routers[0]
		g.Expect(router.Prefixes).To(gomega.ConsistOf(prefixes), "advertised prefixes on %s", node)
		g.Expect(router.Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(prefixes), "allowed prefixes on %s", node)
	}
	g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed withdrawal")
	check("node", "1.1.0.0/24")
	check("node1", "1.1.1.0/24", second)
	fail = false
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "finish withdrawals before publishing the new owners")
	check("node", "1.1.0.0/24", second)
	check("node1", "1.1.1.0/24", first)
	tt.expectRAStatus(true, false)
}

// Truncating diagnostics must not hide a transient error from retry logic.
func TestNodeGenerationErrorReporting(t *testing.T) {
	g := gomega.NewWithT(t)
	failure := &nodeGenerationError{}
	g.Expect(failure.Error()).To(gomega.BeEmpty())
	g.Expect((&nodeGenerationError{errs: []error{}}).Error()).To(gomega.BeEmpty())
	g.Expect((&nodeGenerationError{errs: []error{nil}}).Error()).To(gomega.BeEmpty())
	var messages []string
	for i := 0; i < maxReportedNodeErrors; i++ {
		err := fmt.Errorf("node-%d: %w", i, errPending)
		failure.errs = append(failure.errs, err)
		messages = append(messages, err.Error())
	}
	g.Expect(failure.Error()).To(gomega.Equal(strings.Join(messages, "\n")))
	g.Expect((frrGenerationResult{nodeErrors: failure.errs}).hasRetryableNodeError()).To(gomega.BeFalse(), "pending nodes wait for a change in their inputs")

	transient := errors.New("transient API failure")
	failure.errs = append(failure.errs, transient)
	result := frrGenerationResult{nodeErrors: failure.errs}
	g.Expect(result.failureError().Error()).To(gomega.Equal(strings.Join(messages, "\n") + "\n... and 1 more node errors"))
	g.Expect(errors.Is(result.failureError(), transient)).To(gomega.BeTrue(), "the omitted error remains available to callers")
	g.Expect(result.hasRetryableNodeError()).To(gomega.BeTrue(), "a transient error beyond the reporting limit still requires a retry")
}

func TestController_updateRAStatusDegraded(t *testing.T) {
	metrics.RegisterClusterManagerFunctional()
	g := gomega.NewWithT(t)
	clients := util.GetOVNClientset().GetClusterManagerClientset()
	c := &Controller{raClient: clients.RouteAdvertisementsClient}
	ctx := context.Background()
	ra := (&testRA{Name: "degraded"}).RouteAdvertisements()
	ra.Generation = 3
	_, err := c.raClient.K8sV1().RouteAdvertisements().Create(ctx, ra, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition(ra.Name) })
	transient := errors.New("node API unavailable")
	for _, tc := range []struct {
		name     string
		result   frrGenerationResult
		accepted bool
		degraded bool
		reason   string
	}{
		{"all nodes pending", frrGenerationResult{nodeErrors: []error{fmt.Errorf("node1: %w", errPending)}}, true, true, "Accepted"},
		{"node annotation invalid", frrGenerationResult{nodeErrors: []error{fmt.Errorf("node1: %w", errNodeNotReady)}}, true, true, "Accepted"},
		{"transient node error", frrGenerationResult{nodeErrors: []error{transient}}, true, true, "Accepted"},
		{"invalid template mixed with node error", frrGenerationResult{nodeErrors: []error{errConfig, transient}}, true, true, "Accepted"},
		{"invalid VTEP mixed with node error", frrGenerationResult{vtepErr: errInvalidVTEP, nodeErrors: []error{transient}}, false, true, "InternalError"},
		{"invalid template mixed with pending node", frrGenerationResult{nodeErrors: []error{errPending, errConfig}}, true, true, "Accepted"},
		{"pending node mixed with invalid template", frrGenerationResult{nodeErrors: []error{errConfig, errPending}}, true, true, "Accepted"},
		{"invalid template mixed with unavailable node state", frrGenerationResult{nodeErrors: []error{errNodeNotReady, errConfig}}, true, true, "Accepted"},
		{"invalid template joined with unavailable node state", frrGenerationResult{nodeErrors: []error{errors.Join(errNodeNotReady, errConfig)}}, true, true, "Accepted"},
		{"invalid VTEP with global pending error", frrGenerationResult{vtepErr: errInvalidVTEP, err: errPending}, false, true, "ConfigurationError"},
		{"invalid VTEP mixed with pending node", frrGenerationResult{vtepErr: errInvalidVTEP, nodeErrors: []error{errPending}}, false, true, "ConfigurationError"},
		{"global configuration pending", frrGenerationResult{err: errPending}, false, true, "ConfigurationPending"},
		{"configuration apply error", frrGenerationResult{err: transient}, false, true, "InternalError"},
		{"recovered", frrGenerationResult{}, true, false, "Accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			current, err := c.raClient.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(c.updateRAStatus(ctx, current, false, tc.result)).To(gomega.Succeed())
			current, err = c.raClient.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			for name, wantTrue := range map[string]bool{conditionTypeAccepted: tc.accepted, conditionTypeDegraded: tc.degraded} {
				want := metav1.ConditionFalse
				if wantTrue {
					want = metav1.ConditionTrue
				}
				condition := meta.FindStatusCondition(current.Status.Conditions, name)
				g.Expect(condition).NotTo(gomega.BeNil(), "RA must report %s", name)
				g.Expect(condition.Status).To(gomega.Equal(want), "%s status", name)
				g.Expect(condition.ObservedGeneration).To(gomega.Equal(ra.Generation), "%s observed generation", name)
				if name == conditionTypeAccepted {
					g.Expect(condition.Reason).To(gomega.Equal(tc.reason), "Accepted reason")
				}
				value, found := getRAConditionMetricValue(ra.Name, name, strings.ToLower(string(want)))
				g.Expect(found).To(gomega.BeTrue(), "missing metric for %s=%s", name, want)
				g.Expect(value).To(gomega.Equal(float64(1)), "active metric for %s=%s", name, want)
			}
			// An unchanged degraded result must not produce another status write.
			before := len(clients.RouteAdvertisementsClient.(*rafake.Clientset).Actions())
			g.Expect(c.updateRAStatus(ctx, current, false, tc.result)).To(gomega.Succeed())
			g.Expect(clients.RouteAdvertisementsClient.(*rafake.Clientset).Actions()).To(gomega.HaveLen(before))
		})
	}
}

func TestSharedNADRetainsBothAdvertisements(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	stale := &nadtypes.NetworkAttachmentDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: "blue", Name: "blue", UID: "original", ResourceVersion: "1"}}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(indexer.Add(stale)).To(gomega.Succeed())

	// East's write reached the API before west's reconcile saw it in the cache.
	live := stale.DeepCopy()
	live.ResourceVersion = "2"
	live.Annotations = map[string]string{
		types.OvnRouteAdvertisementsKey:       `["east"]`,
		types.OvnRouteAdvertisementsIntentKey: `{"east":{"targetVRF":"red"}}`,
	}
	client := nadfake.NewSimpleClientset()
	nads := client.K8sCniCncfIoV1().NetworkAttachmentDefinitions(live.Namespace)
	_, err := nads.Create(ctx, live, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish east's updated NAD")
	// Match the API's patch precondition and assign a new version on success.
	version := 2
	applyPatch := ctesting.ObjectReaction(client.Tracker())
	client.PrependReactor("patch", "network-attachment-definitions", func(action ctesting.Action) (bool, runtime.Object, error) {
		var patch nadtypes.NetworkAttachmentDefinition
		g.Expect(json.Unmarshal(action.(ctesting.PatchAction).GetPatch(), &patch)).To(gomega.Succeed())
		if patch.ResourceVersion != "" && patch.ResourceVersion != fmt.Sprint(version) {
			return true, nil, apierrors.NewConflict(action.GetResource().GroupResource(), live.Name, errors.New("stale version"))
		}
		handled, obj, err := applyPatch(action)
		if err != nil {
			return handled, obj, err
		}
		patched := obj.(*nadtypes.NetworkAttachmentDefinition).DeepCopy()
		version++
		patched.ResourceVersion = fmt.Sprint(version)
		return true, patched, client.Tracker().Update(action.GetResource(), patched, patched.Namespace)
	})
	nadLister := nadlisters.NewNetworkAttachmentDefinitionLister(indexer)
	c := Controller{pendingNAD: newPendingWrites(indexer), nadClient: client, nadLister: nadLister}
	updateWest := func() error {
		updates, err := c.getNADUpdates("west", &util.RouteAdvertisementsIntent{TargetVRF: "green"}, []*nadtypes.NetworkAttachmentDefinition{stale})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "prepare west's NAD update")
		_, err = c.applyNADUpdates(ctx, "west", updates)
		return err
	}
	g.Expect(apierrors.IsConflict(updateWest())).To(gomega.BeTrue(), "west must retry its stale patch")
	g.Expect(indexer.Update(live)).To(gomega.Succeed())
	g.Expect(updateWest()).To(gomega.Succeed(), "west can merge its intent once the cache catches up")
	nad, err := nads.Get(ctx, live.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var refs []string
	g.Expect(json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &refs)).To(gomega.Succeed())
	g.Expect(refs).To(gomega.ConsistOf("east", "west"))
	g.Expect(nad.Annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"east":{"targetVRF":"red"},"west":{"targetVRF":"green"}}`))
	both := nad.DeepCopy()

	// West is deleted right after its patch. Its reconcile waits for the
	// patch to reach the informer before withdrawing.
	g.Expect(c.pendingNAD.waiting("west")).To(gomega.BeTrue(), "hold west until its patch is visible")
	g.Expect(c.pendingNAD.waiting("east")).To(gomega.BeFalse(), "west's patch must not hold east")
	g.Expect(indexer.Update(both)).To(gomega.Succeed())
	queue := &controllerutil.FakeController{}
	c.raController = queue
	g.Expect(c.nadNeedsUpdate(stale, both)).To(gomega.BeFalse(), "a coalesced patch can be handled per RA")
	g.Expect(queue.Reconciles).To(gomega.ConsistOf("Reconcile:west", "Reconcile:east"), "wake west and process east's intervening change")
	g.Expect(c.pendingNAD.waiting("west")).To(gomega.BeFalse(), "withdraw using the cache that contains west's patch")
	updates, err := c.getNADUpdates("west", nil, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "prepare withdrawal against the current NAD")
	updated, err := c.applyNADUpdates(ctx, "west", updates)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "withdraw west using the current NAD")
	g.Expect(updated).To(gomega.BeTrue(), "withdraw west")
	nad, err = nads.Get(ctx, live.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "get the shared NAD after withdrawal")
	g.Expect(nad.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.MatchJSON(`["east"]`))
	g.Expect(nad.Annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"east":{"targetVRF":"red"}}`))

	g.Expect(indexer.Update(nad)).To(gomega.Succeed())
	queue.Reconciles = nil
	g.Expect(c.nadNeedsUpdate(both, nad)).To(gomega.BeFalse(), "west already applied this withdrawal")
	g.Expect(queue.Reconciles).To(gomega.BeEmpty(), "a confirmed patch needs no extra reconcile")

	// Replay the shared NAD's watch events, including a VRF-only change
	// while both RAs still select it. East's intent is the same throughout.
	checkEvent := func(old, new *nadtypes.NetworkAttachmentDefinition) {
		t.Helper()
		new = new.DeepCopy()
		now := metav1.Now()
		new.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: fieldManager, Time: &now}}
		queue.Reconciles = nil
		g.Expect(c.nadNeedsUpdate(old, new)).To(gomega.BeFalse(), "an owned patch needs no cluster-wide reconcile")
		g.Expect(c.nadNeedsUpdate(new, new)).To(gomega.BeFalse(), "an informer resync needs no reconcile")
		g.Expect(queue.Reconciles).To(gomega.Equal([]string{"Reconcile:west"}), "only west's entry changed")
	}
	checkEvent(live, both)
	changedVRF := both.DeepCopy()
	changedVRF.Annotations[types.OvnRouteAdvertisementsIntentKey] = `{"east":{"targetVRF":"red"},"west":{"targetVRF":""}}`
	checkEvent(both, changedVRF)
	egressOnly := changedVRF.DeepCopy()
	egressOnly.Annotations[types.OvnRouteAdvertisementsIntentKey] = nad.Annotations[types.OvnRouteAdvertisementsIntentKey]
	checkEvent(changedVRF, egressOnly) // West keeps its reference after dropping pod advertisement.
	checkEvent(egressOnly, nad)

	replacement := nad.DeepCopy()
	replacement.UID = "replacement"
	g.Expect(indexer.Update(replacement)).To(gomega.Succeed(), "recreate the NAD between selection and patch planning")
	updates, err = c.getNADUpdates("west", &util.RouteAdvertisementsIntent{TargetVRF: "green"}, []*nadtypes.NetworkAttachmentDefinition{stale})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("blue/blue")), "retry selection against the replacement")
	g.Expect(updates).To(gomega.BeEmpty(), "a replacement cannot inherit west's selection")
}

func TestController_rejectedUpdateWithdrawsConfigurations(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	var old []*testFRRConfig
	for _, node := range []string{"node", "node1", "node2"} {
		cfg := generatedFRRConfig("ovnk-generated-"+node, node, true)
		cfg.Routers[0].Prefixes = []string{"1.1.99.0/24", "1.3.0.0/24", "fd03::/64"}
		cfg.Routers[0].Neighbors[0].Advertise = cfg.Routers[0].Prefixes
		cfg.Routers[0].Imports = []string{"blue"}
		cfg.Routers = append(cfg.Routers, &testRouter{ASN: 1, VRF: "blue", Imports: []string{"default"}})
		if node == "node1" {
			cfg.Annotations[types.OvnRouteAdvertisementsKey] = "ra/bad-template/node1"
		}
		old = append(old, cfg)
	}
	tt := newFRRReconcileTest(t, g, "eth0", old...)
	config.Gateway.Mode = config.GatewayModeShared
	previousIPv6 := config.IPv6Mode
	t.Cleanup(func() { config.IPv6Mode = previousIPv6 })
	config.IPv6Mode = true
	nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
	tt.c.nm = nm
	for _, name := range []string{"blue", "green"} {
		refs := `["ra","other"]`
		if name == "green" {
			refs = `["other"]`
		}
		tt.addNetwork(nm, &testNAD{Name: name, Namespace: name, Network: util.GenerateCUDNNetworkName(name),
			Topology: types.Layer3Topology, Subnet: "1.3.0.0/16,fd03::/48", Labels: map[string]string{"color": name},
			Annotations: map[string]string{types.OvnRouteAdvertisementsKey: refs}})
	}
	for _, name := range []string{"node1", "node2", "pending"} {
		node := (&testNode{Name: name, Labels: map[string]string{corev1.LabelHostname: name},
			SubnetsAnnotation: `{"default":"1.1.0.0/24","cluster_udn_green":"1.3.0.0/24"}`}).Node()
		if name == "node2" {
			delete(node.Annotations, types.NodeSubnetsAnnotation)
		} else if name == "pending" {
			node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"172.18.0.1/24"}`
		}
		_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "create node %s for rejected-update cleanup", name)
		g.Eventually(func() error { _, err := tt.c.nodeLister.Get(name); return err }, 5*time.Second).Should(gomega.Succeed(), "wait for node %s in the cache", name)
	}
	tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24","cluster_udn_green":"1.3.0.0/24"}`)
	client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
	frrClient := client.ApiV1beta1().FRRConfigurations("frrNamespace")
	bad := (&testFRRConfig{Name: "bad-template", Namespace: "frrNamespace", NodeSelector: map[string]string{corev1.LabelHostname: "node1"},
		Routers: []*testRouter{{ASN: 1, VRF: "unrelated", Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth0"}}}}}).FRRConfiguration()
	_, err := frrClient.Create(ctx, bad, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	tt.waitForFRRCache()
	ra := (&testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true,
		TargetVRF: unsafeInputValue, NetworkSelector: map[string]string{"color": "green"}}).RouteAdvertisements()
	ra.UID, ra.Generation = "uid-ra", 1
	tt.updateRA(ra)
	annotations := func(name string) map[string]string {
		nad, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read annotations on NAD %s/%s", name, name)
		return nad.Annotations
	}
	references := func(name string) []string {
		var refs []string
		g.Expect(json.Unmarshal([]byte(annotations(name)[types.OvnRouteAdvertisementsKey]), &refs)).To(gomega.Succeed())
		return refs
	}
	nadClient := tt.fakeClientset.NetworkAttchDefClient.(interface{ Actions() []ctesting.Action })
	nadActionsBefore := len(nadClient.Actions())
	client.ClearActions()
	failDelete := true
	client.PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
		if failDelete && action.(ctesting.DeleteAction).GetName() == old[1].Name {
			return true, nil, errors.New("injected cleanup delete failure")
		}
		return false, nil, nil
	})
	err = tt.c.reconcile(ctx, "ra")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected cleanup delete failure")))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "cleanup failures must be retried")
	g.Expect(references("blue")).To(gomega.ConsistOf("ra", "other"), "retain references until withdrawal succeeds")
	g.Expect(references("green")).To(gomega.ConsistOf("other"))
	failDelete = false
	tt.failNADPatch.Store(true)
	err = tt.c.reconcile(ctx, "ra")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected NAD patch failure")))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "a failed NAD update must be retried")
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", "bad-template"), "FRR withdrawal completes before NAD cleanup")
	g.Expect(references("blue")).To(gomega.ConsistOf("ra", "other"), "retain the RA reference after the failed NAD patch")
	g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra","other"]`))
	tt.failNADPatch.Store(false)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	g.Expect(references("blue")).To(gomega.ConsistOf("other"))
	g.Expect(references("green")).To(gomega.ConsistOf("other"), "do not publish newly selected networks")
	g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["other"]`))
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", "bad-template"), "withdraw every generated object, keeping the templates")
	status := tt.expectRAStatus(false, true)
	accepted := meta.FindStatusCondition(status.Status.Conditions, conditionTypeAccepted)
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"))
	g.Expect(accepted.Message).To(gomega.ContainSubstring("invalid TargetVRF"))
	for _, action := range client.Actions() {
		g.Expect(action.GetVerb()).NotTo(gomega.BeElementOf("create", "update", "patch"), "rejected input must not publish a valid subset")
	}
	for _, action := range nadClient.Actions()[nadActionsBefore:] {
		g.Expect(action.GetVerb()).NotTo(gomega.BeElementOf("create", "update"))
		if patch, ok := action.(ctesting.PatchAction); ok && patch.GetNamespace() == "green" {
			g.Expect(patch.GetName()).NotTo(gomega.Equal("green"), "a rejected RA must never be published to the green NAD")
		}
	}

	// Correct the inputs while both unready nodes remain unready. Valid
	// nodes must resume, including the newly selected green network.
	bad.Spec.BGP.Routers[0].VRF = ""
	_, err = frrClient.Update(ctx, bad, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	tt.waitForFRRCache()
	ra.Spec.TargetVRF = ""
	ra.Generation++
	tt.updateRA(ra)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	tt.expectRAStatus(true, true)
	generated := tt.generatedConfigs("ra")
	g.Expect(generated).To(gomega.HaveKey("ra/frrConfig/node"))
	g.Expect(generated["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "1.3.0.0/24"))
	g.Expect(references("green")).To(gomega.ConsistOf("ra", "other"))
	g.Expect(annotations("green")[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"uid":"uid-ra","generation":2,"targetVRF":"","egressIP":true}}`), "green must record the corrected RA generation")

	tt.waitForFRRCache()
	ra.Spec.FRRConfigurationSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "peer", Operator: "Unknown"}}
	ra.Generation++
	tt.updateRA(ra)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "reject the malformed selector without retrying")
	status = tt.expectRAStatus(false, true)
	accepted = meta.FindStatusCondition(status.Status.Conditions, conditionTypeAccepted)
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"))
	g.Expect(accepted.Message).To(gomega.ContainSubstring("invalid frrConfigurationSelector"))
	g.Expect(tt.generatedConfigs("ra")).To(gomega.BeEmpty(), "withdraw the previously working advertisements")
	g.Expect(references("green")).To(gomega.ConsistOf("other"), "remove only the rejected RA's reference")
	g.Expect(annotations("green")[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{}`), "withdraw green's pod advertisement intent")
}

func TestControllerKeepsWorkingPeeringTemplate(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	tt.updateRA((&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, AdvertiseEgressIPs: true}).RouteAdvertisements())
	client := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace")
	peer := &testNeighbor{ASN: 2, Address: "192.0.2.1"}
	second := (&testFRRConfig{Name: "second", Namespace: "frrNamespace",
		Routers: []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{peer}}}}).FRRConfiguration()
	second, err := client.Create(ctx, second, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the second peering template")
	tt.waitForFRRCache()
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	tt.expectRAStatus(true, false)
	before := tt.generatedConfigs("ra")
	g.Expect(before).To(gomega.HaveKey("ra/frrConfig/node"), "the first peer must be installed")
	g.Expect(before).To(gomega.HaveKey("ra/second/node"), "the second peer must be installed before withdrawal")

	updatePeer := func(address string, selector metav1.LabelSelector) {
		t.Helper()
		second.Spec.NodeSelector = selector
		second.Spec.BGP.Routers[0].Neighbors[0].Address = address
		second, err = client.Update(ctx, second, metav1.UpdateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "update the second peering template")
		tt.waitForFRRCache()
		g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "reconcile the second peering template")
	}

	invalidSelector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "role", Operator: metav1.LabelSelectorOpIn,
	}}}
	for _, edit := range []struct {
		address    string
		selector   metav1.LabelSelector
		diagnostic string
	}{
		{peer.Address, invalidSelector, "invalid node selector"},
		{"invalid", metav1.LabelSelector{}, "invalid neighbor"},
		{"2001:db8::1", metav1.LabelSelector{}, "no routes or peers match the target VRF"}, // The node has only an IPv4 subnet.
	} {
		updatePeer(edit.address, edit.selector)
		ra := tt.expectRAStatus(true, true)
		configs := tt.generatedConfigs("ra")
		g.Expect(configs["ra/frrConfig/node"]).To(gomega.Equal(before["ra/frrConfig/node"]), "preserve the working peer after %s", edit.diagnostic)
		g.Expect(configs).NotTo(gomega.HaveKey("ra/second/node"), "withdraw the second peer after %s", edit.diagnostic)
		msg := meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded).Message
		g.Expect(msg).To(gomega.ContainSubstring("frrNamespace/second"))
		g.Expect(msg).To(gomega.ContainSubstring(edit.diagnostic))

		updatePeer(peer.Address, metav1.LabelSelector{})
		tt.expectRAStatus(true, false)
		configs = tt.generatedConfigs("ra")
		g.Expect(configs["ra/frrConfig/node"]).To(gomega.Equal(before["ra/frrConfig/node"]), "preserve the working peer through recovery")
		g.Expect(configs).To(gomega.HaveKey("ra/second/node"), "reinstall the second peer after %s", edit.diagnostic)
		g.Expect(configs["ra/second/node"].Spec).To(gomega.Equal(before["ra/second/node"].Spec), "restore the original advertisement")
	}
}

func TestControllerRetriesIndependentTemplateWrites(t *testing.T) {
	for _, verb := range []string{"create", "update"} {
		t.Run(verb, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			tt := newFRRReconcileTest(t, g, "eth0")
			client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
			source := (&testFRRConfig{Name: "first", Namespace: "frrNamespace",
				Routers: []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{{ASN: 2, Interface: "eth1"}}}}}).FRRConfiguration()
			_, err := client.ApiV1beta1().FRRConfigurations(source.Namespace).Create(ctx, source, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create the first peering template")
			tt.waitForFRRCache()
			prefix := "1.1.0.0/24"
			if verb == "update" {
				g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
				tt.waitForFRRCache()
				prefix = "1.1.2.0/24"
				tt.updateNodeSubnets("node", `{"default":"1.1.2.0/24"}`)
			}
			failure := errors.New("template rejected")
			reject := true
			client.PrependReactor(verb, "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
				cfg := action.(interface{ GetObject() runtime.Object }).GetObject().(*frrapi.FRRConfiguration)
				if reject && cfg.Annotations[types.OvnRouteAdvertisementsKey] == "ra/first/node" {
					return true, nil, failure
				}
				return false, nil, nil
			})
			g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed %s", verb)
			// A failed pass counts the cache from before its own writes. The
			// retry runs once they are cached and refreshes the sample.
			tt.waitForFRRCache()
			g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed %s", verb)
			ra := tt.expectRAStatus(true, true)
			g.Expect(meta.FindStatusCondition(ra.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("ra/first/node"))
			configs := tt.generatedConfigs("ra")
			g.Expect(configs).To(gomega.HaveKey("ra/frrConfig/node"), "publish the sibling even when the first write fails")
			g.Expect(configs["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ContainElement(prefix))
			if verb == "create" {
				g.Expect(configs).NotTo(gomega.HaveKey("ra/first/node"))
				expectFRRMetrics(t, "ra", 1, 1, 0)
			} else {
				g.Expect(configs["ra/first/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ContainElement("1.1.0.0/24"), "an unsuccessful update leaves the last API object in place")
				expectFRRMetrics(t, "ra", 2, 2, 0)
			}
			nadKey := config.Default.ClusterDefaultNetworkNAD
			nads := tt.c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadKey.Namespace)
			nad, err := nads.Get(ctx, nadKey.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the default network's intent")
			g.Expect(nad.Annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.ContainSubstring(`"ra"`), "the sibling's configuration must be usable immediately")
			if verb == "update" {
				ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, AdvertiseEgressIPs: true}).RouteAdvertisements()
				ra.Generation = 2
				tt.updateRA(ra)
				tt.updateNodeSubnets("node", `{"default":"1.1.3.0/24"}`)
				prefix = "1.1.3.0/24"
				g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed template")
				g.Expect(tt.generatedConfigs("ra")["ra/frrConfig/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ContainElement(prefix), "an EgressIP-enabled RA can update siblings once withdrawals succeed")
				nad, err = nads.Get(ctx, nadKey.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "read the updated intent for the same target VRF")
				g.Expect(nad.Annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"generation":2,"targetVRF":"","egressIP":true}}`), "publish additive changes while the target VRF stays the same")

				tt.waitForDefaultNADCache()
				podsOnly := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true}).RouteAdvertisements()
				podsOnly.Generation = 3
				tt.updateRA(podsOnly)
				g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed withdrawal")
				g.Expect(tt.defaultNADAnnotations()).To(gomega.Equal(nad.Annotations), "retain EgressIP intent while a template update is rejected")
			}
			reject = false
			tt.waitForFRRCache()
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			tt.expectRAStatus(true, false)
			g.Expect(tt.generatedConfigs("ra")["ra/first/node"].Spec.BGP.Routers[0].Prefixes).To(gomega.ContainElement(prefix), "restore the failed template on retry")
			expectFRRMetrics(t, "ra", 2, 1, 0)
		})
	}
}

func TestControllerAttemptsAllFRRWithdrawals(t *testing.T) {
	for _, scenario := range []string{"obsolete keys", "duplicate keys", "same-node replacement", "unknown node"} {
		t.Run(scenario, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			duplicates := scenario == "duplicate keys"
			var existing []*testFRRConfig
			for i := range 4 {
				node := fmt.Sprintf("node-%d", i)
				if duplicates || scenario == "same-node replacement" {
					node = "node"
				}
				old := generatedFRRConfig(fmt.Sprintf("ovnk-generated-%d", i), node, true)
				if scenario == "same-node replacement" {
					old.Annotations[types.OvnRouteAdvertisementsKey] = "ra/old-template/node"
				} else if scenario == "unknown node" {
					old.Annotations[types.OvnRouteAdvertisementsKey] = "invalid-key"
				}
				existing = append(existing, old)
			}
			if scenario == "same-node replacement" {
				existing = append(existing, generatedFRRConfig("ovnk-generated-retained", "node", true))
			}
			tt := newFRRReconcileTest(t, g, "eth0", existing...)
			client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
			desired := generatedFRRConfig("ovnk-generated-new", "node", true).FRRConfiguration()
			if scenario == "same-node replacement" {
				desired.Spec.BGP.Routers[0].ASN = 2
			}
			failures := []error{errors.New("first deletion failed"), errors.New("second deletion failed")}
			attempts := 0
			client.PrependReactor("delete", "frrconfigurations", func(ctesting.Action) (bool, runtime.Object, error) {
				attempts++
				if attempts <= len(failures) {
					return true, nil, failures[attempts-1]
				}
				return false, nil, nil
			})
			client.ClearActions()
			result := frrGenerationResult{frrConfigs: []*frrapi.FRRConfiguration{desired}}
			changed, err := tt.c.updateFRRConfigurations(ctx, "ra", &result)
			g.Expect(changed).To(gomega.BeTrue())
			for _, failure := range failures {
				g.Expect(errors.Is(err, failure)).To(gomega.BeTrue())
			}
			if duplicates {
				g.Expect(attempts).To(gomega.Equal(3), "retain one desired object and attempt every duplicate deletion")
			} else {
				g.Expect(attempts).To(gomega.Equal(4))
				if scenario == "obsolete keys" {
					current, err := client.ApiV1beta1().FRRConfigurations(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(current).To(gomega.Equal(desired), "another node's failed withdrawal must not block this configuration")
				} else {
					for _, action := range client.Actions() {
						g.Expect(action.GetVerb()).NotTo(gomega.BeElementOf("create", "update"), "wait for withdrawals that could conflict with this node")
					}
					if scenario == "same-node replacement" {
						current, err := client.ApiV1beta1().FRRConfigurations(desired.Namespace).Get(ctx, "ovnk-generated-retained", metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred(), "keep the node's current configuration until its obsolete template is removed")
						g.Expect(current.Spec.BGP.Routers[0].ASN).To(gomega.Equal(uint32(1)))
					}
				}
			}
			tt.waitForFRRCache()
			// Failed objects remain retryable; after success the desired config is published.
			_, err = tt.c.updateFRRConfigurations(ctx, "ra", &result)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			// A failed pass samples the cache as it catches up. Only the
			// successful retry gives a definite count.
			expectFRRMetrics(t, "ra", 1, 1, 0)
			remaining := tt.generatedConfigs("ra")
			key := desired.Annotations[types.OvnRouteAdvertisementsKey]
			g.Expect(remaining).To(gomega.HaveLen(1))
			g.Expect(remaining).To(gomega.HaveKey(key))
			g.Expect(remaining[key].Spec).To(gomega.Equal(desired.Spec))
			if !duplicates {
				g.Expect(remaining[key]).To(gomega.Equal(desired))
			}
		})
	}
}

// Deletion needs no prefix ownership. A generation read failure, however,
// must leave the existing state alone.
func TestWholeWithdrawalRetries(t *testing.T) {
	for _, scenario := range []string{"withdrawal", "generation read failure"} {
		t.Run(scenario, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			old := generatedFRRConfig("ovnk-generated-old", "node", true)
			other := generatedFRRConfig("ovnk-generated-other", "node", true)
			other.Labels[types.OvnRouteAdvertisementsKey] = "other"
			other.Annotations[types.OvnRouteAdvertisementsKey] = "other/frrConfig/node"
			tt := newFRRReconcileTest(t, g, "eth0", old, other)
			client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
			nadClient := tt.fakeClientset.NetworkAttchDefClient.(*nadfake.Clientset)
			client.ClearActions()
			nadClient.ClearActions()
			result := frrGenerationResult{err: errConfig}
			if scenario == "generation read failure" {
				result.err = errors.New("injected generation read failure")
			}
			_, err := tt.c.reconcileRouteAdvertisements(ctx, "ra", nil, &result)
			for _, action := range append(client.Actions(), nadClient.Actions()...) {
				g.Expect(action.GetVerb()).NotTo(gomega.BeElementOf("get", "list"), "withdrawal must use the informer cache for %s", action.GetResource())
			}
			if scenario == "generation read failure" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected")))
				g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "read failures must remain retryable even when withdrawing an invalid RA")
				_, getErr := client.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
				g.Expect(getErr).NotTo(gomega.HaveOccurred())
				g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra","other"]`))
				_, err = tt.c.reconcileRouteAdvertisements(ctx, "ra", nil, &frrGenerationResult{err: errConfig})
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = client.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
			remaining, err := client.ApiV1beta1().FRRConfigurations(other.Namespace).Get(ctx, other.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(remaining.Spec).To(gomega.Equal(other.FRRConfiguration().Spec))
			g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["other"]`))
		})
	}
}

func TestRollbackAfterNADPatchFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := t.Context()
	tt := newFRRReconcileTest(t, g, "eth0")
	source := (&testFRRConfig{Name: "frrConfig", Namespace: "frrNamespace", Routers: []*testRouter{
		{ASN: 1, VRF: "blue", Neighbors: []*testNeighbor{{ASN: 2, Interface: "eth0"}}},
		{ASN: 1, VRF: "red", Neighbors: []*testNeighbor{{ASN: 2, Interface: "eth1"}}},
	}}).FRRConfiguration()
	_, err := tt.c.frrClient.ApiV1beta1().FRRConfigurations(source.Namespace).Update(ctx, source, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "make both target VRFs available")
	tt.waitForFRRCache()
	reconcileVRF := func(vrf string, generation int64) error {
		t.Helper()
		ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, TargetVRF: vrf}).RouteAdvertisements()
		ra.Generation = generation
		tt.updateRA(ra)
		return tt.c.reconcile(ctx, "ra")
	}
	expectExport := func(vrf string) {
		t.Helper()
		configs := tt.generatedConfigs("ra")
		g.Expect(configs).To(gomega.HaveKey("ra/frrConfig/node"))
		g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(configs["ra/frrConfig/node"], vrf, "1.1.0.0/24")).To(gomega.BeTrue(), "FRRConfiguration must export the pod subnet in VRF %s", vrf)
	}
	g.Expect(reconcileVRF("blue", 1)).To(gomega.Succeed(), "publish the initial intent")
	tt.waitForDefaultNADCache()
	tt.waitForFRRCache()

	// Freeze the FRR cache at blue. The fake client does not assign
	// resource versions, so supply the version returned by the red update.
	stale := tt.frrCacheWithout("")
	tt.c.frrLister = frrlisters.NewFRRConfigurationLister(stale)
	tt.c.pendingFRR = newPendingWrites(stale)
	version := 1
	tt.fakeClientset.FRRClient.(Fake).PrependReactor("update", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
		cfg := action.(ctesting.UpdateAction).GetObject().(*frrapi.FRRConfiguration)
		version++
		cfg.ResourceVersion = fmt.Sprint(version)
		return false, nil, nil
	})
	tt.failNADPatch.Store(true)
	g.Expect(reconcileVRF("red", 2)).To(gomega.MatchError(gomega.ContainSubstring("injected NAD patch failure")))
	expectExport("red")
	g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"generation":1,"targetVRF":"blue"}}`), "the failed patch leaves the older intent in place")

	// The rollback runs once the informer shows the red export. Observe the
	// API at the moment the NAD advances so a later FRR write cannot hide
	// incorrect ordering.
	tt.failNADPatch.Store(false)
	g.Expect(reconcileVRF("blue", 3)).To(gomega.Succeed(), "defer rollback while the cache still shows blue")
	expectExport("red")
	g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"generation":1,"targetVRF":"blue"}}`), "the held reconcile must not advance NAD intent")
	red := tt.generatedConfigs("ra")["ra/frrConfig/node"]
	g.Expect(stale.Update(red)).To(gomega.Succeed(), "deliver the red export before resuming the rollback")
	tt.fakeClientset.NetworkAttchDefClient.(Fake).PrependReactor("patch", "network-attachment-definitions", func(ctesting.Action) (bool, runtime.Object, error) {
		expectExport("blue")
		return false, nil, nil
	})
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "restore the original export before publishing the rollback")
	g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"ra":{"generation":3,"targetVRF":"blue"}}`), "publish the rollback generation")
}

func TestPartialDeselectionWithdrawsBeforeClearingNAD(t *testing.T) {
	const (
		gen1       = `{"ra":{"uid":"uid-ra","generation":1,"targetVRF":""}}`
		gen2Red    = `{"ra":{"uid":"uid-ra","generation":2,"targetVRF":"red"}}`
		gen2Egress = `{"ra":{"uid":"uid-ra","generation":2,"targetVRF":"","egressIP":true}}`
		gen3       = `{"ra":{"uid":"uid-ra","generation":3,"targetVRF":""}}`
	)
	for _, tc := range []struct{ name, verb, oldVRF, priorIntent string }{
		{"delete obsolete template", "delete", "red", gen2Red},
		{"change target VRF", "update", "red", gen2Red},
		{"drop EgressIP in the same VRF", "update", "", gen2Egress},
		{"change target VRF with a legacy NAD reference", "update", "red", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := t.Context()
			old := generatedFRRConfig("ovnk-generated-old", "node", true)
			old.Routers[0].VRF = tc.oldVRF
			old.RawConfig = ""
			old.Routers[0].Prefixes = []string{"10.200.1.0/24", "10.201.1.0/24"}
			if tc.verb == "delete" {
				old.Annotations[types.OvnRouteAdvertisementsKey] = "ra/blue-peers/node"
			}
			if tc.oldVRF == "" {
				old.Routers[0].Prefixes = append(old.Routers[0].Prefixes, "192.0.2.10/32")
			}
			old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
			tt := newFRRReconcileTest(t, g, "eth0", old)
			// The RA drops blue and starts advertising the default network.
			// Red keeps its pod routes, but its previous export must change.
			tt.setDefaultNADReferences(`["other"]`)
			nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
			tt.c.nm = nm
			redBefore := map[string]map[string]string{
				"red":     {types.OvnRouteAdvertisementsKey: `["ra"]`},
				"lagging": {},
			}
			// During migration only red/red has a legacy reference. Publishing
			// intent in the new namespace would switch the whole network's VRF.
			if tc.priorIntent != "" {
				redBefore["red"][types.OvnRouteAdvertisementsIntentKey] = tc.priorIntent
				redBefore["lagging"] = map[string]string{
					types.OvnRouteAdvertisementsKey:       `["ra"]`,
					types.OvnRouteAdvertisementsIntentKey: gen1,
				}
			}
			for namespace, annotations := range redBefore {
				tt.addNetwork(nm, &testNAD{Name: "red", Namespace: namespace, Network: util.GenerateCUDNNetworkName("red"),
					Topology: types.Layer3Topology, Transport: types.NetworkTransportNoOverlay, Subnet: "10.201.0.0/16/24", Labels: map[string]string{"color": "red"},
					Annotations: annotations})
			}
			redAnnotations := func(namespace string) map[string]string {
				t.Helper()
				nad, err := tt.c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(ctx, "red", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "read red's NAD in namespace %s", namespace)
				return nad.Annotations
			}
			tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24","cluster_udn_red":"10.201.1.0/24"}`)
			ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, NetworkSelector: map[string]string{"color": "red"}}).RouteAdvertisements()
			ra.UID, ra.Generation = "uid-ra", 3
			tt.updateRA(ra)
			blue := (&testNAD{Name: "blue", Namespace: "tenant", Network: util.GenerateCUDNNetworkName("blue"),
				Topology: types.Layer3Topology, Subnet: "10.200.0.0/16", Annotations: map[string]string{
					types.OvnRouteAdvertisementsKey:       `["ra","other"]`,
					types.OvnRouteAdvertisementsIntentKey: `{"ra":{"targetVRF":"red"},"other":{"targetVRF":"green"}}`,
				}}).NAD()
			nads := tt.c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(blue.Namespace)
			_, err := nads.Create(ctx, blue, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "seed blue's previously published intent")
			g.Eventually(func() (*nadtypes.NetworkAttachmentDefinition, error) {
				return tt.c.nadLister.NetworkAttachmentDefinitions(blue.Namespace).Get(blue.Name)
			}, 5*time.Second).Should(gomega.Equal(blue), "observe blue's NAD")

			client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
			failure := errors.New("injected " + tc.verb + " failure")
			reject := true
			client.PrependReactor(tc.verb, "frrconfigurations", func(ctesting.Action) (bool, runtime.Object, error) {
				if reject {
					return true, nil, failure
				}
				return false, nil, nil
			})
			g.Expect(errors.Is(tt.c.reconcile(ctx, "ra"), failure)).To(gomega.BeTrue(), "retry the failed withdrawal")
			tt.expectRAStatus(true, true)
			g.Expect(tt.defaultNADAnnotations()[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(gen3), "publish the independent default network's intent despite the failed withdrawal")
			cfg, err := client.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the export left by the failed withdrawal")
			g.Expect(cfg.Spec).To(gomega.Equal(old.FRRConfiguration().Spec), "the previous VRF and prefixes are still configured")
			for namespace, before := range redBefore {
				after := redAnnotations(namespace)
				g.Expect(after[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(before[types.OvnRouteAdvertisementsKey]), "retain red's references in namespace %s", namespace)
				g.Expect(after[types.OvnRouteAdvertisementsIntentKey]).To(gomega.Equal(before[types.OvnRouteAdvertisementsIntentKey]), "defer red's intent change in namespace %s", namespace)
			}
			current, err := nads.Get(ctx, blue.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read blue's NAD after the failed withdrawal")
			g.Expect(current.Annotations).To(gomega.Equal(blue.Annotations), "retain blue's reference and intent until its export is withdrawn")

			// Observe the FRR object at the moment the retry patches blue's NAD.
			tt.fakeClientset.NetworkAttchDefClient.(*nadfake.Clientset).PrependReactor("patch", "network-attachment-definitions", func(action ctesting.Action) (bool, runtime.Object, error) {
				if action.GetNamespace() == blue.Namespace && action.(ctesting.PatchAction).GetName() == blue.Name {
					cfg, err := client.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
					if tc.verb == "delete" {
						g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "delete blue's export before clearing its NAD")
					} else {
						g.Expect(err).NotTo(gomega.HaveOccurred(), "retain the shared export")
						g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "10.201.1.0/24"), "withdraw blue and publish the selected networks before clearing blue's NAD")
					}
				}
				return false, nil, nil
			})
			reject = false
			tt.waitForDefaultNADCache()
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed(), "retry the withdrawal")
			configs := tt.generatedConfigs("ra")
			g.Expect(configs).To(gomega.HaveLen(1), "withdraw blue's obsolete configuration")
			g.Expect(configs).To(gomega.HaveKey("ra/frrConfig/node"), "publish the selected networks")
			g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(configs["ra/frrConfig/node"], "", "10.201.1.0/24")).To(gomega.BeTrue(), "export red's pod subnet through the default VRF")
			for namespace := range redBefore {
				g.Expect(redAnnotations(namespace)[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(gen3), "publish red's new intent in namespace %s after recovery", namespace)
			}
			current, err = nads.Get(ctx, blue.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read blue's NAD after withdrawal")
			g.Expect(current.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.MatchJSON(`["other"]`), "remove only the deselected RA's reference")
			g.Expect(current.Annotations[types.OvnRouteAdvertisementsIntentKey]).To(gomega.MatchJSON(`{"other":{"targetVRF":"green"}}`), "preserve the other RA's intent")
		})
	}
}

// A reconcile must not compare desired state with a cache that lacks the
// RouteAdvertisements' own last writes.
func TestPendingWritesHoldReconcile(t *testing.T) {
	previousEVPN := config.OVNKubernetesFeature.EnableEVPN
	t.Cleanup(func() { config.OVNKubernetesFeature.EnableEVPN = previousEVPN })
	config.OVNKubernetesFeature.EnableEVPN = false
	g := gomega.NewWithT(t)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	queue := &controllerutil.FakeController{}
	frrLister := frrlisters.NewFRRConfigurationLister(indexer)
	nadStore := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	nadLister := nadlisters.NewNetworkAttachmentDefinitionLister(nadStore)
	c := Controller{
		frrLister:    frrLister,
		nadLister:    nadLister,
		raController: queue,
		pendingFRR:   newPendingWrites(indexer),
		pendingNAD:   newPendingWrites(nadStore),
	}
	cached := &frrapi.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: "frr", Name: "updated", UID: "updated", ResourceVersion: "1"}}
	deleted := &frrapi.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: "frr", Name: "deleted", UID: "deleted", ResourceVersion: "1"}}
	g.Expect(indexer.Add(cached)).To(gomega.Succeed())
	g.Expect(indexer.Add(deleted)).To(gomega.Succeed())

	written := cached.DeepCopy()
	written.ResourceVersion = "2"
	c.pendingFRR.written("ra", written)
	c.pendingFRR.deleted("ra", deleted)
	g.Expect(c.pendingFRR.waiting("other")).To(gomega.BeFalse(), "another RA is not held")
	g.Expect(c.reconcile(t.Context(), "ra")).To(gomega.Succeed())
	g.Expect(queue.Reconciles).To(gomega.HaveLen(1), "retry the cache check without publishing new state")
	g.Expect(queue.Reconciles[0]).To(gomega.Equal("After:ra"))

	// A stale event does not release the RA; the written version does.
	queue.Reconciles = nil
	ra, _, _ := c.pendingFRR.observed(cached)
	g.Expect(ra).To(gomega.BeEmpty())
	g.Expect(c.frrConfigurationNeedsUpdate(cached, written)).To(gomega.BeFalse(), "our own write needs no cluster-wide reconcile")
	g.Expect(queue.Reconciles).To(gomega.Equal([]string{"Reconcile:ra"}), "wake the held RA")
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeTrue(), "the deleted object is still cached")
	g.Expect(indexer.Delete(deleted)).To(gomega.Succeed())
	queue.Reconciles = nil
	g.Expect(c.reconcileFRRConfiguration(t.Context(), "frr/deleted")).To(gomega.Succeed())
	g.Expect(queue.Reconciles).To(gomega.Equal([]string{"Reconcile:ra"}), "our own deletion only wakes the held RA")
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeFalse())

	// An RA that is not held is not reconciled for its own write.
	queue.Reconciles = nil
	written = written.DeepCopy()
	written.ResourceVersion = "3"
	c.pendingFRR.written("ra", written)
	g.Expect(c.frrConfigurationNeedsUpdate(cached, written)).To(gomega.BeFalse())
	g.Expect(queue.Reconciles).To(gomega.BeEmpty())

	// A relist can skip our version. A later one releases the RA, but is
	// someone else's change and is not ignored.
	c.pendingFRR.written("ra", written)
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeTrue())
	later := written.DeepCopy()
	later.ResourceVersion = "10"
	ra, held, own := c.pendingFRR.observed(later)
	g.Expect(ra).To(gomega.Equal("ra"))
	g.Expect(held).To(gomega.BeTrue())
	g.Expect(own).To(gomega.BeFalse())
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeFalse())

	// An event delivered before its write was recorded is found in the cache.
	g.Expect(indexer.Update(written)).To(gomega.Succeed())
	c.pendingFRR.written("ra", written)
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeFalse())

	// The object's entire lifetime can fall between two watch connections.
	// A relist past the create proves the missing object was also deleted.
	missing := written.DeepCopy()
	missing.Name, missing.ResourceVersion = "missed", "12"
	c.pendingFRR.written("ra", missing)
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeTrue(), "absence in an older cache must not release the RA")
	g.Expect(indexer.Replace(nil, "13")).To(gomega.Succeed(), "relist without the created object")
	g.Expect(c.pendingFRR.waiting("ra")).To(gomega.BeFalse(), "a newer relist confirms the object is gone")
}

type replacingNetworkManager struct {
	*networkmanager.FakeNetworkManager
	previous util.NetInfo
}

func (nm *replacingNetworkManager) GetNetwork(name string) util.NetInfo {
	if nm.previous != nil && name == nm.previous.GetNetworkName() {
		previous := nm.previous
		nm.previous = nil
		return previous
	}
	return nm.FakeNetworkManager.GetNetwork(name)
}

func TestNetworkReadinessUsesSelectedNetwork(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false
	nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
	networkName := util.GenerateCUDNNetworkName("production-blue-network")
	tt.addNetwork(nm, &testNAD{Name: "blue", Namespace: "blue", Network: networkName,
		Topology: types.Layer3Topology, Subnet: "1.3.0.0/16/24", Labels: map[string]string{"color": "blue"}})
	previous := util.NewMutableNetInfo(nm.GetNetwork(networkName))
	previous.SetNetworkID(7)
	replacement := util.NewMutableNetInfo(previous)
	replacement.SetNetworkID(12)
	nm.PrimaryNetworks["blue"] = replacement
	// Selection sees the old network; any later lookup sees its replacement.
	// The long CUDN name makes its source VRF depend on the network ID, while
	// the target VRF and pod prefixes stay unchanged across the replacement.
	tt.c.nm = &replacingNetworkManager{FakeNetworkManager: nm, previous: previous}
	tt.updateNodeSubnets("node", fmt.Sprintf(`{"default":"1.1.0.0/24",%q:"1.3.0.0/24"}`, networkName))
	ra := (&testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"color": "blue"}}).RouteAdvertisements()
	ra.UID, ra.Generation = "ra-uid", 1
	tt.updateRA(ra)

	for _, network := range []util.NetInfo{previous, replacement} {
		g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
		configs := tt.generatedConfigs("ra")
		g.Expect(configs).To(gomega.HaveLen(1))
		cfg := configs["ra/frrConfig/node"]
		g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(cfg, "", "1.3.0.0/24")).To(gomega.BeTrue())
		g.Expect(cfg.Spec.BGP.Routers).To(gomega.ConsistOf(
			gomega.HaveField("VRF", gomega.Equal("")),
			gomega.HaveField("VRF", gomega.Equal(util.GetNetworkVRFName(network))),
		))
		g.Expect(cfg.Spec.BGP.Routers).To(gomega.ContainElement(gomega.And(
			gomega.HaveField("VRF", gomega.Equal("")),
			gomega.HaveField("Imports", gomega.ConsistOf(frrapi.Import{VRF: util.GetNetworkVRFName(network)})),
		)))
		tt.waitForFRRCache()
	}
}

func newNetworkReadinessTest(t *testing.T, g *gomega.WithT) (*frrReconcileTest, *networkmanager.FakeNetworkManager, *ratypes.RouteAdvertisements) {
	t.Helper()
	tt := newFRRReconcileTest(t, g, "eth0")
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false
	nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
	tt.c.nm = nm
	tt.addNetwork(nm, &testNAD{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
		Topology: types.Layer3Topology, Subnet: "1.3.0.0/16/24", Labels: map[string]string{"color": "blue", "selected": "true"}})
	tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24","cluster_udn_blue":"1.3.0.0/24"}`)
	ra := (&testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"color": "blue"}}).RouteAdvertisements()
	ra.UID, ra.Generation = "ra-uid", 1
	tt.updateRA(ra)
	return tt, nm, ra
}

func TestAutoCoverageFollowsRenderedRouters(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt, nm, ra := newNetworkReadinessTest(t, g)
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	nm.SetNodeActive("cluster_udn_blue", "node", true)
	other := (&testNode{Name: "other", SubnetsAnnotation: `{"default":"1.1.1.0/24"}`}).Node()
	_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, other, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create a second node where blue is inactive")
	g.Eventually(func() error { _, err := tt.c.nodeLister.Get(other.Name); return err }, 5*time.Second).Should(gomega.Succeed(), "wait for the second node")
	ra.Spec.TargetVRF = "auto"
	tt.updateRA(ra)
	client := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace")
	source, err := client.Get(ctx, "frrConfig", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "get the default network peer")
	source.Spec.BGP.Routers = append(source.Spec.BGP.Routers, frrapi.Router{ASN: 1, VRF: "blue", Neighbors: []frrapi.Neighbor{{ASN: 1}}})
	for _, peer := range []string{"2001:db8::1", "192.0.2.1"} {
		source.Spec.BGP.Routers[1].Neighbors[0].Address = peer
		source, err = client.Update(ctx, source, metav1.UpdateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "configure blue's peer %s", peer)
		tt.waitForFRRCache()
		g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
		cfg := tt.generatedConfigs("ra")["ra/frrConfig/node"]
		g.Expect(cfg).NotTo(gomega.BeNil(), "retain the working default VRF")
		g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(cfg, "", "1.1.0.0/24")).To(gomega.BeTrue())
		missing := peer == "2001:db8::1"
		g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(cfg, "blue", "1.3.0.0/24")).To(gomega.Equal(!missing))
		status := tt.expectRAStatus(true, missing)
		if missing {
			g.Expect(meta.FindStatusCondition(status.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("missing [cluster_udn_blue]"))
		}
	}
	before := tt.generatedConfigs("ra")["ra/frrConfig/other"]
	g.Expect(before).NotTo(gomega.BeNil(), "the default-only node has a working configuration")
	source.Spec.BGP.Routers[1].Neighbors[0].DualStackAddressFamily = true
	_, err = client.Update(ctx, source, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "set an unsupported peer option in blue's VRF")
	tt.waitForFRRCache()
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	tt.expectRAStatus(true, true)
	configs := tt.generatedConfigs("ra")
	g.Expect(configs).NotTo(gomega.HaveKey("ra/frrConfig/node"), "withdraw the template where blue is active")
	g.Expect(configs["ra/frrConfig/other"]).To(gomega.Equal(before), "blue's invalid peer leaves the default-only node untouched")
}

func TestAutoVRFKeepsIndependentSubnet(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt, _, ra := newNetworkReadinessTest(t, g)
	ra.Spec.TargetVRF = "auto"
	tt.updateRA(ra)
	blue := (&testFRRConfig{Name: "blue-only", Namespace: "frrNamespace", Routers: []*testRouter{
		{ASN: 1, VRF: "blue", Neighbors: []*testNeighbor{{ASN: 2, Address: "192.0.2.2"}}},
	}}).FRRConfiguration()
	_, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(blue.Namespace).Create(ctx, blue, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create blue's independent peering template")
	tt.waitForFRRCache()
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	before := tt.generatedConfigs("ra")
	g.Expect(before).To(gomega.HaveLen(2), "both peering configurations start with an allocation")
	for _, subnets := range []string{`{"default":"1.1.0.0/24"}`, `{"default":"1.1.0.0/24","cluster_udn_blue":["bad-cidr"]}`} {
		tt.waitForFRRCache()
		tt.updateNodeSubnets("node", subnets)
		g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
		configs := tt.generatedConfigs("ra")
		g.Expect(configs).To(gomega.HaveLen(1), "withdraw only blue's configuration")
		g.Expect(configs["ra/frrConfig/node"]).To(gomega.Equal(before["ra/frrConfig/node"]), "keep the working default VRF intact")
		status := tt.expectRAStatus(true, true)
		g.Expect(meta.FindStatusCondition(status.Status.Conditions, conditionTypeDegraded).Message).To(gomega.ContainSubstring("cluster_udn_blue"))
	}
	tt.waitForFRRCache()
	tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24","cluster_udn_blue":"1.3.0.0/24"}`)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	tt.expectRAStatus(true, false)
	configs := tt.generatedConfigs("ra")
	g.Expect(configs).To(gomega.HaveKey("ra/blue-only/node"))
	g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(configs["ra/blue-only/node"], "blue", "1.3.0.0/24")).To(gomega.BeTrue(), "restore blue's advertisement after allocation recovers")
}

func TestNetworkReadinessWithdrawsOnFailedAddition(t *testing.T) {
	for _, specChange := range []bool{false, true} {
		name := "new matching network"
		if specChange {
			name = "RA selector update"
		}
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			tt, nm, ra := newNetworkReadinessTest(t, g)
			createGreen := func() {
				tt.addNetwork(nm, &testNAD{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"),
					Topology: types.Layer3Topology, Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}})
			}
			selectGreen := func() {
				ra.Spec.NetworkSelectors = (&testRA{SelectsDefault: true, NetworkSelector: map[string]string{"selected": "true"}}).RouteAdvertisements().Spec.NetworkSelectors
				ra.Generation++
				tt.updateRA(ra)
			}
			if specChange {
				createGreen()
			} else {
				selectGreen()
			}
			other := (&testRA{Name: "other", AdvertisePods: true, SelectsDefault: true}).RouteAdvertisements()
			_, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(ctx, other, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create independent RA %s", other.Name)
			g.Eventually(func() error { _, err := tt.c.raLister.Get("other"); return err }, 5*time.Second).Should(gomega.Succeed(), "wait for RA %s in the cache", other.Name)
			g.Expect(tt.c.reconcile(ctx, "other")).To(gomega.Succeed())
			otherConfigs := tt.generatedConfigs("other")
			g.Expect(otherConfigs).To(gomega.HaveLen(1))
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			tt.waitForFRRCache()
			key := "ra/frrConfig/node"
			g.Expect(tt.generatedConfigs("ra")[key].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "1.3.0.0/24"))
			if specChange {
				selectGreen()
			} else {
				createGreen()
			}
			for range 2 {
				g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
				g.Expect(tt.generatedConfigs("ra")).To(gomega.BeEmpty(), "green's failure withdraws the other networks for this RA/node too")
				tt.waitForFRRCache()
				tt.expectRAStatus(true, true)
				g.Expect(tt.c.reconcile(ctx, "other")).To(gomega.Succeed())
				g.Expect(tt.generatedConfigs("other")).To(gomega.Equal(otherConfigs), "another RA on this node must continue working")
			}

			tt.updateNodeSubnets("node", `{"default":"1.1.0.0/24","cluster_udn_blue":"1.3.0.0/24","cluster_udn_green":"1.4.0.0/24"}`)
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			tt.expectRAStatus(true, false)
			g.Expect(tt.generatedConfigs("ra")[key].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "1.3.0.0/24", "1.4.0.0/24"), "recovery advertises the existing networks again")
			tt.waitForFRRCache()
			source := (&testFRRConfig{Name: "extra", Namespace: "frrNamespace", Routers: []*testRouter{{ASN: 1, Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth1"}}}}}).FRRConfiguration()
			_, err = tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(source.Namespace).Create(ctx, source, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			tt.waitForFRRCache()
			tt.updateNodeSubnets("node", `{"default":"1.1.1.0/24","cluster_udn_blue":"1.3.0.0/24","cluster_udn_green":"1.4.0.0/24"}`)
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			expanded := tt.generatedConfigs("ra")
			g.Expect(expanded).To(gomega.HaveLen(2))
			g.Expect(expanded[key].Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.1.0/24", "1.3.0.0/24", "1.4.0.0/24"))
			g.Expect(expanded).To(gomega.HaveKey("ra/extra/node"))
		})
	}
}

func TestControllerWithdrawsFailedNodeRegardlessOfNetworkActivity(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("blue remains active=%t", active), func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			tt, nm, _ := newNetworkReadinessTest(t, g)
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			blue := util.GenerateCUDNNetworkName("blue")
			nm.SetNodeActive(blue, "node", true)
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			configs := tt.generatedConfigs("ra")
			g.Expect(configs).To(gomega.HaveLen(1))
			original := configs["ra/frrConfig/node"]
			g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(original, "", "1.3.0.0/24")).To(gomega.BeTrue())
			tt.waitForFRRCache()

			// Blue stays selected but may have lost its last pod. Missing
			// default-network allocations must withdraw the shared config
			// whether blue is still active or not.
			nm.SetNodeActive(blue, "node", active)
			tt.updateNodeSubnets("node", `{}`)
			for range 2 {
				g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
				tt.expectRAStatus(true, true)
				g.Expect(tt.generatedConfigs("ra")).To(gomega.BeEmpty())
				tt.waitForFRRCache()
			}
		})
	}
}

func TestCleanupFailureStatusAtRetryExhaustion(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	raClient := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements()
	previous := tt.expectRAStatus(true, false)

	// Missing allocations must still reach withdrawal.
	g.Expect(tt.generatedConfigs("ra")).To(gomega.HaveLen(1))
	tt.waitForFRRCache()
	tt.updateNodeSubnets("node", `{}`)
	ra := previous.DeepCopy()
	ra.Generation++
	ra.Spec.TargetVRF = unsafeInputValue
	tt.updateRA(ra)

	tt.fakeClientset.FRRClient.(*frrfake.Clientset).PrependReactor("delete", "frrconfigurations", func(ctesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected withdrawal failure")
	})

	// Exercise the failed attempts, then invoke the same callback the
	// workqueue uses when it exhausts its retry budget.
	var err error
	for range controllerutil.DefaultMaxAttempts + 1 {
		err = tt.c.reconcile(ctx, "ra")
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected withdrawal failure")))
	}
	beforeCallback, getErr := raClient.Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(getErr).NotTo(gomega.HaveOccurred())
	g.Expect(beforeCallback.Status.Conditions).To(gomega.Equal(previous.Status.Conditions), "rejected cleanup returns before status publication")
	g.Expect(tt.c.handleReconcileError(ctx, "ra", err)).To(gomega.Succeed())
	afterCallback := tt.expectRAStatus(false, true)
	degraded := meta.FindStatusCondition(afterCallback.Status.Conditions, conditionTypeDegraded)
	g.Expect(degraded.Message).To(gomega.ContainSubstring("injected withdrawal failure"))
}
