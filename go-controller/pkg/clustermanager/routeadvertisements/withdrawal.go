// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"

	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// withdrawDeselectedNetworks removes only old advertisements. In particular,
// regenerating from the current templates here would publish parts of a rejected
// update. NAD references are removed only after every FRR update succeeds, so a
// retry (including after a restart) can still discover the outstanding removals.
func (c *Controller) withdrawDeselectedNetworks(ctx context.Context, ra *ratypes.RouteAdvertisements, selectedNADs []*nadtypes.NetworkAttachmentDefinition) (bool, error) {
	selected := sets.New[string]()
	retainedNADs := slices.Clone(selectedNADs)
	for _, nad := range selectedNADs {
		selected.Insert(util.GetAnnotatedNetworkName(nad))
	}
	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return false, err
	}
	removed := map[string]util.NetInfo{}
	retained := map[string]util.NetInfo{}
	for _, nad := range nads {
		var references []string
		if value := nad.Annotations[types.OvnRouteAdvertisementsKey]; value != "" {
			if err := json.Unmarshal([]byte(value), &references); err != nil {
				return false, err
			}
		}
		name := util.GetAnnotatedNetworkName(nad)
		if !slices.Contains(references, ra.Name) {
			continue
		}
		network := c.nm.GetNetwork(name)
		if network == nil {
			network, err = util.ParseNADInfo(nad)
			if err != nil {
				return false, fmt.Errorf("cannot withdraw network %q: %w", name, err)
			}
		}
		if selected.Has(name) {
			// A different NAD may now select the same network. Keep its old
			// reference until acceptance permits publishing the replacement.
			retainedNADs = append(retainedNADs, nad)
			retained[name] = network
		} else {
			removed[name] = network
		}
	}
	if len(removed) == 0 {
		return c.syncNADReferences(ctx, ra.Name, retainedNADs, true)
	}
	withdrawal := newNetworkWithdrawal(removed, retained)
	// VTEP addresses belong to the underlay, rather than the tenant subnets.
	// Keep them when another selected EVPN network still uses the same VTEP.
	retainedVTEPs := sets.New[string]()
	for _, network := range retained {
		retainedVTEPs.Insert(network.EVPNVTEPName())
	}
	for _, network := range removed {
		name := network.EVPNVTEPName()
		if name == "" || retainedVTEPs.Has(name) {
			continue
		}
		vtep, err := c.vtepLister.Get(name)
		if err != nil {
			return false, err
		}
		for _, cidr := range vtep.Spec.CIDRs {
			prefix, err := netip.ParsePrefix(string(cidr))
			if err != nil {
				return false, err
			}
			withdrawal.removedPrefixes = append(withdrawal.removedPrefixes, prefix)
		}
	}
	configs, err := c.frrLister.List(labels.SelectorFromSet(labels.Set{types.OvnRouteAdvertisementsKey: ra.Name}))
	if err != nil {
		return false, err
	}
	var egressIPs map[string]map[string]sets.Set[string]
	if slices.Contains(ra.Spec.Advertisements, ratypes.EgressIP) {
		retainedNames := sets.New[string]()
		for name := range retained {
			retainedNames.Insert(name)
		}
		egressIPs, err = c.getEgressIPsByNodesByNetworks(retainedNames)
		if err != nil {
			return false, err
		}
	}
	changed := false
	var errs []error
	for _, cfg := range configs {
		if cfg.Annotations[types.OvnRouteAdvertisementsKey] == "" {
			continue
		}
		updated := cfg.DeepCopy()
		if slices.Contains(ra.Spec.Advertisements, ratypes.EgressIP) {
			// Failed EgressIP nodes were already withdrawn. On successful nodes,
			// discard old assignments outside the retained pod subnets as well.
			key := strings.Split(cfg.Annotations[types.OvnRouteAdvertisementsKey], "/")
			if len(key) != 3 {
				return changed, fmt.Errorf("invalid generated FRRConfiguration key on %s/%s", cfg.Namespace, cfg.Name)
			}
			allowed := sets.New[string]()
			for _, addresses := range egressIPs[key[2]] {
				allowed.Insert(addresses.UnsortedList()...)
			}
			for i := range updated.Spec.BGP.Routers {
				router := &updated.Spec.BGP.Routers[i]
				remove := func(prefix string) bool {
					return !allowed.Has(prefix) && !prefixInNetworks(prefix, withdrawal.retainedPrefixes)
				}
				router.Prefixes = slices.DeleteFunc(router.Prefixes, remove)
				for j := range router.Neighbors {
					router.Neighbors[j].ToAdvertise.Allowed.Prefixes = slices.DeleteFunc(router.Neighbors[j].ToAdvertise.Allowed.Prefixes, remove)
				}
			}
		}
		withdrawal.prune(&updated.Spec)
		if reflect.DeepEqual(updated.Spec, cfg.Spec) {
			continue
		}
		_, err := c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager})
		if apierrors.IsNotFound(err) {
			// Already withdrawn by updateFRRConfigurations.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("failed withdrawing deselected networks from FRRConfiguration %s/%s: %w", cfg.Namespace, cfg.Name, err))
			continue
		}
		changed = true
	}
	if len(errs) > 0 {
		return changed, errors.Join(errs...)
	}
	nadChanged, err := c.syncNADReferences(ctx, ra.Name, retainedNADs, true)
	return changed || nadChanged, err
}

type networkWithdrawal struct {
	removedPrefixes       []netip.Prefix
	retainedPrefixes      []netip.Prefix
	retainedPrefixesByVRF map[string][]netip.Prefix
	removedVRFs           sets.Set[string]
	removedVNIs           sets.Set[string]
	retainEVPN            bool
	removedEVPN           bool
}

func newNetworkWithdrawal(removed, retained map[string]util.NetInfo) *networkWithdrawal {
	w := &networkWithdrawal{removedVRFs: sets.New[string](), removedVNIs: sets.New[string](), retainedPrefixesByVRF: map[string][]netip.Prefix{}}
	for _, network := range removed {
		w.removedVRFs.Insert(util.GetNetworkVRFName(network))
		for _, subnet := range network.Subnets() {
			w.removedPrefixes = append(w.removedPrefixes, netip.MustParsePrefix(subnet.CIDR.String()))
		}
		for _, vni := range []int32{network.EVPNMACVRFVNI(), network.EVPNIPVRFVNI()} {
			if vni != 0 {
				w.removedVNIs.Insert(fmt.Sprint(vni))
				w.removedEVPN = true
			}
		}
	}
	for _, network := range retained {
		w.removedVRFs.Delete(util.GetNetworkVRFName(network))
		for _, subnet := range network.Subnets() {
			prefix := netip.MustParsePrefix(subnet.CIDR.String())
			w.retainedPrefixes = append(w.retainedPrefixes, prefix)
			vrf := util.GetNetworkVRFName(network)
			w.retainedPrefixesByVRF[vrf] = append(w.retainedPrefixesByVRF[vrf], prefix)
		}
		for _, vni := range []int32{network.EVPNMACVRFVNI(), network.EVPNIPVRFVNI()} {
			if vni != 0 {
				w.removedVNIs.Delete(fmt.Sprint(vni))
				w.retainEVPN = true
			}
		}
	}
	return w
}

func prefixInNetworks(value string, networks []netip.Prefix) bool {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(networks, func(network netip.Prefix) bool {
		return network.Bits() <= prefix.Bits() && network.Contains(prefix.Addr())
	})
}

func (w *networkWithdrawal) removePrefix(prefix string) bool {
	return prefixInNetworks(prefix, w.removedPrefixes) && !prefixInNetworks(prefix, w.retainedPrefixes)
}

func (w *networkWithdrawal) prune(spec *frrtypes.FRRConfigurationSpec) {
	for i := range spec.BGP.Routers {
		router := &spec.BGP.Routers[i]
		removePrefix := w.removePrefix
		vrf := router.VRF
		if vrf == "" {
			vrf = types.DefaultNetworkName
		}
		if w.removedVRFs.Has(vrf) {
			// Overlapping CIDRs in another VRF do not keep this tenant's routes
			// alive. Only retained networks imported into this router share its
			// target VRF and require its overlapping prefixes.
			var importedPrefixes []netip.Prefix
			for _, imported := range router.Imports {
				importedPrefixes = append(importedPrefixes, w.retainedPrefixesByVRF[imported.VRF]...)
			}
			removePrefix = func(prefix string) bool {
				return prefixInNetworks(prefix, w.removedPrefixes) && !prefixInNetworks(prefix, importedPrefixes)
			}
		}
		router.Prefixes = slices.DeleteFunc(router.Prefixes, removePrefix)
		for j := range router.Neighbors {
			neighbor := &router.Neighbors[j]
			neighbor.ToAdvertise.Allowed.Prefixes = slices.DeleteFunc(neighbor.ToAdvertise.Allowed.Prefixes, removePrefix)
			neighbor.ToReceive.Allowed.Prefixes = slices.DeleteFunc(neighbor.ToReceive.Allowed.Prefixes, func(p frrtypes.PrefixSelector) bool { return removePrefix(p.Prefix) })
		}
	}
	// A deselected network's VRF can also be the shared target for a retained
	// network. Keep that router, its incoming imports, and its unicast raw
	// configuration while removing the deselected network's EVPN bindings.
	unusedVRFs := w.removedVRFs.Clone()
	for _, router := range spec.BGP.Routers {
		if len(router.Prefixes) > 0 {
			vrf := router.VRF
			if vrf == "" {
				vrf = types.DefaultNetworkName
			}
			unusedVRFs.Delete(vrf)
		}
	}
	for i := range spec.BGP.Routers {
		router := &spec.BGP.Routers[i]
		router.Imports = slices.DeleteFunc(router.Imports, func(vrf frrtypes.Import) bool { return unusedVRFs.Has(vrf.VRF) })
	}
	spec.BGP.Routers = slices.DeleteFunc(spec.BGP.Routers, func(router frrtypes.Router) bool { return unusedVRFs.Has(router.VRF) })
	spec.Raw.Config = w.pruneRawConfig(spec.Raw.Config, unusedVRFs)
}

// Only remove complete sections emitted by generateRawConfig. Retained sections
// remain byte-for-byte unchanged: rebuilding raw config could activate new peers
// or change route targets while the RA is rejected.
func (w *networkWithdrawal) pruneRawConfig(raw string, unusedVRFs sets.Set[string]) string {
	var kept strings.Builder
	skipUntil := ""
	routerVRF := ""
	for _, line := range strings.SplitAfter(raw, "\n") {
		command := strings.TrimSuffix(line, "\n")
		if skipUntil != "" {
			if command == skipUntil {
				skipUntil = ""
			}
			continue
		}
		fields := strings.Fields(command)
		if len(fields) >= 3 && fields[0] == "router" && fields[1] == "bgp" {
			routerVRF = ""
			if len(fields) == 5 && fields[3] == "vrf" {
				routerVRF = fields[4]
			}
		}
		switch {
		case len(fields) == 2 && fields[0] == "vrf" && w.removedVRFs.Has(fields[1]):
			skipUntil = "exit-vrf"
		case len(fields) == 5 && fields[0] == "router" && fields[1] == "bgp" && fields[3] == "vrf" && unusedVRFs.Has(fields[4]):
			skipUntil = "exit"
		case strings.HasPrefix(command, "  vni ") && len(fields) == 2 && w.removedVNIs.Has(fields[1]):
			skipUntil = "  exit-vni"
		case command == " address-family l2vpn evpn" && (w.removedVRFs.Has(routerVRF) || (w.removedEVPN && !w.retainEVPN)):
			skipUntil = " exit-address-family"
		default:
			kept.WriteString(line)
		}
	}
	return kept.String()
}
