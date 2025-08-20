package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/applyconfiguration/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	generateName = "ovnk-generated-"
	fieldManager = "clustermanager-routeadvertisements-controller"
)

var (
	errConfig      = errors.New("configuration error")
	errPending     = errors.New("configuration pending")
	cudnController = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
)

// Controller reconciles RouteAdvertisements
type Controller struct {
	wf *factory.WatchFactory

	eipLister       egressiplisters.EgressIPLister
	frrLister       frrlisters.FRRConfigurationLister
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	nodeLister      corelisters.NodeLister
	raLister        ralisters.RouteAdvertisementsLister
	namespaceLister corelisters.NamespaceLister

	frrClient frrclientset.Interface
	nadClient nadclientset.Interface
	raClient  raclientset.Interface

	eipController  controllerutil.Controller
	frrController  controllerutil.Controller
	nadController  controllerutil.Controller
	nodeController controllerutil.Controller
	raController   controllerutil.Controller
	nsController   controllerutil.Controller

	nm networkmanager.Interface

	raCacheMutex   sync.RWMutex
	raNetworkCache map[string]*raNetworkCacheEntry
}

// NewController builds a controller that reconciles RouteAdvertisements
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	c := &Controller{
		wf:              wf,
		eipLister:       wf.EgressIPInformer().Lister(),
		frrLister:       wf.FRRConfigurationsInformer().Lister(),
		nadLister:       wf.NADInformer().Lister(),
		nodeLister:      wf.NodeCoreInformer().Lister(),
		raLister:        wf.RouteAdvertisementsInformer().Lister(),
		namespaceLister: wf.NamespaceInformer().Lister(),
		frrClient:       ovnClient.FRRClient,
		nadClient:       ovnClient.NetworkAttchDefClient,
		raClient:        ovnClient.RouteAdvertisementsClient,
		nm:              nm,
		raNetworkCache:  make(map[string]*raNetworkCacheEntry),
	}

	handleError := func(key string, errorstatus error) error {
		ra, err := c.raLister.Get(key)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot get RouteAdvertisements %q to report error %v in status: %v",
				key,
				errorstatus,
				err,
			)
		}

		return c.updateRAStatus(ra, false, errorstatus)
	}

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: raNeedsUpdate,
		HandleError:    handleError,
	}
	c.raController = controllerutil.NewController("clustermanager routeadvertisements controller", raConfig)

	frrConfig := &controllerutil.ControllerConfig[frrtypes.FRRConfiguration]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileFRRConfiguration,
		Threadiness:    1,
		Informer:       wf.FRRConfigurationsInformer().Informer(),
		Lister:         wf.FRRConfigurationsInformer().Lister().List,
		ObjNeedsUpdate: frrConfigurationNeedsUpdate,
	}
	c.frrController = controllerutil.NewController("clustermanager routeadvertisements frrconfiguration controller", frrConfig)

	nadConfig := &controllerutil.ControllerConfig[nadtypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNAD,
		Threadiness:    1,
		Informer:       wf.NADInformer().Informer(),
		Lister:         wf.NADInformer().Lister().List,
		ObjNeedsUpdate: nadNeedsUpdate,
	}
	c.nadController = controllerutil.NewController("clustermanager routeadvertisements nad controller", nadConfig)

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(_ string) error { c.raController.ReconcileAll(); return nil },
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController("clustermanager routeadvertisements node controller", nodeConfig)

	eipConfig := &controllerutil.ControllerConfig[eiptypes.EgressIP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileEgressIPs,
		Threadiness:    1,
		Informer:       wf.EgressIPInformer().Informer(),
		Lister:         wf.EgressIPInformer().Lister().List,
		ObjNeedsUpdate: egressIPNeedsUpdate,
	}
	c.eipController = controllerutil.NewController("clustermanager routeadvertisements egressip controller", eipConfig)

	nsConfig := &controllerutil.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileEgressIPs,
		Threadiness:    1,
		Informer:       wf.NamespaceInformer().Informer(),
		Lister:         wf.NamespaceInformer().Lister().List,
		ObjNeedsUpdate: nsNeedsUpdate,
	}
	c.nsController = controllerutil.NewController("clustermanager routeadvertisements namespace controller", nsConfig)

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager routeadvertisements started")
	return controllerutil.Start(
		c.eipController,
		c.frrController,
		c.nadController,
		c.nodeController,
		c.nsController,
		c.raController,
	)
}

func (c *Controller) Stop() {
	controllerutil.Stop(
		c.eipController,
		c.frrController,
		c.nadController,
		c.nodeController,
		c.nsController,
		c.raController,
	)
	klog.Infof("Cluster manager routeadvertisements stopped")
}

func (c *Controller) ReconcileNetwork(_ string, old, new util.NetInfo) {
	// This controller already listens on NAD events but there is two additional
	// scenarios we need to cover for:
	// - for newly created networks, we need to wait until network manager is
	// aware of them.
	// - if the namespaces served by a network change.
	oldNamespaces, newNamespaces := sets.New[string](), sets.New[string]()
	if old != nil {
		oldNamespaces.Insert(old.GetNADNamespaces()...)
	}
	if new != nil {
		newNamespaces.Insert(new.GetNADNamespaces()...)
	}
	if new != nil && !newNamespaces.Equal(oldNamespaces) {
		// we use one of the NADs of the network to reconcile it
		nads := new.GetNADs()
		if len(nads) > 0 {
			c.nadController.Reconcile(nads[0])
		}
		// if the namespaces served by a network changed, it is possible that
		// those namespaces are served or no longer served by the default
		// network, so reconcile it as well
		c.nadController.Reconcile(config.Kubernetes.OVNConfigNamespace + "/" + types.DefaultNetworkName)
	}
}

// Reconcile RouteAdvertisements. For each selected FRRConfiguration and node,
// another FRRConfiguration might be generated:
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will announce from the node the selected network prefixes for that node on
// the matching target VRFs.
//
// - If EgressIP advertisements are enabled, the generated FRRConfiguration will
// announce from the node the EgressIPs allocated to it on the matching target
// VRFs. Selected EgressIP are those that serve the same namespaces as the
// selected networks. Target VRF `auto` is not supported for EgressIPs.
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will import the target VRFs on the selected networks as required.
//
// - The generated FRRConfiguration will be labeled with the RouteAdvertisements
// name and annotated with an internal key to facilitate updating it when
// needed.
//
// The controller will also annotate the NADs of the selected networks with the
// RouteAdvertisements that select them to facilitate processing for downstream
// zone/node controllers.
//
// Finally, it will update the status of the RouteAdvertisements.
//
// The controller processes selected events of RouteAdvertisements,
// FRRConfigurations, Nodes, EgressIPs, NADs and namespaces.
func (c *Controller) reconcile(name string) error {
	startTime := time.Now()
	klog.V(5).Infof("Syncing routeadvertisements %q", name)
	defer func() {
		klog.V(4).Infof("Finished syncing routeadvertisements %q, took %v", name, time.Since(startTime))
	}()

	ra, err := c.raLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get RouteAdvertisements %q: %w", name, err)
	}

	hadUpdates, err := c.reconcileRouteAdvertisements(name, ra)
	if err != nil && !errors.Is(err, errConfig) && !errors.Is(err, errPending) {
		return fmt.Errorf("failed to reconcile RouteAdvertisements %q: %w", name, err)
	}

	return c.updateRAStatus(ra, hadUpdates, err)
}

func (c *Controller) reconcileRouteAdvertisements(name string, ra *ratypes.RouteAdvertisements) (bool, error) {
	c.invalidateRACache(name)
	// generate FRRConfigurations
	frrConfigs, nads, cfgErr := c.generateFRRConfigurations(ra)
	if cfgErr != nil && !errors.Is(cfgErr, errPending) {
		return false, cfgErr
	}

	// update them
	hadFRRConfigUpdates, err := c.updateFRRConfigurations(name, frrConfigs)
	if err != nil {
		return false, fmt.Errorf("failed updating FRRConfigurations for RouteAdvertisements %q: %w", name, err)
	}

	// annotate NADs
	hadNADUpdates, err := c.updateNADs(name, nads)
	if err != nil {
		return false, fmt.Errorf("failed annotating NADs for RouteAdvertisements %q: %w", name, err)
	}

	return hadFRRConfigUpdates || hadNADUpdates, cfgErr
}

// selectedNetworks is a helper struct that stores information about networks
// that have been selected by a RouteAdvertisements. It is important that prefix
// lists are ordered to generate consistent FRRConfigurations.
type selectedNetworks struct {
	// networkSet is a set of selected network names
	networkSet sets.Set[string]
	// networks is an ordered list of selected network names
	networks []string
	// vrfs is an ordered list of selected networks VRF's
	vrfs []string
	// networkVRFs is a mapping of VRF to corresponding network
	networkVRFs map[string]string
	// subnets is an ordered list of all selected network subnets
	subnets []string
	// hostSubnets is an ordered list of all selected network subnets specific to a node
	hostSubnets []string
	// networkSubnets is a map of selected network names to their ordered network subnets
	networkSubnets map[string][]string
	// hostNetworkSubnets is a map of selected network names to their ordered network subnets specific for a node
	hostNetworkSubnets map[string][]string
	// prefixLength is a map of selected network to their prefix length
	prefixLength map[string]uint32
	// networkType is a map of selected network to their topology
	networkTopology map[string]string
}

// generateFRRConfigurations generates FRRConfigurations for the route
// advertisements. Also returns the selected network NADs.
func (c *Controller) generateFRRConfigurations(ra *ratypes.RouteAdvertisements) ([]*frrtypes.FRRConfiguration, []*nadtypes.NetworkAttachmentDefinition, error) {
	if ra == nil {
		return nil, nil, nil
	}

	advertisements := sets.New(ra.Spec.Advertisements...)
	if advertisements.Has(ratypes.EgressIP) && ra.Spec.TargetVRF == "auto" {
		return nil, nil, fmt.Errorf("%w: advertising EgressIP not supported with TargetVRF set to 'auto'", errConfig)
	}

	// if we are matching on the well known default network label, create an
	// internal nad for it if it doesn't exist
	nads, err := c.getSelectedNADs(ra.Spec.NetworkSelectors)
	if err != nil {
		return nil, nil, err
	}
	if len(nads) == 0 {
		return nil, nil, fmt.Errorf("%w: no networks selected", errPending)
	}

	// validate and gather information about the networks
	selectedNetworks, err := c.getSelectedNetworkInfoSorted(nads, advertisements)
	if err != nil {
		return nil, nil, err
	}

	if err = c.checkSubnetOverlaps(ra, selectedNetworks); err != nil {
		return nil, nil, err
	}

	// gather selected nodes
	nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
	if err != nil {
		return nil, nil, err
	}
	if !nodeSelector.Empty() && advertisements.Has(ratypes.PodNetwork) {
		return nil, nil, fmt.Errorf("%w: node selector has to select all nodes if pod network is advertised", errConfig)
	}
	nodes, err := c.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("%w: no nodes selected", errPending)
	}
	// prepare a map of selected nodes to the FRRConfigurations that apply to
	// them
	nodeToFRRConfig := map[string][]*frrtypes.FRRConfiguration{}
	for _, node := range nodes {
		nodeToFRRConfig[node.Name] = nil
	}

	// gather selected FRRConfigurations, map them to the selected nodes
	frrSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.FRRConfigurationSelector)
	if err != nil {
		return nil, nil, err
	}
	frrConfigs, err := c.frrLister.List(frrSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(frrConfigs) == 0 {
		return nil, nil, fmt.Errorf("%w: no FRRConfigurations selected", errPending)
	}
	for _, frrConfig := range frrConfigs {
		if strings.HasPrefix(frrConfig.Name, generateName) {
			klog.V(4).Infof("Skipping FRRConfiguration %q selected by RouteAdvertisements %q as it was generated by ovn-kubernetes", frrConfig.Name, ra.Name)
			continue
		}
		nodeSelector, err := metav1.LabelSelectorAsSelector(&frrConfig.Spec.NodeSelector)
		if err != nil {
			return nil, nil, err
		}
		nodes, err := c.nodeLister.List(nodeSelector)
		if err != nil {
			return nil, nil, err
		}
		for _, node := range nodes {
			if _, selected := nodeToFRRConfig[node.Name]; !selected {
				// this RouteAdvertisements does not select this node, skip
				continue
			}
			nodeToFRRConfig[node.Name] = append(nodeToFRRConfig[node.Name], frrConfig)
		}
	}

	// helper to gather host subnets and cache during reconcile
	// TODO perhaps cache across reconciles as well
	hostSubnets := map[string]map[string][]string{}
	getHostSubnets := func(nodeName string, network string) ([]string, error) {
		if _, parsed := hostSubnets[nodeName]; !parsed {
			node, err := c.nodeLister.Get(nodeName)
			if err != nil {
				return nil, err
			}
			subnets, err := util.ParseNodeHostSubnetsAnnotation(node)
			if err != nil {
				return nil, fmt.Errorf("%w: waiting for subnet annotation to be set for node %q: %w", errConfig, nodeName, err)
			}
			hostSubnets[nodeName] = make(map[string][]string, len(subnets))
			for network, subnet := range subnets {
				hostSubnets[nodeName][network] = util.StringSlice(subnet)
			}
		}
		return hostSubnets[nodeName][network], nil
	}

	// helper to gather egress ips and cache during reconcile
	// TODO perhaps cache across reconciles as well
	var eipsByNodesByNetworks map[string]map[string]sets.Set[string]
	getEgressIPsByNode := func(nodeName string) (map[string]sets.Set[string], error) {
		if eipsByNodesByNetworks == nil {
			eipsByNodesByNetworks, err = c.getEgressIPsByNodesByNetworks(selectedNetworks.networkSet)
			if err != nil {
				return nil, err
			}
		}
		return eipsByNodesByNetworks[nodeName], nil
	}

	// helper to gather the following prefixes:
	//  - EgressIPs
	//  - host subnets for networks with networkTopology layer3
	//  - network subnets for networks with networkTopology layer2
	getPrefixes := func(nodeName, network, networkTopology string, networkSubnets []string) ([]string, error) {
		// gather host subnets
		var subnets []string
		if advertisements.Has(ratypes.PodNetwork) {
			if networkTopology == types.Layer2Topology {
				subnets = networkSubnets
				if len(subnets) == 0 {
					return nil, fmt.Errorf("%w: no layer2 subnets found", errConfig)
				}
			} else {
				subnets, err = getHostSubnets(nodeName, network)
				if err != nil || len(subnets) == 0 {
					return nil, fmt.Errorf("%w: will wait for subnet annotation to be set for node %q and network %q: %w", errConfig, nodeName, network, err)
				}
			}

		}
		// gather EgressIPs
		var eips []string
		if advertisements.Has(ratypes.EgressIP) {
			eipsByNode, err := getEgressIPsByNode(nodeName)
			if err != nil {
				return nil, err
			}
			eips = eipsByNode[network].UnsortedList()
		}

		prefixes := make([]string, 0, len(subnets)+len(eips))
		prefixes = append(prefixes, subnets...)
		prefixes = append(prefixes, eips...)
		return prefixes, nil
	}

	generated := []*frrtypes.FRRConfiguration{}
	for nodeName, frrConfigs := range nodeToFRRConfig {
		// reset node specific information
		selectedNetworks.hostNetworkSubnets = map[string][]string{}
		selectedNetworks.hostSubnets = []string{}

		// gather node specific information
		for _, network := range selectedNetworks.networks {
			selectedNetworks.hostNetworkSubnets[network], err = getPrefixes(nodeName, network,
				selectedNetworks.networkTopology[network], selectedNetworks.networkSubnets[network])
			if err != nil {
				return nil, nil, err
			}
			selectedNetworks.hostSubnets = append(selectedNetworks.hostSubnets, selectedNetworks.hostNetworkSubnets[network]...)
			// ordered
			slices.Sort(selectedNetworks.hostNetworkSubnets[network])
		}
		// order, dedup
		selectedNetworks.hostSubnets = sets.List(sets.New(selectedNetworks.hostSubnets...))

		// if there is no prefixes to advertise for this node, skip it
		if len(selectedNetworks.hostSubnets) == 0 {
			continue
		}

		matchedNetworks := sets.New[string]()
		for _, frrConfig := range frrConfigs {
			// generate FRRConfiguration for each source FRRConfiguration/node combination
			new, err := c.generateFRRConfiguration(
				ra,
				frrConfig,
				nodeName,
				selectedNetworks,
				matchedNetworks,
			)
			if err != nil {
				return nil, nil, err
			}
			if new == nil {
				// if we got nil, we didn't match any VRF
				return nil, nil, fmt.Errorf("%w: FRRConfiguration %q selected for node %q has no VRF matching the RouteAdvertisements target VRF or any selected network",
					errConfig, frrConfig.Name, nodeName)
			}
			generated = append(generated, new)
		}
		// check that we matched all the selected networks on 'auto'
		if ra.Spec.TargetVRF == "auto" && !matchedNetworks.HasAll(selectedNetworks.networks...) {
			return nil, nil, fmt.Errorf("%w: selected FRRConfigurations for node %q don't match all selected networks with target VRF 'auto'", errConfig, nodeName)
		}
	}

	return generated, nads, nil
}

func (c *Controller) buildSelectedNetworkInfo(nads []*nadtypes.NetworkAttachmentDefinition, advertisements sets.Set[ratypes.AdvertisementType]) (*selectedNetworks, error) {
	selectedNetworks := &selectedNetworks{
		networkSet:      sets.New[string](),
		networkVRFs:     map[string]string{},
		networkSubnets:  map[string][]string{},
		prefixLength:    map[string]uint32{},
		networkTopology: map[string]string{},
	}
	for _, nad := range nads {
		networkName := util.GetAnnotatedNetworkName(nad)
		network := c.nm.GetNetwork(networkName)
		if network == nil {
			// network not yet known by network manager, skip
			continue
		}
		if selectedNetworks.networkSet.Has(networkName) {
			continue
		}
		if !network.IsDefault() && !network.IsPrimaryNetwork() {
			return nil, fmt.Errorf("%w: selected network %q is not the default nor a primary network", errConfig, networkName)
		}
		if network.TopologyType() != types.Layer3Topology && network.TopologyType() != types.Layer2Topology {
			return nil, fmt.Errorf("%w: selected network %q has unsupported topology %q", errConfig, networkName, network.TopologyType())
		}

		if advertisements.Has(ratypes.EgressIP) && network.TopologyType() == types.Layer2Topology {
			return nil, fmt.Errorf("%w: EgressIP advertisement is currently not supported for Layer2 networks, network: %s", errConfig, network.GetNetworkName())
		}

		vrf := util.GetNetworkVRFName(network)
		if vfrNet, hasVFR := selectedNetworks.networkVRFs[vrf]; hasVFR && vfrNet != networkName {
			return nil, fmt.Errorf("%w: vrf %q found to be mapped to multiple networks %v", errConfig, vrf, []string{vfrNet, networkName})
		}
		selectedNetworks.networkSet.Insert(networkName)
		selectedNetworks.vrfs = append(selectedNetworks.vrfs, vrf)
		selectedNetworks.networkVRFs[vrf] = networkName
		selectedNetworks.networkTopology[networkName] = network.TopologyType()
		for _, cidr := range network.Subnets() {
			subnet := cidr.CIDR.String()
			len := uint32(cidr.HostSubnetLength)
			selectedNetworks.networkSubnets[networkName] = append(selectedNetworks.networkSubnets[networkName], subnet)
			selectedNetworks.subnets = append(selectedNetworks.subnets, subnet)
			selectedNetworks.prefixLength[subnet] = len
		}
	}

	return selectedNetworks, nil
}

func (c *Controller) getSelectedNetworkInfoSorted(nads []*nadtypes.NetworkAttachmentDefinition, advertisements sets.Set[ratypes.AdvertisementType]) (*selectedNetworks, error) {
	selectedNetworks, err := c.buildSelectedNetworkInfo(nads, advertisements)
	if err != nil {
		return nil, err
	}

	// Apply sorting
	selectedNetworks.networks = sets.List(selectedNetworks.networkSet)
	for networkName := range selectedNetworks.networkSubnets {
		// ordered
		slices.Sort(selectedNetworks.networkSubnets[networkName])
	}

	// ordered
	slices.Sort(selectedNetworks.vrfs)
	slices.Sort(selectedNetworks.subnets)

	return selectedNetworks, nil
}

// generateFRRConfiguration generates a FRRConfiguration from a source for a
// specific node. Also fills matchedNetworks with the networks that have a VRF
// that matched any router VRF of the FRRConfiguration.
func (c *Controller) generateFRRConfiguration(
	ra *ratypes.RouteAdvertisements,
	source *frrtypes.FRRConfiguration,
	nodeName string,
	selectedNetworks *selectedNetworks,
	matchedNetworks sets.Set[string],
) (*frrtypes.FRRConfiguration, error) {
	routers := []frrtypes.Router{}

	// go over the source routers
	for i, router := range source.Spec.BGP.Routers {

		targetVRF := ra.Spec.TargetVRF
		var matchedVRF, matchedNetwork string
		var advertisePrefixes []string

		// We will use the router if:
		// - the router VRF matches the target VRF
		// - if the target VRF is 'auto', the router VRF is that of a selected network
		// Prepare each scenario with a switch statement and check after that
		switch {
		case targetVRF == "auto" && router.VRF == "":
			// match on default network/VRF, advertise node prefixes
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
		case targetVRF == "auto":
			// match router.VRF to network.VRF, advertise node prefixes
			matchedVRF = router.VRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
		case targetVRF == "":
			// match on default network/VRF, advertise node prefixes
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostSubnets
		default:
			// match router.VRF to network.VRF, advertise node prefixes
			matchedVRF = targetVRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostSubnets
		}
		if matchedVRF != router.VRF || len(advertisePrefixes) == 0 {
			// either this router VRF does not match the target VRF or we don't
			// have prefixes for it (which might be due to this RA not selecting
			// this network, but not just)
			continue
		}
		matchedNetworks.Insert(matchedNetwork)

		// if this router's VRF matches the target VRF, copy it and set the
		// prefixes as appropriate
		targetRouter := router
		targetRouter.Prefixes = advertisePrefixes
		targetRouter.Neighbors = make([]frrtypes.Neighbor, 0, len(source.Spec.BGP.Routers[i].Neighbors))
		for _, neighbor := range source.Spec.BGP.Routers[i].Neighbors {
			// If MultiProtocol is enabled (default) then a BGP session carries
			// prefixes of both IPv4 and IPv6 families. Our problem is that with
			// an IPv4 session, FRR can incorrectly pick the masquerade IPv6
			// address (instead of the real address) as next hop for IPv6
			// prefixes and that won't work. Note that with a dedicated IPv6
			// session that can't happen since FRR will use the same address
			// that was used to stablish the session. Let's enforce the use of
			// DisableMP for now.
			if !neighbor.DisableMP {
				return nil, fmt.Errorf("%w: DisableMP==false not supported, seen on FRRConfiguration %s/%s neighbor %s",
					errConfig,
					source.Namespace,
					source.Name,
					neighbor.Address,
				)
			}

			isIPV6 := utilnet.IsIPv6String(neighbor.Address)
			advertisePrefixes := util.MatchAllIPNetsStringFamily(isIPV6, advertisePrefixes)
			if len(advertisePrefixes) == 0 {
				continue
			}

			neighbor.ToAdvertise = frrtypes.Advertise{
				Allowed: frrtypes.AllowedOutPrefixes{
					Mode:     frrtypes.AllowRestricted,
					Prefixes: advertisePrefixes,
				},
			}
			targetRouter.Neighbors = append(targetRouter.Neighbors, neighbor)
		}
		if len(targetRouter.Neighbors) == 0 {
			// we ended up with no neighbor
			continue
		}

		// append this router to the list of routers we will include in the
		// generated FRR config and track its index as we might need to add
		// imports to it
		routers = append(routers, targetRouter)
		targetRouterIndex := len(routers) - 1

		// VRFs are isolated in "auto" so no need to handle imports
		if targetVRF == "auto" {
			continue
		}

		// before handling imports, lets normalize the VRF for the default
		// network: when doing imports, the default VRF is is referred to as
		// "default" instead of ""
		if matchedVRF == "" {
			matchedVRF = types.DefaultNetworkName
		}

		// handle imports: when the target VRF is not "auto" we need to leak
		// between the target VRF and the selected networks, reciprocally
		// importing from each
		for _, vrf := range selectedNetworks.vrfs { // ordered
			// skip self
			if vrf == matchedVRF {
				continue
			}

			// import all other selected networks into this router's network.
			routers[targetRouterIndex].Imports = append(routers[targetRouterIndex].Imports, frrtypes.Import{VRF: vrf})

			// add an additional router to import the target VRF into selected
			// network
			importRouter := frrtypes.Router{
				ASN:     router.ASN,
				ID:      router.ID,
				Imports: []frrtypes.Import{{VRF: matchedVRF}},
			}
			if vrf != types.DefaultNetworkName {
				importRouter.VRF = vrf
			}
			routers = append(routers, importRouter)
		}
	}
	if len(routers) == 0 {
		// we ended up with no routers, bail out
		return nil, nil
	}

	new := &frrtypes.FRRConfiguration{}
	new.GenerateName = generateName
	new.Namespace = source.Namespace
	// label the FRRConfigurations with the RA name, we use this to find the
	// existing set of FRRConfigurations that need to be reconciled for a given
	// RA
	new.Labels = map[string]string{
		types.OvnRouteAdvertisementsKey: ra.Name,
	}
	// annotate each generated FRRConfiguration with a unique key
	// (ra/source/node) which is used in the reconciliation to know whether an
	// existing FRRConfiguration should be deleted or not.
	new.Annotations = map[string]string{
		types.OvnRouteAdvertisementsKey: fmt.Sprintf("%s/%s/%s", ra.Name, source.Name, nodeName),
	}
	new.Spec = source.Spec
	new.Spec.BGP.Routers = routers
	new.Spec.NodeSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/hostname": nodeName,
		},
	}

	return new, nil
}

// updateFRRConfigurations updates the FRRConfigurations that apply for a
// RouteAdvertisements. It fetches existing FRRConfigurations by label and
// indexes them by the annotated key. Then compares this state with desired
// state and creates, updates or deletes the FRRConfigurations accordingly.
func (c *Controller) updateFRRConfigurations(ra string, frrConfigurations []*frrtypes.FRRConfiguration) (bool, error) {
	var hadUpdates bool

	// fetch the currently existing FRRConfigurations for this
	// RouteAdvertisements
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{types.OvnRouteAdvertisementsKey: ra},
	})
	if err != nil {
		return hadUpdates, err
	}
	frrConfigs, err := c.frrLister.List(selector)
	if err != nil {
		return hadUpdates, err
	}
	// map them by our internal unique key
	existing := make(map[string][]*frrtypes.FRRConfiguration, len(frrConfigs))
	for _, frrConfig := range frrConfigs {
		key := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]
		if key == "" {
			continue
		}
		existing[key] = append(existing[key], frrConfig)
	}

	// go through the FRRConfigurations that should exist for this
	// RouteAdvertisements
	for _, newFRRConfig := range frrConfigurations {
		key := newFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]
		oldFRRConfigs := existing[key]

		if len(oldFRRConfigs) == 0 {
			// does not exist, create
			_, err := c.frrClient.ApiV1beta1().FRRConfigurations(newFRRConfig.Namespace).Create(
				context.Background(),
				newFRRConfig,
				metav1.CreateOptions{
					FieldManager: fieldManager,
				},
			)
			if err != nil {
				return hadUpdates, err
			}
			hadUpdates = true
			continue
		}

		// If it already exists, update. Unexpected user actions can lead us to
		// have multiple FRRConfigurations with the same key, in that case we
		// pick one to update and delete the rest.
		oldFRRConfig := oldFRRConfigs[len(oldFRRConfigs)-1]
		existing[key] = oldFRRConfigs[:len(oldFRRConfigs)-1]

		// no changes needed so skip
		if reflect.DeepEqual(newFRRConfig.Spec, oldFRRConfig.Spec) {
			continue
		}

		// otherwise update
		newFRRConfig.Name = oldFRRConfig.Name
		newFRRConfig.ResourceVersion = oldFRRConfig.ResourceVersion
		_, err := c.frrClient.ApiV1beta1().FRRConfigurations(newFRRConfig.Namespace).Update(
			context.Background(),
			newFRRConfig,
			metav1.UpdateOptions{
				FieldManager: fieldManager,
			},
		)
		if err != nil {
			return hadUpdates, err
		}
		hadUpdates = true
	}

	// delete FRRConfigurations that should not exist
	for _, obsoleteFRRConfigs := range existing {
		for _, obsoleteFRRConfig := range obsoleteFRRConfigs {
			err := c.frrClient.ApiV1beta1().FRRConfigurations(obsoleteFRRConfig.Namespace).Delete(
				context.Background(),
				obsoleteFRRConfig.Name,
				metav1.DeleteOptions{},
			)
			if err != nil && !apierrors.IsNotFound(err) {
				return hadUpdates, err
			}
			hadUpdates = true
		}
	}

	return hadUpdates, nil
}

// updateNADs updates the annotation of the NADs that apply for a
// RouteAdvertisements. It iterates all the existing NADs updating the
// annotation accordingly, adding or removing the RouteAdvertisements reference
// as needed.
func (c *Controller) updateNADs(ra string, nads []*nadtypes.NetworkAttachmentDefinition) (bool, error) {
	var hadUpdates bool
	selected := sets.New[string]()
	for _, nad := range nads {
		selected.Insert(nad.Namespace + "/" + nad.Name)
	}

	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return hadUpdates, err
	}

	k := kube.KubeOVN{
		NADClient: c.nadClient,
	}

	// go through all the NADs and update the annotation adding or removing the
	// reference to this RouteAdvertisements as required
	for _, nad := range nads {
		var ras []string

		if nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
			err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &ras)
			if err != nil {
				return hadUpdates, err
			}
		}

		raSet := sets.New(ras...)
		nadName := nad.Namespace + "/" + nad.Name
		if selected.Has(nadName) {
			raSet.Insert(ra)
			selected.Delete(nadName)
		} else {
			raSet.Delete(ra)
		}

		if len(ras) == raSet.Len() {
			continue
		}

		nadRAjson, err := json.Marshal(raSet.UnsortedList())
		if err != nil {
			return hadUpdates, err
		}

		err = k.SetAnnotationsOnNAD(
			nad.Namespace,
			nad.Name,
			map[string]string{
				types.OvnRouteAdvertisementsKey: string(nadRAjson),
			},
			fieldManager,
		)
		if err != nil {
			return hadUpdates, fmt.Errorf("failed to annotate NAD %q: %w", nad.Name, err)
		}

		hadUpdates = true
	}
	if selected.Len() != 0 {
		return hadUpdates, fmt.Errorf("failed to annotate NADs that were not found %v", selected.UnsortedList())
	}

	return hadUpdates, nil
}

// updateRAStatus update the RouteAdvertisements 'Accepted' status according to
// the error provided
func (c *Controller) updateRAStatus(ra *ratypes.RouteAdvertisements, hadUpdates bool, err error) error {
	if ra == nil {
		return nil
	}

	var updateStatus bool
	condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
	switch {
	case condition == nil:
		fallthrough
	case condition.ObservedGeneration != ra.Generation:
		fallthrough
	case (err == nil) != (condition.Status == metav1.ConditionTrue):
		fallthrough
	case hadUpdates:
		updateStatus = true
	}
	if !updateStatus {
		return nil
	}

	status := "Accepted"
	cstatus := metav1.ConditionTrue
	reason := "Accepted"
	msg := "ovn-kubernetes cluster-manager validated the resource and requested the necessary configuration changes"
	if err != nil {
		status = fmt.Sprintf("Not Accepted: %v", err)
		cstatus = metav1.ConditionFalse
		msg = err.Error()
		switch {
		case errors.Is(err, errConfig):
			reason = "ConfigurationError"
		case errors.Is(err, errPending):
			reason = "ConfigurationPending"
		default:
			reason = "InternalError"
		}
	}

	_, err = c.raClient.K8sV1().RouteAdvertisements().ApplyStatus(
		context.Background(),
		raapply.RouteAdvertisements(ra.Name).WithStatus(
			raapply.RouteAdvertisementsStatus().WithStatus(status).WithConditions(
				metaapply.Condition().
					WithType("Accepted").
					WithStatus(cstatus).
					WithLastTransitionTime(metav1.NewTime(time.Now())).
					WithReason(reason).
					WithMessage(msg).
					WithObservedGeneration(ra.Generation),
			),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply status for RouteAdvertisements %q: %w", ra.Name, err)
	}

	return nil
}

func (c *Controller) getSelectedNADs(networkSelectors apitypes.NetworkSelectors) ([]*nadtypes.NetworkAttachmentDefinition, error) {
	var selected []*nadtypes.NetworkAttachmentDefinition
	for _, networkSelector := range networkSelectors {
		switch networkSelector.NetworkSelectionType {
		case apitypes.DefaultNetwork:
			// if we are selecting the default networkdefault network label,
			// make sure a NAD exists for it
			nad, err := util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient)
			if err != nil {
				return nil, fmt.Errorf("failed to get/create default network NAD: %w", err)
			}
			selected = append(selected, nad)
		case apitypes.ClusterUserDefinedNetworks:
			nadSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return nil, err
			}
			nads, err := c.nadLister.List(nadSelector)
			if err != nil {
				return nil, err
			}
			for _, nad := range nads {
				// check this NAD is controlled by a CUDN
				controller := metav1.GetControllerOfNoCopy(nad)
				isCUDN := controller != nil && controller.Kind == cudnController.Kind && controller.APIVersion == cudnController.GroupVersion().String()
				if !isCUDN {
					continue
				}
				selected = append(selected, nad)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported network selection type %s", errConfig, networkSelector.NetworkSelectionType)
		}
	}

	return selected, nil
}

// getEgressIPsByNodesByNetworks iterates all existing egress IPs that apply to
// any of the provided networks and returns a "node -> network -> eips"
// map.
func (c *Controller) getEgressIPsByNodesByNetworks(networks sets.Set[string]) (map[string]map[string]sets.Set[string], error) {
	eipsByNodesByNetworks := map[string]map[string]sets.Set[string]{}
	addEgressIPsByNodesByNetwork := func(eipsByNodes map[string]string, network string) {
		for node, eip := range eipsByNodes {
			if eipsByNodesByNetworks[node] == nil {
				eipsByNodesByNetworks[node] = map[string]sets.Set[string]{}
			}
			if eipsByNodesByNetworks[node][network] == nil {
				eipsByNodesByNetworks[node][network] = sets.New[string]()
			}
			eipsByNodesByNetworks[node][network].Insert(eip)
		}
	}

	addEgressIPsByNodesByNetworkSelector := func(eipsByNodes map[string]string, namespaceSelector *metav1.LabelSelector) error {
		nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			return err
		}
		selected, err := c.namespaceLister.List(nsSelector)
		if err != nil {
			return err
		}
		for _, namespace := range selected {
			namespaceNetwork := c.nm.GetActiveNetworkForNamespaceFast(namespace.Name)
			networkName := namespaceNetwork.GetNetworkName()
			if networks.Has(networkName) {
				addEgressIPsByNodesByNetwork(eipsByNodes, networkName)
			}
		}
		return nil
	}

	eips, err := c.eipLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, eip := range eips {
		eipsByNodes := make(map[string]string, len(eip.Status.Items))
		for _, item := range eip.Status.Items {
			// skip unassigned EIPs
			if item.EgressIP == "" || item.Node == "" {
				continue
			}

			ip := item.EgressIP + util.GetIPFullMaskString(item.EgressIP)
			eipsByNodes[item.Node] = ip
		}
		if len(eipsByNodes) == 0 {
			continue
		}

		err = addEgressIPsByNodesByNetworkSelector(eipsByNodes, &eip.Spec.NamespaceSelector)
		if err != nil {
			return nil, err
		}
	}

	return eipsByNodesByNetworks, nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(fieldManager, managedFields)
}

func raNeedsUpdate(oldObj, newObj *ratypes.RouteAdvertisements) bool {
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation
}

func frrConfigurationNeedsUpdate(oldObj, newObj *frrtypes.FRRConfiguration) bool {
	// ignore if it was created or updated by ourselves
	if newObj != nil && isOwnUpdate(newObj.ManagedFields) {
		return false
	}
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func nadNeedsUpdate(oldObj, newObj *nadtypes.NetworkAttachmentDefinition) bool {
	// ignore if it updated by ourselves
	if newObj != nil && isOwnUpdate(newObj.ManagedFields) {
		return false
	}
	nadSupported := func(nad *nadtypes.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return false
		}
		network, err := util.ParseNADInfo(newObj)
		if err != nil {
			return true
		}
		return network.IsDefault() ||
			(network.IsPrimaryNetwork() && (network.TopologyType() == types.Layer3Topology || network.TopologyType() == types.Layer2Topology))
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}

	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		util.NodeSubnetAnnotationChanged(oldObj, newObj) ||
		oldObj.Annotations[util.OvnNodeIfAddr] != newObj.Annotations[util.OvnNodeIfAddr]
}

func egressIPNeedsUpdate(oldObj, newObj *eiptypes.EgressIP) bool {
	if oldObj != nil && newObj != nil {
		return !reflect.DeepEqual(oldObj.Status, newObj.Status) || !reflect.DeepEqual(oldObj.Spec.NamespaceSelector, newObj.Spec.NamespaceSelector)
	}
	if oldObj != nil && len(oldObj.Status.Items) > 0 {
		return true
	}
	if newObj != nil && len(newObj.Status.Items) > 0 {
		return true
	}
	return false
}

func nsNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	// we only care about label changes, added/deleted namespaces served by a
	// UDN will already be reflected in a network update
	return oldObj != nil && newObj != nil && !reflect.DeepEqual(oldObj.Labels, newObj.Labels)
}

func (c *Controller) reconcileFRRConfiguration(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed spliting FRFConfiguration reconcile key %q: %v", key, err)
		return nil
	}

	frrConfig, err := c.frrLister.FRRConfigurations(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// safest approach is to reconcile all existing RouteAdvertisements (could
	// be potentially avoided with additional caching but let's hold it until we
	// know we need it)
	c.raController.ReconcileAll()

	// on startup, we might be syncing a FRRConfiguration generated by us for a
	// RouteAdvertisements that does not exist, so make sure to reconcile it so
	// that the FRRConfiguration is deleted if needed
	if frrConfig != nil && frrConfig.Labels[types.OvnRouteAdvertisementsKey] != "" {
		c.raController.Reconcile(frrConfig.Labels[types.OvnRouteAdvertisementsKey])
	}

	return nil
}

func (c *Controller) reconcileNAD(key string) error {
	c.invalidateCacheForNAD(key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed spliting NAD reconcile key %q: %v", key, err)
		return nil
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// safest approach is to reconcile all existing RouteAdvertisements (could be potentially
	// avoided with additional caching but let's hold it until we know we need
	// it)
	c.raController.ReconcileAll()

	// on startup, we might be syncing a NAD annotated by us with a
	// RouteAdvertisements that does not longer exist, so make sure to reconcile
	// annotated RouteAdvertisements so that the annotation is updated
	// accordingly
	if nad != nil && nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
		var ras []string
		err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &ras)
		if err != nil {
			return err
		}
		for _, ra := range ras {
			c.raController.Reconcile(ra)
		}
	}

	return nil
}

func (c *Controller) reconcileEgressIPs(string) error {
	// reconcile RAs that advertise EIPs
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, ra := range ras {
		if sets.New(ra.Spec.Advertisements...).Has(ratypes.EgressIP) {
			c.raController.Reconcile(ra.Name)
		}
	}

	return nil
}

// vrfNetworkIPNet represents an IPNet associated with a specific network in a VRF
type vrfNetworkIPNet struct {
	network string
	ipNet   *net.IPNet
}

// checkSubnetOverlaps validates that subnets from the current RA don't overlap
// with any existing subnets in the same VRFs, both within the RA itself and
// across different RAs
func (c *Controller) checkSubnetOverlaps(ra *ratypes.RouteAdvertisements, selectedNetworks *selectedNetworks) error {
	networkVRFsIPNets, err := createNetworkVRFsIPNetsMap(ra.Spec.TargetVRF, selectedNetworks)
	if err != nil {
		return fmt.Errorf("error parsing subnets for RouteAdvertisement %q: %w", ra.Name, err)
	}

	// Check for overlaps within each VRF network
	for vrf, vrfNetworkIPNets := range networkVRFsIPNets {
		// Check for overlaps within current RA (only between different networks)
		if overlappingSubnets := checkVRFNetworkIPNetSliceOverlaps(vrfNetworkIPNets); len(overlappingSubnets) > 0 {
			slices.Sort(overlappingSubnets)
			return fmt.Errorf("%w: overlapping CIDR detected within RouteAdvertisement %q in VRF %q: %v", errConfig, ra.Name, vrf, overlappingSubnets)
		}
		// Check for overlaps between current RA and other RAs in the same VRF
		if err := c.checkCrossRAOverlaps(ra, vrf, vrfNetworkIPNets); err != nil {
			return err
		}
	}

	return nil
}

// createNetworkVRFsIPNetsMap creates a map of VRF -> vrfNetworkIPNet from selectedNetworks
func createNetworkVRFsIPNetsMap(targetVRF string, selectedNetworks *selectedNetworks) (map[string][]vrfNetworkIPNet, error) {
	networkVRFsIPNets := make(map[string][]vrfNetworkIPNet)
	if targetVRF == "auto" {
		for networkVRF, network := range selectedNetworks.networkVRFs {
			subnets := selectedNetworks.networkSubnets[network]
			ipNets, err := util.ParseIPNets(subnets)
			if err != nil {
				return nil, fmt.Errorf("failed to parse subnets for VRF %s: %w", networkVRF, err)
			}
			// Create vrfNetworkIPNet entries with network information
			var vrfNetworkIPNets []vrfNetworkIPNet
			for _, ipNet := range ipNets {
				vrfNetworkIPNets = append(vrfNetworkIPNets, vrfNetworkIPNet{
					network: network,
					ipNet:   ipNet,
				})
			}
			networkVRFsIPNets[networkVRF] = vrfNetworkIPNets
		}
	} else {
		// For non-auto targetVRF, iterate over each network's subnets individually
		var vrfNetworkIPNets []vrfNetworkIPNet
		for network, networkSubnets := range selectedNetworks.networkSubnets {
			ipNets, err := util.ParseIPNets(networkSubnets)
			if err != nil {
				return nil, fmt.Errorf("failed to parse subnets for network %s in VRF %s: %w", network, targetVRF, err)
			}
			// Create vrfNetworkIPNet entries with correct network information
			for _, ipNet := range ipNets {
				vrfNetworkIPNets = append(vrfNetworkIPNets, vrfNetworkIPNet{
					network: network,
					ipNet:   ipNet,
				})
			}
		}
		networkVRFsIPNets[targetVRF] = vrfNetworkIPNets
	}
	return networkVRFsIPNets, nil
}

// checkVRFNetworkIPNetSliceOverlaps checks for subnet overlap within a list of vrfNetworkIPNet,
// but only reports conflicts if the overlapping subnets belong to different networks
func checkVRFNetworkIPNetSliceOverlaps(vrfNetworkIPNets []vrfNetworkIPNet) []string {
	for i, subnet1 := range vrfNetworkIPNets {
		for j := i + 1; j < len(vrfNetworkIPNets); j++ {
			subnet2 := vrfNetworkIPNets[j]
			// Only check for overlap if the subnets belong to different networks
			if subnet1.network != subnet2.network {
				overlaps := util.IPNetOverlaps(subnet1.ipNet, subnet2.ipNet)
				if len(overlaps) > 0 {
					return []string{subnet1.ipNet.String(), subnet2.ipNet.String()}
				}
			}
		}
	}
	return []string{}
}

type raNetworkCacheEntry struct {
	vrfSubnetsMap map[string][]vrfNetworkIPNet
	dependentNADs sets.Set[string]
}

func (c *Controller) getCachedRANetworkInfo(raName string) (*raNetworkCacheEntry, bool) {
	c.raCacheMutex.RLock()
	defer c.raCacheMutex.RUnlock()
	entry, exists := c.raNetworkCache[raName]
	return entry, exists
}

func (c *Controller) setCachedRANetworkInfo(raName string, entry *raNetworkCacheEntry) {
	c.raCacheMutex.Lock()
	defer c.raCacheMutex.Unlock()
	c.raNetworkCache[raName] = entry
}

func (c *Controller) invalidateRACache(raName string) {
	c.raCacheMutex.Lock()
	defer c.raCacheMutex.Unlock()
	delete(c.raNetworkCache, raName)
}

func (c *Controller) invalidateCacheForNAD(nadName string) {
	c.raCacheMutex.Lock()
	defer c.raCacheMutex.Unlock()
	for raName, entry := range c.raNetworkCache {
		if entry.dependentNADs.Has(nadName) {
			delete(c.raNetworkCache, raName)
		}
	}
}

// updateCachedRANetworkInfoEntry builds and caches network info for an RA.
func (c *Controller) updateCachedRANetworkInfoEntry(ra *ratypes.RouteAdvertisements) (*raNetworkCacheEntry, error) {
	nads, err := c.getSelectedNADs(ra.Spec.NetworkSelectors)
	if err != nil {
		return nil, err
	}
	if len(nads) == 0 {
		return nil, nil
	}

	selectedNetworks, err := c.buildSelectedNetworkInfo(nads, sets.New(ra.Spec.Advertisements...))
	if err != nil {
		return nil, err
	}

	dependentNADs := sets.New[string]()
	for _, nad := range nads {
		dependentNADs.Insert(nad.Namespace + "/" + nad.Name)
	}

	vrfIPNetsMap, err := createNetworkVRFsIPNetsMap(ra.Spec.TargetVRF, selectedNetworks)
	if err != nil {
		return nil, err
	}

	cache := &raNetworkCacheEntry{
		vrfSubnetsMap: vrfIPNetsMap,
		dependentNADs: dependentNADs,
	}
	c.setCachedRANetworkInfo(ra.Name, cache)
	return cache, nil
}

// checkCrossRAOverlaps checks if subnets from current RA overlap with subnets from other RAs in the same VRF
func (c *Controller) checkCrossRAOverlaps(ra *ratypes.RouteAdvertisements, targetVRF string, currentRAVRFNetworkIPNets []vrfNetworkIPNet) error {
	allRAs, err := c.raLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, otherRA := range allRAs {
		if otherRA.Name == ra.Name {
			continue
		}

		// Skip RAs that target different VRF and not auto
		if otherRA.Spec.TargetVRF != targetVRF && otherRA.Spec.TargetVRF != "auto" {
			continue
		}

		cache, exists := c.getCachedRANetworkInfo(otherRA.Name)
		if !exists {
			var err error
			// We build cache entries on-demand rather than amortizing the cost across RA reconciliations.
			// This ensures deterministic conflict detection during startup scenarios where RAs may be reconciled
			// in any order. The trade-off is that cache invalidation events (e.g., NAD, node changes) cause the
			// next RA reconciliation to pay a higher cost rebuilding multiple cache entries.
			cache, err = c.updateCachedRANetworkInfoEntry(otherRA)
			if err != nil {
				klog.V(4).Infof("Failed to get selected networks from RouteAdvertisement %q for cross overlap check with RouteAdvertisement %q: %v", otherRA.Name, ra.Name, err)
				continue
			}
			if cache == nil {
				continue
			}
		}

		otherVRFNetworkIPNets, hasMatchingVRF := cache.vrfSubnetsMap[targetVRF]
		if !hasMatchingVRF || len(otherVRFNetworkIPNets) == 0 {
			continue
		}

		// Check for overlaps between current RA subnets and other RA subnets in the same VRF
		// Only report conflicts if the overlapping subnets belong to different networks
		for _, currentRASubnet := range currentRAVRFNetworkIPNets {
			for _, otherRASubnet := range otherVRFNetworkIPNets {
				// Skip overlap check if both subnets belong to the same network
				if currentRASubnet.network == otherRASubnet.network {
					continue
				}
				hasOverlap := len(util.IPNetOverlaps(currentRASubnet.ipNet, otherRASubnet.ipNet)) > 0
				if !hasOverlap {
					continue
				}

				// Update otherRA only if status is accepted to avoid infinite reconciliation loops
				if accepted := meta.FindStatusCondition(otherRA.Status.Conditions, "Accepted"); accepted == nil || accepted.Status == metav1.ConditionTrue {
					// Enqueue otherRA for reconciliation
					c.raController.Reconcile(otherRA.Name)
				}

				return fmt.Errorf("%w: overlapping CIDR detected between RouteAdvertisements %q and %q in VRF %q: [%s %s]",
					errConfig, ra.Name, otherRA.Name, targetVRF, currentRASubnet.ipNet.String(), otherRASubnet.ipNet.String())
			}
		}
	}
	return nil
}
