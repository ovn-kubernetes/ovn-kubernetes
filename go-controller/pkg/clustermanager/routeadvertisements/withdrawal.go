// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// withdrawRejectedNetworks preserves existing advertisements only for
// networks still selected by the RA. Generation can reject input before returning
// selected NADs, so selection must be obtained independently for withdrawals.
func (c *Controller) withdrawRejectedNetworks(ctx context.Context, ra *ratypes.RouteAdvertisements, remaining int) (bool, error) {
	if remaining == 0 {
		// Without generated configurations, no NAD may remain advertised by
		// this RA, including networks that are still selected.
		return c.updateNADs(ctx, ra.Name, nil)
	}
	nads, err := c.getSelectedNADs(ctx, ra.Spec.NetworkSelectors)
	if err != nil {
		return false, err
	}
	return c.withdrawDeselectedNetworks(ctx, ra, nads)
}

// withdrawDeselectedNetworks removes only old advertisements. In particular,
// regenerating from the current templates here would publish parts of a rejected
// update. NAD references are removed only after every FRR update succeeds, so a
// retry (including after a restart) can still discover the outstanding removals.
func (c *Controller) withdrawDeselectedNetworks(ctx context.Context, ra *ratypes.RouteAdvertisements, selectedNADs []*nadtypes.NetworkAttachmentDefinition) (bool, error) {
	// Reconciliation may have just deleted configurations that are still in
	// the informer cache. Count the API objects before retaining NAD references.
	configs, err := c.frrClient.ApiV1beta1().FRRConfigurations(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{types.OvnRouteAdvertisementsKey: ra.Name}).String(),
	})
	if err != nil {
		return false, err
	}
	configs.Items = slices.DeleteFunc(configs.Items, func(cfg frrtypes.FRRConfiguration) bool {
		return cfg.Annotations[types.OvnRouteAdvertisementsKey] == ""
	})
	if len(configs.Items) == 0 {
		return c.syncNADReferences(ctx, ra.Name, nil, true)
	}
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
	advertiseEgressIPs := slices.Contains(ra.Spec.Advertisements, ratypes.EgressIP)
	// Existing advertisements can include EgressIPs even when the rejected
	// update no longer requests them. Use recorded network references as the
	// withdrawal context, including when selection itself is unchanged.
	if len(removed) == 0 && len(retained) == 0 && !advertiseEgressIPs {
		return c.syncNADReferences(ctx, ra.Name, retainedNADs, true)
	}
	withdrawal := newNetworkWithdrawal(removed, retained)
	// VTEP addresses belong to the underlay, rather than the tenant subnets.
	// Keep them when another selected EVPN network still uses the same VTEP.
	retainedVTEPs := sets.New[string]()
	for _, network := range retained {
		if name := network.EVPNVTEPName(); name != "" {
			retainedVTEPs.Insert(name)
		}
	}
	for _, network := range removed {
		name := network.EVPNVTEPName()
		if name == "" || retainedVTEPs.Has(name) {
			continue
		}
		vtep, err := c.vtepLister.Get(name)
		if apierrors.IsNotFound(err) {
			// The CUDN can stop referencing this VTEP before its NAD is
			// updated. Its deletion must not strand old advertisements.
			continue
		}
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
	var egressIPs map[string]map[string]sets.Set[string]
	retainedVTEPCIDRs := map[string][]*net.IPNet{}
	if advertiseEgressIPs {
		retainedNames := sets.New[string]()
		for name := range retained {
			retainedNames.Insert(name)
		}
		egressIPs, err = c.getEgressIPsByNodesByNetworks(retainedNames)
		if err != nil {
			return false, err
		}
	}
	for _, name := range sets.List(retainedVTEPs) {
		vtep, getErr := c.vtepLister.Get(name)
		if getErr != nil {
			return false, getErr
		}
		retainedVTEPCIDRs[name], err = util.ParseIPNets(vtep.Spec.CIDRs)
		if err != nil {
			return false, err
		}
	}
	changed := false
	remaining := 0
	var errs []error
	for _, cfg := range configs.Items {
		remaining++
		updated := cfg.DeepCopy()
		// Generated prefixes belong to this RA. Keep only existing routes
		// needed by retained networks, including when a deleted VTEP's
		// CIDRs and node annotation are no longer available for removal.
		key := strings.Split(cfg.Annotations[types.OvnRouteAdvertisementsKey], "/")
		if len(key) != 3 {
			return changed, fmt.Errorf("invalid generated FRRConfiguration key on %s/%s", cfg.Namespace, cfg.Name)
		}
		allowed := sets.New[string]()
		for _, addresses := range egressIPs[key[2]] {
			allowed.Insert(addresses.UnsortedList()...)
		}
		var vtepPrefixes sets.Set[string]
		if len(retainedVTEPCIDRs) > 0 {
			node, getErr := c.nodeLister.Get(key[2])
			if apierrors.IsNotFound(getErr) {
				// Unsafe input can reject generation before deleted nodes
				// are excluded. Their validated configurations are now orphaned.
				deleteErr := c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Delete(ctx, cfg.Name, metav1.DeleteOptions{})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					errs = append(errs, fmt.Errorf("failed deleting orphaned FRRConfiguration %s/%s: %w", cfg.Namespace, cfg.Name, deleteErr))
				} else {
					changed = true
					remaining--
				}
				continue
			}
			if getErr != nil {
				errs = append(errs, fmt.Errorf("failed getting node %q for VTEP withdrawal: %w", key[2], getErr))
				continue
			}
			// Validate independently: malformed data for one retained VTEP
			// must not block withdrawals or discard another VTEP's valid routes.
			vtepPrefixes = sets.New[string]()
			for name, cidrs := range retainedVTEPCIDRs {
				prefixes, validationErr := getNodeVTEPPrefixes(node, map[string][]*net.IPNet{name: cidrs})
				if validationErr == nil {
					vtepPrefixes.Insert(prefixes.UnsortedList()...)
				}
			}
		}
		for i := range updated.Spec.BGP.Routers {
			router := &updated.Spec.BGP.Routers[i]
			remove := func(prefix string) bool {
				// The default VRF also carries EVPN underlay routes. Preserve
				// only this node's retained VTEP hosts, not arbitrary EgressIPs
				// in the same VTEP CIDRs or in tenant VRFs.
				if router.VRF == "" && vtepPrefixes.Has(prefix) {
					return false
				}
				return !allowed.Has(prefix) && !prefixInNetworks(prefix, withdrawal.retainedPrefixes)
			}
			router.Prefixes = slices.DeleteFunc(router.Prefixes, remove)
			for j := range router.Neighbors {
				router.Neighbors[j].ToAdvertise.Allowed.Prefixes = slices.DeleteFunc(router.Neighbors[j].ToAdvertise.Allowed.Prefixes, remove)
			}
		}
		withdrawal.prune(&updated.Spec, vtepPrefixes)
		if reflect.DeepEqual(updated.Spec, cfg.Spec) {
			continue
		}
		_, err := c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager})
		if apierrors.IsNotFound(err) {
			// Deleted after the list; it no longer keeps NAD references alive.
			remaining--
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
	if remaining == 0 {
		// No network remains advertised when every configuration was deleted.
		retainedNADs = nil
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

func (w *networkWithdrawal) prune(spec *frrtypes.FRRConfigurationSpec, retainedVTEPs sets.Set[string]) {
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
		// A removed VTEP's CIDR can overlap another VTEP during a rejected
		// update. Protect this node's retained hosts only in the underlay VRF.
		removeAdvertisement := func(prefix string) bool {
			return !(router.VRF == "" && retainedVTEPs.Has(prefix)) && removePrefix(prefix)
		}
		router.Prefixes = slices.DeleteFunc(router.Prefixes, removeAdvertisement)
		for j := range router.Neighbors {
			neighbor := &router.Neighbors[j]
			neighbor.ToAdvertise.Allowed.Prefixes = slices.DeleteFunc(neighbor.ToAdvertise.Allowed.Prefixes, removeAdvertisement)
			neighbor.ToReceive.Allowed.Prefixes = slices.DeleteFunc(neighbor.ToReceive.Allowed.Prefixes, func(p frrtypes.PrefixSelector) bool {
				return removePrefix(p.Prefix) && !(router.VRF == "" && prefixSelectorMatchesVTEP(p, retainedVTEPs))
			})
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
	spec.BGP.Routers = slices.DeleteFunc(spec.BGP.Routers, func(router frrtypes.Router) bool {
		vrf := router.VRF
		if vrf == "" {
			vrf = types.DefaultNetworkName
		}
		return unusedVRFs.Has(vrf)
	})
	spec.Raw.Config = w.pruneRawConfig(spec.Raw.Config, unusedVRFs)
}

// Receive filters cover CIDR ranges, while retainedVTEPs contains validated
// host prefixes. Preserve only filters that actually accept a retained host.
func prefixSelectorMatchesVTEP(selector frrtypes.PrefixSelector, retainedVTEPs sets.Set[string]) bool {
	prefix, err := netip.ParsePrefix(selector.Prefix)
	if err != nil {
		return false
	}
	for value := range retainedVTEPs {
		host := netip.MustParsePrefix(value)
		bits := uint32(host.Bits())
		if !prefix.Contains(host.Addr()) || (selector.GE != 0 && bits < selector.GE) || (selector.LE != 0 && bits > selector.LE) {
			continue
		}
		if selector.GE != 0 || selector.LE != 0 || host.Bits() == prefix.Bits() {
			return true
		}
	}
	return false
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
