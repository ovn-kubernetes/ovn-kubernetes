// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	networkqosv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// networkQoSState is the cache that keeps the state of a single
// network qos in the cluster with namespace+name being unique
type networkQoSState struct {
	sync.RWMutex
	// name of the network qos
	name      string
	namespace string

	SrcAddrSet addressset.AddressSet
	// SrcPortGroupName is the OVN name of the per-NetworkQoS source port group,
	// used on ipamless localnet networks to match source pods via inport == @pg
	// instead of the src-IP address set. Empty on IPAM-enabled networks.
	SrcPortGroupName string
	Pods             sync.Map // pods name -> ips in the srcAddrSet (IPAM) / LSP UUIDs in the source port group (ipamless)
	SwitchRefs       sync.Map // switch name -> list of source pods
	PodSelector      labels.Selector

	// egressRules stores the objects needed to track .Spec.Egress changes
	EgressRules []*GressRule
}

func (nqosState *networkQoSState) getObjectNameKey() string {
	return joinMetaNamespaceAndName(nqosState.namespace, nqosState.name, ":")
}

func (nqosState *networkQoSState) getDbObjectIDs(controller string, ruleIndex int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: nqosState.getObjectNameKey(),
		libovsdbops.RuleIndex:     fmt.Sprintf("%d", ruleIndex),
	})
}

// initSourcePortGroupName computes and stores the deterministic OVN name of this
// NetworkQoS's source port group. Used on ipamless localnet networks, where
// source pods are matched by port-group membership rather than src IP.
func (nqosState *networkQoSState) initSourcePortGroupName(controllerName string) {
	nqosState.SrcPortGroupName = libovsdbutil.GetPortGroupName(
		GetNetworkQoSPortGroupDbIDs(nqosState.namespace, nqosState.name, controllerName))
}

func (nqosState *networkQoSState) initAddressSets(addressSetFactory addressset.AddressSetFactory, controllerName string) error {
	var err error
	// init source address set
	nqosState.SrcAddrSet, err = addressSetFactory.EnsureAddressSet(GetNetworkQoSAddrSetDbIDs(nqosState.namespace, nqosState.name, "src", "0", controllerName))
	if err != nil {
		return fmt.Errorf("failed to init source address set for %s/%s: %w", nqosState.namespace, nqosState.name, err)
	}
	// ensure destination address sets
	for ruleIndex, rule := range nqosState.EgressRules {
		for destIndex, dest := range rule.Classifier.Destinations {
			if dest.NamespaceSelector == nil && dest.PodSelector == nil {
				continue
			}
			dest.DestAddrSet, err = addressSetFactory.EnsureAddressSet(GetNetworkQoSAddrSetDbIDs(nqosState.namespace, nqosState.name, strconv.Itoa(ruleIndex), strconv.Itoa(destIndex), controllerName))
			if err != nil {
				return fmt.Errorf("failed to init destination address set for %s/%s: %w", nqosState.namespace, nqosState.name, err)
			}
		}
	}
	return nil
}

func (nqosState *networkQoSState) matchSourceSelector(pod *corev1.Pod) bool {
	if pod.Namespace != nqosState.namespace {
		return false
	}
	if nqosState.PodSelector == nil {
		return true
	}
	return nqosState.PodSelector.Matches(labels.Set(pod.Labels))
}

func (nqosState *networkQoSState) configureSourcePod(ctrl *Controller, pod *corev1.Pod, addresses []string) error {
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	// the pod's IP can change while it still matches the selector (a DHCP
	// lease may be replaced across a sandbox recreation), delete what this pod
	// previously contributed before adding the current addresses, or the
	// stale IP stays in the address set until the pod is deleted
	if previousAddresses, ok := nqosState.Pods.Load(fullPodName); ok {
		if stale := staleAddresses(previousAddresses.([]string), addresses); len(stale) > 0 {
			if err := nqosState.SrcAddrSet.DeleteAddresses(stale); err != nil {
				return fmt.Errorf("failed to delete stale addresses {%s} from address set %s: %v",
					strings.Join(stale, ","), nqosState.SrcAddrSet.GetName(), err)
			}
		}
	}
	if err := nqosState.SrcAddrSet.AddAddresses(addresses); err != nil {
		return fmt.Errorf("failed to add addresses {%s} to address set %s for NetworkQoS %s/%s: %v", strings.Join(addresses, ","), nqosState.SrcAddrSet.GetName(), nqosState.namespace, nqosState.name, err)
	}
	nqosState.Pods.Store(fullPodName, addresses)
	klog.V(4).Infof("Successfully added address (%s) of pod %s to address set %s", strings.Join(addresses, ","), fullPodName, nqosState.SrcAddrSet.GetName())
	return nqosState.bindQoSToPodSwitch(ctrl, pod, fullPodName)
}

// configureSourcePodIPAMless is the ipamless-localnet counterpart of
// configureSourcePod. Source pods on ipamless localnet networks have no
// OVN-managed IP, so they are matched by logical switch port membership in the
// per-NetworkQoS source port group (inport == @pg) rather than by a src-IP
// address set. It resolves each of the pod's logical switch ports (there is
// usually one per network attachment) to its UUID, adds the resolved ports to
// the source port group, and binds the QoS rules to the pod's localnet switch.
//
// When a port's LSP is not yet present in the NB DB (the pod is annotated but
// its LSP has not been created yet), that port is skipped and will be
// reconciled on a later pod update event, mirroring the AdminNetworkPolicy
// subject-recompute behaviour. If none of the ports are resolvable yet the call
// is a no-op (no port group membership, no switch binding) and returns nil so
// the reconcile is retried later without surfacing an error.
func (nqosState *networkQoSState) configureSourcePodIPAMless(ctrl *Controller, pod *corev1.Pod, lspNames []string) error {
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	lspUUIDs := make([]string, 0, len(lspNames))
	for _, lspName := range lspNames {
		lsp, err := libovsdbops.GetLogicalSwitchPort(ctrl.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				// The pod is annotated but its LSP has not been created yet.
				// Skip and wait: the later pod update event (once the LSP lands)
				// will reconcile it.
				klog.V(5).Infof("NetworkQoS %s/%s: logical switch port %s for source pod %s not found yet, will reconcile on next pod event", nqosState.namespace, nqosState.name, lspName, fullPodName)
				continue
			}
			return fmt.Errorf("failed to look up logical switch port %s for NetworkQoS %s/%s: %w", lspName, nqosState.namespace, nqosState.name, err)
		}
		lspUUIDs = append(lspUUIDs, lsp.UUID)
	}
	if len(lspUUIDs) == 0 {
		// Nothing resolvable yet; skip without error so it is retried on the
		// next pod event.
		return nil
	}
	ops, err := libovsdbops.AddPortsToPortGroupOps(ctrl.nbClient, nil, nqosState.SrcPortGroupName, lspUUIDs...)
	if err != nil {
		return fmt.Errorf("failed to build ops to add source pod %s ports to port group %s (NetworkQoS %s/%s): %w", fullPodName, nqosState.SrcPortGroupName, nqosState.namespace, nqosState.name, err)
	}
	if _, err := libovsdbops.TransactAndCheck(ctrl.nbClient, ops); err != nil {
		return fmt.Errorf("failed to add source pod %s ports to port group %s (NetworkQoS %s/%s): %w", fullPodName, nqosState.SrcPortGroupName, nqosState.namespace, nqosState.name, err)
	}
	nqosState.Pods.Store(fullPodName, lspUUIDs)
	klog.V(4).Infof("Successfully added logical switch port(s) %v of pod %s to source port group %s", lspUUIDs, fullPodName, nqosState.SrcPortGroupName)
	return nqosState.bindQoSToPodSwitch(ctrl, pod, fullPodName)
}

// bindQoSToPodSwitch resolves the logical switch for the pod and, if the QoS
// rules are not already bound to it, binds them, tracking the pod under the
// switch's SwitchRefs entry. Shared by the IPAM (address-set) and ipamless
// (port-group) source paths.
func (nqosState *networkQoSState) bindQoSToPodSwitch(ctrl *Controller, pod *corev1.Pod, fullPodName string) error {
	// get switch name
	switchName := ctrl.getLogicalSwitchName(pod.Spec.NodeName)
	if switchName == "" {
		return fmt.Errorf("failed to get logical switch name for node %s, topology %s", pod.Spec.NodeName, ctrl.TopologyType())
	}

	podList := []string{}
	val, loaded := nqosState.SwitchRefs.Load(switchName)
	if loaded {
		podList = val.([]string)
	}

	if !loaded {
		klog.V(4).Infof("Adding NetworkQoS %s/%s to logical switch %s", nqosState.namespace, nqosState.name, switchName)
		start := time.Now()
		if err := ctrl.addQoSToLogicalSwitch(nqosState, switchName); err != nil {
			return err
		}
		recordOvnOperationDuration("add", time.Since(start).Milliseconds())
	}

	podList = append(podList, fullPodName)
	nqosState.SwitchRefs.Store(switchName, podList)
	return nil
}

func (nqosState *networkQoSState) removePodFromSource(ctrl *Controller, fullPodName string, addresses []string) error {
	if len(addresses) == 0 {
		// if no addresses is provided, try lookup in cache
		if val, ok := nqosState.Pods.Load(fullPodName); ok {
			addresses = val.([]string)
		}
	}
	if len(addresses) > 0 {
		if err := nqosState.SrcAddrSet.DeleteAddresses(addresses); err != nil {
			return fmt.Errorf("failed to delete addresses (%s) from address set %s: %v", strings.Join(addresses, ","), nqosState.SrcAddrSet.GetName(), err)
		}
	}
	nqosState.Pods.Delete(fullPodName)
	return nqosState.removeZeroQoSNodes(ctrl, fullPodName)
}

// removePodLSPFromSource is the ipamless-localnet counterpart of
// removePodFromSource: it removes the pod's logical switch port(s) from the
// per-NetworkQoS source port group (rather than deleting src IPs from an address
// set) and then unbinds the QoS from any switch that no longer has source pods.
func (nqosState *networkQoSState) removePodLSPFromSource(ctrl *Controller, fullPodName string) error {
	if val, ok := nqosState.Pods.Load(fullPodName); ok {
		if lspUUIDs := val.([]string); len(lspUUIDs) > 0 {
			ops, err := libovsdbops.DeletePortsFromPortGroupOps(ctrl.nbClient, nil, nqosState.SrcPortGroupName, lspUUIDs...)
			if err != nil {
				return fmt.Errorf("failed to build ops to remove pod %s ports from port group %s: %w", fullPodName, nqosState.SrcPortGroupName, err)
			}
			if _, err := libovsdbops.TransactAndCheck(ctrl.nbClient, ops); err != nil {
				return fmt.Errorf("failed to remove pod %s ports from port group %s: %w", fullPodName, nqosState.SrcPortGroupName, err)
			}
		}
	}
	nqosState.Pods.Delete(fullPodName)
	return nqosState.removeZeroQoSNodes(ctrl, fullPodName)
}

func (nqosState *networkQoSState) removeZeroQoSNodes(ctrl *Controller, fullPodName string) error {
	zeroQoSSwitches := []string{}
	// since node is unknown when pod is delete, iterate the SwitchRefs to remove the pod
	nqosState.SwitchRefs.Range(func(key, val any) bool {
		switchName := key.(string)
		podList := val.([]string)
		podList = slices.DeleteFunc(podList, func(s string) bool {
			return s == fullPodName
		})
		if len(podList) == 0 {
			zeroQoSSwitches = append(zeroQoSSwitches, switchName)
		} else {
			nqosState.SwitchRefs.Store(switchName, podList)
		}
		return true
	})
	// unbind qos from L3 logical switches where doesn't have source pods any more
	if len(zeroQoSSwitches) > 0 && ctrl.TopologyType() == types.Layer3Topology {
		start := time.Now()
		if err := ctrl.removeQoSFromLogicalSwitches(nqosState, zeroQoSSwitches); err != nil {
			return err
		}
		recordOvnOperationDuration("remove", time.Since(start).Milliseconds())
		for _, lsw := range zeroQoSSwitches {
			nqosState.SwitchRefs.Delete(lsw)
		}
	}
	return nil
}

func (nqosState *networkQoSState) getAddressSetHashNames() []string {
	addrsetNames := []string{}
	if nqosState.SrcAddrSet != nil {
		v4Hash, v6Hash := nqosState.SrcAddrSet.GetASHashNames()
		addrsetNames = append(addrsetNames, v4Hash, v6Hash)
	}
	for _, rule := range nqosState.EgressRules {
		for _, dest := range rule.Classifier.Destinations {
			if dest.DestAddrSet != nil {
				v4Hash, v6Hash := dest.DestAddrSet.GetASHashNames()
				addrsetNames = append(addrsetNames, v4Hash, v6Hash)
			}
		}
	}
	return addrsetNames
}

func (nqosState *networkQoSState) cleanupStaleAddresses(addressSetMap map[string]sets.Set[string]) error {
	if nqosState.SrcAddrSet != nil {
		addresses := addressSetMap[nqosState.SrcAddrSet.GetName()]
		v4Addresses, _ := nqosState.SrcAddrSet.GetAddresses()
		staleAddresses := []string{}
		for _, address := range v4Addresses {
			if !addresses.Has(address) {
				staleAddresses = append(staleAddresses, address)
			}
		}
		if len(staleAddresses) > 0 {
			if err := nqosState.SrcAddrSet.DeleteAddresses(staleAddresses); err != nil {
				return err
			}
		}
	}
	for _, egress := range nqosState.EgressRules {
		for _, dest := range egress.Classifier.Destinations {
			if dest.DestAddrSet == nil {
				continue
			}
			addresses := addressSetMap[dest.DestAddrSet.GetName()]
			v4Addresses, _ := dest.DestAddrSet.GetAddresses()
			staleAddresses := []string{}
			for _, address := range v4Addresses {
				if !addresses.Has(address) {
					staleAddresses = append(staleAddresses, address)
				}
			}
			if len(staleAddresses) > 0 {
				if err := dest.DestAddrSet.DeleteAddresses(staleAddresses); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type GressRule struct {
	Priority   int
	Dscp       int
	Classifier *Classifier

	// bandwitdh
	Rate  *int
	Burst *int
}

type trafficDirection string

const (
	trafficDirSource trafficDirection = "src"
	trafficDirDest   trafficDirection = "dst"
)

type Classifier struct {
	Destinations []*Destination
	Ports        []*networkqosv1alpha1.Port
}

// ToQosMatchString generates dest and protocol/port part of QoS match string, based on
// Classifier's destinations, protocol and port fields, example:
// (ip4.dst == $addr_set_name || (ip4.dst == 128.116.0.0/17 && ip4.dst != {128.116.0.0,128.116.0.255})) && tcp && tcp.dst == 8080
// Multiple destinations will be connected by "||".
// See https://github.com/ovn-org/ovn/blob/2bdf1129c19d5bd2cd58a3ddcb6e2e7254b05054/ovn-nb.xml#L2942-L3025 for details
func (c *Classifier) ToQosMatchString(ipv4Enabled, ipv6Enabled bool) string {
	if c == nil {
		return ""
	}
	destMatchStrings := []string{}
	for _, dest := range c.Destinations {
		match := "ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0"
		if dest.DestAddrSet != nil {
			match = addressSetToMatchString(dest.DestAddrSet, trafficDirDest, ipv4Enabled, ipv6Enabled)
		} else if dest.IpBlock != nil && dest.IpBlock.CIDR != "" {
			ipVersion := "ip4"
			if utilnet.IsIPv6CIDRString(dest.IpBlock.CIDR) {
				ipVersion = "ip6"
			}
			if len(dest.IpBlock.Except) == 0 {
				match = fmt.Sprintf("%s.%s == %s", ipVersion, trafficDirDest, dest.IpBlock.CIDR)
			} else {
				match = fmt.Sprintf("%s.%s == %s && %s.%s != {%s}", ipVersion, trafficDirDest, dest.IpBlock.CIDR, ipVersion, trafficDirDest, strings.Join(dest.IpBlock.Except, ","))
			}
		}
		destMatchStrings = append(destMatchStrings, match)
	}

	output := ""
	if len(destMatchStrings) == 1 {
		output = destMatchStrings[0]
	} else {
		for index, str := range destMatchStrings {
			if index > 0 {
				output += " || "
			}
			if strings.Contains(str, "||") || strings.Contains(str, "&&") {
				output = output + fmt.Sprintf("(%s)", str)
			} else {
				output = output + str
			}
		}
	}
	if strings.Contains(output, "||") {
		output = fmt.Sprintf("(%s)", output)
	}
	protoPortMap := map[string][]string{}
	for _, port := range c.Ports {
		if port.Protocol == "" {
			continue
		}
		protocol := strings.ToLower(port.Protocol)
		ports := protoPortMap[protocol]
		if ports == nil {
			ports = []string{}
		}
		if port.Port != nil {
			ports = append(ports, fmt.Sprintf("%d", *port.Port))
		}
		protoPortMap[protocol] = ports
	}

	sortedProtocols := make([]string, 0, len(protoPortMap))
	for protocol := range protoPortMap {
		sortedProtocols = append(sortedProtocols, protocol)
	}
	sort.Strings(sortedProtocols)

	portMatches := []string{}
	for _, protocol := range sortedProtocols {
		ports := protoPortMap[protocol]
		match := protocol
		if len(ports) == 1 {
			match = fmt.Sprintf("%s && %s.dst == %s", protocol, protocol, ports[0])
		} else if len(ports) > 1 {
			match = fmt.Sprintf("%s && %s.dst == {%s}", protocol, protocol, strings.Join(ports, ","))
		}
		portMatches = append(portMatches, match)
	}
	if len(portMatches) == 1 {
		output = fmt.Sprintf("%s && %s", output, portMatches[0])
	} else if len(portMatches) > 1 {
		output = fmt.Sprintf("%s && ((%s))", output, strings.Join(portMatches, ") || ("))
	}
	return output
}

type Destination struct {
	IpBlock *knet.IPBlock

	DestAddrSet       addressset.AddressSet
	PodSelector       labels.Selector
	Pods              sync.Map // pods name -> ips in the destAddrSet
	NamespaceSelector labels.Selector
}

func (dest *Destination) matchPod(podNs *corev1.Namespace, pod *corev1.Pod, qosNamespace string) bool {
	switch {
	case dest.NamespaceSelector != nil && dest.PodSelector != nil:
		return dest.NamespaceSelector.Matches(labels.Set(podNs.Labels)) && dest.PodSelector.Matches(labels.Set(pod.Labels))
	case dest.NamespaceSelector == nil && dest.PodSelector != nil:
		return pod.Namespace == qosNamespace && dest.PodSelector.Matches(labels.Set(pod.Labels))
	case dest.NamespaceSelector != nil && dest.PodSelector == nil:
		return dest.NamespaceSelector.Matches(labels.Set(podNs.Labels))
	default: //dest.NamespaceSelector == nil && dest.PodSelector == nil:
		return false
	}
}

func (dest *Destination) addPod(podNamespace, podName string, addresses []string) error {
	fullPodName := joinMetaNamespaceAndName(podNamespace, podName)
	// drop addresses the pod no longer holds.
	if val, ok := dest.Pods.Load(fullPodName); ok {
		if stale := staleAddresses(val.([]string), addresses); len(stale) > 0 {
			if err := dest.DestAddrSet.DeleteAddresses(stale); err != nil {
				return fmt.Errorf("failed to delete stale addresses (%s): %v", strings.Join(stale, ","), err)
			}
		}
	}
	if err := dest.DestAddrSet.AddAddresses(addresses); err != nil {
		return err
	}
	// add pod to map
	dest.Pods.Store(fullPodName, addresses)
	return nil
}

// staleAddresses returns the entries of old that are absent from current.
func staleAddresses(old, current []string) []string {
	currentSet := make(map[string]struct{}, len(current))
	for _, a := range current {
		currentSet[a] = struct{}{}
	}
	var stale []string
	for _, a := range old {
		if _, ok := currentSet[a]; !ok {
			stale = append(stale, a)
		}
	}
	return stale
}

func (dest *Destination) removePod(fullPodName string, addresses []string) error {
	if len(addresses) == 0 {
		val, ok := dest.Pods.Load(fullPodName)
		if ok && val != nil {
			addresses = val.([]string)
		}
	}
	if err := dest.DestAddrSet.DeleteAddresses(addresses); err != nil {
		return fmt.Errorf("failed to remove addresses (%s): %v", strings.Join(addresses, ","), err)
	}
	dest.Pods.Delete(fullPodName)
	return nil
}

func getQoSRulePriority(qosPriority, ruleIndex int) int {
	return 10000 + qosPriority*10 + ruleIndex
}
