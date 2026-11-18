// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	advertisedNetworkSubnetsControllerName = "advertised-network-subnets"
	// advertisedNetworkSubnetsSyncKey is the single key of the address set
	// reconciler: every relevant NAD event leads to a full sync.
	advertisedNetworkSubnetsSyncKey = "sync"
)

// AdvertisedNetworkSubnetsController keeps the advertised network subnets
// address set equal to the subnets of every advertised default or primary
// network of the cluster. With dynamic UDN allocation only some of these
// networks have a controller running in this zone, while the isolation drop
// ACL must match traffic to and from all of them.
//
// A RouteAdvertisements can only select the default network and the networks
// of ClusterUserDefinedNetworks. Such a network counts as advertised for as
// long as one of its NADs references a RouteAdvertisements, whatever that
// RouteAdvertisements advertises and whether or not it still exists: a subnet
// that is wrongly present only makes isolation stricter, while a missing one
// lets traffic through. Any other primary network is only advertised when its
// transport is no-overlay or EVPN.
type AdvertisedNetworkSubnetsController struct {
	addressSetFactory addressset.AddressSetFactory
	nadLister         nadlisters.NetworkAttachmentDefinitionLister
	networkManager    networkmanager.Interface

	// nadAdvertisements holds, by NAD key, what each NAD contributed in the
	// last sync. Only accessed from sync, which never runs concurrently.
	nadAdvertisements map[string]*nadAdvertisement

	// nadReconciler is registered with the network manager and turns every
	// NAD event into a request for a sync: a burst of them is served by few
	// runs of the single-key reconciler
	nadReconciler   controller.Reconciler
	nadReconcilerID uint64
	reconciler      controller.Reconciler
}

// nadAdvertisement is what a NAD contributes to the advertised subnets of its
// network.
type nadAdvertisement struct {
	// resourceVersion of the NAD this was parsed from
	resourceVersion string
	// network is empty for a NAD that contributes nothing
	network    string
	subnets    []string
	advertised bool
}

// NewAdvertisedNetworkSubnetsController creates the controller that owns the
// contents of the advertised network subnets address set. The address set
// itself is created by ConfigureAdvertisedNetworkIsolation.
func NewAdvertisedNetworkSubnetsController(
	addressSetFactory addressset.AddressSetFactory,
	nadLister nadlisters.NetworkAttachmentDefinitionLister,
	networkManager networkmanager.Interface,
) *AdvertisedNetworkSubnetsController {
	c := &AdvertisedNetworkSubnetsController{
		addressSetFactory: addressSetFactory,
		nadLister:         nadLister,
		networkManager:    networkManager,
	}

	c.reconciler = controller.NewReconciler(advertisedNetworkSubnetsControllerName, &controller.ReconcilerConfig{
		Reconcile:   func(context.Context, string) error { return c.sync() },
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	c.nadReconciler = controller.NewReconciler(advertisedNetworkSubnetsControllerName+"-nad", &controller.ReconcilerConfig{
		Reconcile: func(context.Context, string) error {
			c.reconciler.Reconcile(advertisedNetworkSubnetsSyncKey)
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	return c
}

// Start syncs the address set and starts following NAD events.
func (c *AdvertisedNetworkSubnetsController) Start() error {
	c.nadReconcilerID = c.networkManager.RegisterNADReconciler(c.nadReconciler)
	if err := controller.StartWithInitialSync(c.sync, c.reconciler, c.nadReconciler); err != nil {
		c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
		return fmt.Errorf("failed to start the advertised network subnets sync: %w", err)
	}
	return nil
}

// Stop stops the controller.
func (c *AdvertisedNetworkSubnetsController) Stop() {
	c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
	controller.Stop(c.nadReconciler, c.reconciler)
}

// sync replaces the contents of the advertised network subnets address set
// with the subnets of the networks that are currently advertised.
func (c *AdvertisedNetworkSubnetsController) sync() error {
	desired, err := c.advertisedSubnets()
	if err != nil {
		return fmt.Errorf("failed to compute the advertised network subnets: %w", err)
	}

	addrSetDBIDs := GetAdvertisedNetworkSubnetsAddressSetDBIDs()
	addrSet, err := c.addressSetFactory.EnsureAddressSet(addrSetDBIDs)
	if err != nil {
		return fmt.Errorf("failed to ensure advertised subnets address set %s: %w", addrSetDBIDs, err)
	}
	v4Addrs, v6Addrs := addrSet.GetAddresses()
	current := sets.New(v4Addrs...).Insert(v6Addrs...)
	if current.Equal(desired) {
		return nil
	}

	if err = addrSet.SetAddresses(sets.List(desired)); err != nil {
		return fmt.Errorf("failed to set addresses %v on advertised subnets address set %s: %w",
			sets.List(desired), addrSetDBIDs, err)
	}
	klog.Infof("Advertised network subnets address set updated: added %v, removed %v",
		sets.List(desired.Difference(current)), sets.List(current.Difference(desired)))
	return nil
}

// advertisedSubnets returns the subnets of the default network and of every
// primary network defined by a NAD, as long as they are advertised.
func (c *AdvertisedNetworkSubnetsController) advertisedSubnets() (sets.Set[string], error) {
	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list NADs: %w", err)
	}

	advertisements := make(map[string]*nadAdvertisement, len(nads))
	for _, nad := range nads {
		nadKey := util.GetNADName(nad.Namespace, nad.Name)
		advertisement, known := c.nadAdvertisements[nadKey]
		if !known || advertisement.resourceVersion != nad.ResourceVersion {
			parsed, parseErr := parseNADAdvertisement(nad)
			switch {
			case parseErr == nil:
				advertisement = parsed
			case known && advertisement.network != "":
				// The network manager keeps a network running with its last
				// valid configuration when a NAD update is rejected, and
				// still follows the annotation: keep the subnets the NAD
				// contributed so far.
				klog.Errorf("Keeping the advertised subnets of NAD %s unchanged: %v", nadKey, parseErr)
				kept := *advertisement
				kept.resourceVersion = nad.ResourceVersion
				kept.advertised = kept.advertised || referencesRouteAdvertisements(nad)
				advertisement = &kept
			default:
				advertisement = &nadAdvertisement{resourceVersion: nad.ResourceVersion}
			}
		}
		advertisements[nadKey] = advertisement
	}
	c.nadAdvertisements = advertisements

	// the default network has a NAD only once a RouteAdvertisements selects it
	defaultNetwork := &util.DefaultNetInfo{}
	advertisedNetworks := sets.New[string]()
	if util.IsPodNetworkAdvertisedAtNode(defaultNetwork, "") {
		advertisedNetworks.Insert(types.DefaultNetworkName)
	}
	for _, advertisement := range advertisements {
		if advertisement.advertised {
			advertisedNetworks.Insert(advertisement.network)
		}
	}

	// NADs of the same network normally carry the same subnets; take them
	// all, whichever NAD of the network references a RouteAdvertisements
	subnets := sets.New[string]()
	if advertisedNetworks.Has(types.DefaultNetworkName) {
		for _, subnet := range defaultNetwork.Subnets() {
			subnets.Insert(subnet.CIDR.String())
		}
	}
	for _, advertisement := range advertisements {
		if advertisedNetworks.Has(advertisement.network) {
			subnets.Insert(advertisement.subnets...)
		}
	}
	return subnets, nil
}

// parseNADAdvertisement returns what the NAD contributes: nothing, unless it
// is the NAD of the default network, the NAD of a primary network of a
// ClusterUserDefinedNetwork, or the NAD of a primary network advertised by
// its transport.
func parseNADAdvertisement(nad *nettypes.NetworkAttachmentDefinition) (*nadAdvertisement, error) {
	advertisement := &nadAdvertisement{resourceVersion: nad.ResourceVersion}
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		return nil, fmt.Errorf("failed to parse NAD %s/%s: %w", nad.Namespace, nad.Name, err)
	}
	// true for a network other than the default one only with a no-overlay
	// or EVPN transport
	advertisedByTransport := util.IsPodNetworkAdvertisedAtNode(netInfo, "")
	switch {
	case netInfo.IsDefault():
		defaultNAD := config.Default.ClusterDefaultNetworkNAD
		if nad.Namespace != defaultNAD.Namespace || nad.Name != defaultNAD.Name {
			return advertisement, nil
		}
	case !netInfo.IsPrimaryNetwork():
		return advertisement, nil
	case !isControlledByCUDN(nad) && !advertisedByTransport:
		return advertisement, nil
	}

	advertisement = &nadAdvertisement{
		resourceVersion: nad.ResourceVersion,
		network:         netInfo.GetNetworkName(),
		advertised:      advertisedByTransport || referencesRouteAdvertisements(nad),
	}
	for _, subnet := range netInfo.Subnets() {
		advertisement.subnets = append(advertisement.subnets, subnet.CIDR.String())
	}
	return advertisement, nil
}

func isControlledByCUDN(nad *nettypes.NetworkAttachmentDefinition) bool {
	cudn := userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
	owner := metav1.GetControllerOfNoCopy(nad)
	return owner != nil && owner.Kind == cudn.Kind && owner.APIVersion == cudn.GroupVersion().String()
}

// referencesRouteAdvertisements returns true if the NAD is annotated with at
// least one RouteAdvertisements, or with something that cannot be parsed.
func referencesRouteAdvertisements(nad *nettypes.NetworkAttachmentDefinition) bool {
	annotation := nad.Annotations[types.OvnRouteAdvertisementsKey]
	if annotation == "" {
		return false
	}
	var raNames []string
	if err := json.Unmarshal([]byte(annotation), &raNames); err != nil {
		klog.Errorf("Failed to parse annotation %s of NAD %s/%s, considering its network advertised: %v",
			types.OvnRouteAdvertisementsKey, nad.Namespace, nad.Name, err)
		return true
	}
	return len(raNames) > 0
}
