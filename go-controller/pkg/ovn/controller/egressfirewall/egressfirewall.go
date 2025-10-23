package egressfirewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers/externalversions/egressfirewall/v1"
	v2 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
	dnsnameresolver "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	aclDeleteBatchSize = 1000
	// transaction time to delete 80K ACLs from 2 port groups is ~4.5 sec.
	// transaction time to delete 80K acls from one port group and add them to another
	// is ~3 sec
	// Therefore this limit is safe enough to not exceed 10 sec transaction timeout.
	aclChangePGBatchSize           = 80000
	matchKindV4CIDR      matchKind = iota
	matchKindV6CIDR
	matchKindV4AddressSet
	matchKindV6AddressSet
	defaultControllerOwner = "default-network-controller"
)

type egressFirewall struct {
	name        string
	namespace   string
	egressRules []*egressFirewallRule
}

type egressFirewallRule struct {
	id     int
	access egressfirewallapi.EgressFirewallRuleType
	ports  []egressfirewallapi.EgressFirewallPort
	to     destination
}

type destination struct {
	cidrSelector string
	dnsName      string
	// clusterSubnetIntersection is true, if egress firewall rule CIDRSelector intersects with clusterSubnet.
	// Based on this flag we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection bool
	// nodeName: nodeIPs
	nodeAddrs    map[string][]string
	nodeSelector *metav1.LabelSelector
}

type matchTarget struct {
	kind  matchKind
	value string
	// clusterSubnetIntersection is inherited from the egressFirewallRule destination.
	// clusterSubnetIntersection is true, if egress firewall rule CIDRSelector intersects with clusterSubnet.
	// Based on this flag we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection bool
}

type matchKind int

type EFController struct {
	sync.RWMutex

	name        string
	zone        string
	ruleCounter sync.Map

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	kube     *kube.KubeOVN

	nodeLister      corelisters.NodeLister
	namespaceLister corelisters.NamespaceLister
	efLister        v2.EgressFirewallLister

	// egressFirewalls is a map of namespaces and the egressFirewall attached to it
	egressFirewalls sync.Map

	controller     controller.Controller
	nodeController controller.Controller
	networkManager networkmanager.Interface
	// dnsNameResolver is used for resolving the IP addresses of DNS names
	// used in egress firewall rules
	dnsNameResolver dnsnameresolver.DNSNameResolver
	observManager   *observability.Manager
}

func NewEFController(
	name string,
	zone string,
	kube *kube.KubeOVN,
	nbClient libovsdbclient.Client,
	namespaceLister corelisters.NamespaceLister,
	nodeInformer coreinformers.NodeInformer,
	efInformer v1.EgressFirewallInformer,
	networkManager networkmanager.Interface,
	dnsNameResolver dnsnameresolver.DNSNameResolver,
	observManager *observability.Manager,
) (*EFController, error) {
	c := &EFController{
		name:            name,
		zone:            zone,
		nbClient:        nbClient,
		kube:            kube,
		nodeLister:      nodeInformer.Lister(),
		namespaceLister: namespaceLister,
		efLister:        efInformer.Lister(),
		egressFirewalls: sync.Map{},
		networkManager:  networkManager,
		dnsNameResolver: dnsNameResolver,
		observManager:   observManager,
		ruleCounter:     sync.Map{},
	}

	controllerConfig := &controller.ControllerConfig[egressfirewallapi.EgressFirewall]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    efInformer.Informer(),
		Lister:      efInformer.Lister().List,
		MaxAttempts: controller.InfiniteAttempts,
		Reconcile:   c.sync,
		// Node changes can require reconcile, need to always reconcile. Should not be many updates to EF.
		ObjNeedsUpdate: func(_, _ *egressfirewallapi.EgressFirewall) bool {
			return true
		},
		Threadiness: 1,
	}

	c.controller = controller.NewController(
		c.name,
		controllerConfig,
	)

	nodeControllerConfig := &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: efNodeNeedsUpdate,
		Reconcile:      c.updateEgressFirewallForNode,
		Threadiness:    1,
	}

	c.nodeController = controller.NewController(
		c.name+"-node",
		nodeControllerConfig,
	)

	return c, nil
}

// Reconcile queues the key to the egress firewall controller
func (oc *EFController) Reconcile(key string) {
	oc.controller.Reconcile(key)
}

// syncEgressFirewall deletes stale db entries for previous versions of Egress Firewall implementation and removes
// stale db entries for Egress Firewalls that don't exist anymore.
// Egress firewall implementation had many versions, the latest one makes no difference for gateway modes, and creates
// ACLs on namespaced port groups.
func (oc *EFController) syncEgressFirewall() error {
	egressFirewalls, err := oc.efLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list egress firewalls: %w", oc.name, err)
	}

	err = oc.deleteStaleACLs()
	if err != nil {
		return err
	}

	existingEFNamespaces := map[string]bool{}
	for _, ef := range egressFirewalls {
		existingEFNamespaces[ef.Namespace] = true
	}

	// find all existing egress firewall ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, defaultControllerOwner, nil)
	aclP := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	efACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclP)
	if err != nil {
		return fmt.Errorf("cannot find Egress Firewall ACLs: %v", err)
	}

	// another sync to move ACLs to the right port group
	err = oc.moveACLsToNamespacedPortGroups(existingEFNamespaces, efACLs)
	if err != nil {
		return err
	}

	var deletedNSACLs = map[string][]*nbdb.ACL{}
	for _, acl := range efACLs {
		namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if !existingEFNamespaces[namespace] {
			deletedNSACLs[namespace] = append(deletedNSACLs[namespace], acl)
		}
	}

	err = batching.BatchMap[*nbdb.ACL](aclChangePGBatchSize, deletedNSACLs, func(batchNsACLs map[string][]*nbdb.ACL) error {
		var ops []ovsdb.Operation
		var err error
		for _, acls := range batchNsACLs {
			// find the port group that has the stale acl
			for _, acl := range acls {
				p := func(item *nbdb.PortGroup) bool {
					if len(item.ACLs) == 0 {
						return false
					}
					for _, aclUUID := range item.ACLs {
						if acl.UUID == aclUUID {
							return true
						}
					}
					return false
				}
				foundPGs, err := libovsdbops.FindPortGroupsWithPredicate(oc.nbClient, p)
				if err != nil {
					return fmt.Errorf("failed to search for port groups during egress firewall ACL sync: %w", err)
				}
				if len(foundPGs) > 1 {
					pgNames := make([]string, len(foundPGs))
					for _, pg := range foundPGs {
						pgNames = append(pgNames, pg.Name)
					}
					klog.Warningf("Found multiple port groups associated with the same ACL %q: %s",
						*acl.Name, strings.Join(pgNames, ","))
				}
				for _, pg := range foundPGs {
					// delete stale ACLs from namespaced port group
					// both port group and acls may not exist after moveACLsToNamespacedPortGroups,
					// but DeleteACLsFromPortGroupOps doesn't return error in these cases
					ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, pg.Name, acls...)
					if err != nil {
						return fmt.Errorf("failed to build cleanup ops: %w", err)
					}
				}
			}
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to clean up egress firewall ACLs: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Delete stale address sets related to EgressFirewallDNS which are not referenced by any ACL.
	return oc.dnsNameResolver.DeleteStaleAddrSets(oc.nbClient)
}

func (oc *EFController) Start() error {
	klog.Infof("Starting EgressFirewall controller")
	return controller.StartWithInitialSync(oc.syncEgressFirewall, oc.controller, oc.nodeController)
}

func (oc *EFController) Stop() {
	klog.Infof("%s: shutting down", oc.name)
	controller.Stop(oc.nodeController, oc.controller)
}

func (oc *EFController) sync(key string) error {
	oc.Lock()
	defer oc.Unlock()
	klog.V(5).Info("Syncing EgressFirewall", key)
	namespace, efName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key for egress firewall: %s", key)
	}
	egressFirewall, err := oc.efLister.EgressFirewalls(namespace).Get(efName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		// delete case
		klog.V(3).Infof("Removing egress firewall %s", key)
		p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDsNoRule(namespace), nil)
		invalidACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
		if err != nil {
			return fmt.Errorf("error finding ACLs for egress firewall %s: %v", key, err)
		}
		if err := libovsdbops.DeleteACLsFromAllPortGroups(oc.nbClient, invalidACLs...); err != nil {
			return fmt.Errorf("error deleting stale ACLs for egress firewall %s: %v", key, err)
		}
		if err := oc.dnsNameResolver.Delete(namespace); err != nil {
			return err
		}
		numRules, ok := oc.ruleCounter.Load(key)
		if ok {
			metrics.UpdateEgressFirewallRuleCount(float64(-numRules.(uint32)))
			oc.ruleCounter.Delete(key)
		} else {
			klog.Errorf("Unable to decrement egress firewall rule count, cache miss for key: %s", key)
		}
		metrics.DecrementEgressFirewallCount()
		return nil
	}

	// add/update case

	// find old ACLs, we are going to track them, then remove them after we add
	// Egress Firewall dynamically allocates priority depending on the rule position in the spec, which
	// complicates tracking stale vs valid rules
	// Therefore we wholesale add all rules then delete old rules, ensuring no gaps where there is
	// no firewall applied
	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDsNoRule(namespace), nil)
	invalidACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("error finding ACLs for egress firewall %s: %v", egressFirewall.Name, err)
	}

	err = oc.addEgressFirewall(egressFirewall, &invalidACLs)
	if err == nil {
		// remove invalidACLs
		if removalErr := libovsdbops.DeleteACLsFromAllPortGroups(oc.nbClient, invalidACLs...); removalErr != nil {
			err = fmt.Errorf("error deleting stale ACLs for egress firewall %s: %v", egressFirewall.Name, err)
		}
	}
	if statusErr := oc.setEgressFirewallStatus(egressFirewall, len(invalidACLs), err); statusErr != nil {
		return fmt.Errorf("failed to update egress firewall status %s, error: %w",
			GetEgressFirewallNamespacedName(egressFirewall), statusErr)
	}

	return err
}

func (oc *EFController) buildEgressFirewallConstruct(egressFirewall *egressfirewallapi.EgressFirewall) (*egressFirewall, error) {
	ef := cloneEgressFirewall(egressFirewall)
	var errorList []error
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			errorList = append(errorList, fmt.Errorf("egressFirewall for namespace %s has too many rules, max allowed number is %v",
				egressFirewall.Namespace, types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority))
			break
		}
		efr, err := oc.newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			errorList = append(errorList, fmt.Errorf("cannot create EgressFirewall Rule to destination %s for namespace %s: %w",
				egressFirewallRule.To.CIDRSelector, egressFirewall.Namespace, err))
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if len(errorList) > 0 {
		return nil, utilerrors.Join(errorList...)
	}
	return ef, nil
}

func (oc *EFController) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall, invalidACLs *[]*nbdb.ACL) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	ef, err := oc.buildEgressFirewallConstruct(egressFirewall)
	if err != nil {
		return err
	}

	pgName, err := oc.getNamespacePortGroupNameUnknownOwner(egressFirewall.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get portgroup for egress firewall %s/%s namespace: %w",
			egressFirewall.Namespace, egressFirewall.Name, err)
	}
	aclLoggingLevels, err := oc.getNamespaceACLLogging(ef.namespace)
	if err != nil {
		return fmt.Errorf("failed to get acl logging levels for egress firewall %s/%s: %w",
			egressFirewall.Namespace, egressFirewall.Name, err)
	}

	if err := oc.addEgressFirewallRules(ef, pgName, aclLoggingLevels, invalidACLs); err != nil {
		return err
	}
	return nil
}

// newEgressFirewallRule creates a new egressFirewallRule. For the logging level, it will pick either of
// aclLoggingAllow or aclLoggingDeny depending if this is an allow or deny rule.
func (oc *EFController) newEgressFirewallRule(rawEgressFirewallRule egressfirewallapi.EgressFirewallRule, id int) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	// Validate the egress firewall rule destination and update the appropriate
	// fields of efr.
	var err error
	efr.to.cidrSelector, efr.to.dnsName, efr.to.clusterSubnetIntersection, efr.to.nodeSelector, err =
		util.ValidateAndGetEgressFirewallDestination(rawEgressFirewallRule.To)
	if err != nil {
		return efr, err
	}
	// If nodeSelector is set then fetch the node addresses.
	if efr.to.nodeSelector != nil {
		efr.to.nodeAddrs = map[string][]string{}
		selector, err := metav1.LabelSelectorAsSelector(rawEgressFirewallRule.To.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid node selector: %w", err)
		}
		nodes, err := oc.nodeLister.List(selector)
		if err != nil {
			return efr, fmt.Errorf("unable to query nodes for egress firewall: %w", err)
		}
		for _, node := range nodes {
			hostAddresses, err := util.GetNodeHostAddrs(node)
			if err != nil {
				return efr, fmt.Errorf("unable to get node host CIDRs for egress firewall, node: %s: %w", node.Name, err)
			}
			efr.to.nodeAddrs[node.Name] = hostAddresses
		}
	}
	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

func efNodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels) ||
		util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
}

func (oc *EFController) updateEgressFirewallForNode(nodeName string) error {
	klog.V(3).Infof("Syncing node %q for egress firewall", nodeName)

	// cycle through egress firewalls and check if any match this node's labels
	egressFirewalls, err := oc.efLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list all egress firewalls: %w", err)
	}

	for _, egressFirewall := range egressFirewalls {
		ef, err := oc.buildEgressFirewallConstruct(egressFirewall)
		if err != nil {
			return fmt.Errorf("failed to create EgressFirewall object for egress firewall %s/%s: %w",
				egressFirewall.Namespace, egressFirewall.Name, err)
		}

		for _, rule := range ef.egressRules {
			// nodeSelector will always have a value, but it is mutually exclusive from cidrSelector and dnsName
			if len(rule.to.cidrSelector) != 0 || len(rule.to.dnsName) != 0 {
				continue
			}
			_, err := metav1.LabelSelectorAsSelector(rule.to.nodeSelector)
			if err != nil {
				klog.Errorf("Error while parsing label selector %#v for egress firewall in namespace %s",
					rule.to.nodeSelector, egressFirewall.Namespace)
				continue
			}
			// selector is a valid node selector, could be affected by this node, need to sync
			key, err := cache.MetaNamespaceKeyFunc(egressFirewall)
			if err != nil {
				return fmt.Errorf("failed to get key for egress firewall %s/%s: %w",
					egressFirewall.Namespace, egressFirewall.Name, err)
			}
			klog.Infof("Syncing egress firewall %s due to node %q change", key, nodeName)
			oc.controller.Reconcile(key)
			break
		}
	}

	return nil
}

func (oc *EFController) addEgressFirewallRules(ef *egressFirewall, pgName string,
	aclLogging *libovsdbutil.ACLLoggingLevels, invalidACLs *[]*nbdb.ACL, ruleIDs ...int) error {
	var ops []ovsdb.Operation
	var err error
	var hasDNS bool
	for _, rule := range ef.egressRules {
		// check if only specific rule ids are requested to be added
		if len(ruleIDs) > 0 {
			found := false
			for _, providedID := range ruleIDs {
				if rule.id == providedID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		var action string
		var matchTargets []matchTarget
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = nbdb.ACLActionAllow
		} else {
			action = nbdb.ACLActionDrop
		}
		if len(rule.to.nodeAddrs) > 0 {
			// sort node ips to ensure the same order when no changes are present
			// this ensure ACL recalculation won't happen just because of the order changes
			allIPs := []string{}
			for _, nodeIPs := range rule.to.nodeAddrs {
				allIPs = append(allIPs, nodeIPs...)
			}
			slices.Sort(allIPs)

			for _, addr := range allIPs {
				if utilnet.IsIPv6String(addr) {
					matchTargets = append(matchTargets, matchTarget{matchKindV6CIDR, addr, false})
				} else {
					matchTargets = append(matchTargets, matchTarget{matchKindV4CIDR, addr, false})
				}
			}
		} else if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTargets = []matchTarget{{matchKindV6CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			} else {
				matchTargets = []matchTarget{{matchKindV4CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			}
		} else if len(rule.to.dnsName) > 0 {
			hasDNS = true
			// rule based on DNS NAME
			dnsName := rule.to.dnsName
			// If DNSNameResolver is enabled, then use the egressFirewallExternalDNS to get the address
			// set corresponding to the DNS name, otherwise use the egressFirewallDNS
			// to get the address set.
			if config.OVNKubernetesFeature.EnableDNSNameResolver {
				// Convert the DNS name to lower case fully qualified domain name.
				dnsName = util.LowerCaseFQDN(rule.to.dnsName)
			}
			dnsNameAddressSets, err := oc.dnsNameResolver.Add(ef.namespace, dnsName)
			if err != nil {
				return fmt.Errorf("error with DNSNameResolver - %v", err)
			}
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName, rule.to.clusterSubnetIntersection})
			}
			if dnsNameIPv6ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName, rule.to.clusterSubnetIntersection})
			}
		}

		if len(matchTargets) == 0 {
			klog.Warningf("Egress Firewall rule: %#v has no destination...ignoring", *rule)
			// ensure the ACL is removed from OVN
			if err := oc.deleteEgressFirewallRule(ef.namespace, pgName, rule.id); err != nil {
				return err
			}
			continue
		}

		match := generateMatch(pgName, matchTargets, rule.ports)
		aclIDs := oc.GetEgressFirewallACLDbIDs(ef.namespace, rule.id)
		priority := types.EgressFirewallStartPriority - rule.id
		egressFirewallACL := libovsdbutil.BuildACLWithDefaultTier(
			aclIDs,
			priority,
			match,
			action,
			aclLogging,
			// since egressFirewall has direction to-lport, set type to ingress
			libovsdbutil.LportIngress,
		)

		// iterate through previously invalidACLs and validate them (remove from slice) if we are going to configure them
		s := *invalidACLs
		for i, acl := range s {
			if isEgressFirewallACLEqual(acl, egressFirewallACL) {
				copy(s[i:], s[i+1:])
				*invalidACLs = s[:len(s)-1]
				break
			}
		}

		ops, err = oc.createEgressFirewallACLOps(ops, egressFirewallACL, pgName)
		if err != nil {
			return err
		}
	}

	if !hasDNS {
		if err := oc.dnsNameResolver.Delete(ef.namespace); err != nil {
			return err
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed configure egress firewall %s/%s: %w",
			ef.namespace, ef.name, err)
	}

	return nil
}

// deleteStaleACLs cleans up 2 previous implementations:
//   - Cleanup the old implementation (using LRP) in local GW mode
//     For this it just deletes all LRP setup done for egress firewall
//   - Cleanup the old implementation (using ACLs on the join and node switches)
//     For this it deletes all the ACLs on the join and node switches, they will be created from scratch later.
func (oc *EFController) deleteStaleACLs() error {
	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority <= types.EgressFirewallStartPriority && item.Priority >= types.MinimumReservedEgressFirewallPriority
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting egress firewall policies on router %s: %v", types.OVNClusterRouter, err)
	}

	// delete acls from all switches, they reside on the port group now
	// Lookup all ACLs used for egress Firewalls
	aclPred := func(item *nbdb.ACL) bool {
		return item.Priority >= types.MinimumReservedEgressFirewallPriority && item.Priority <= types.EgressFirewallStartPriority
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclPred)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}
	if len(egressFirewallACLs) != 0 {
		err = batching.Batch[*nbdb.ACL](aclDeleteBatchSize, egressFirewallACLs, func(batchACLs []*nbdb.ACL) error {
			// optimize the predicate to exclude switches that don't reference deleting acls.
			aclsToDelete := sets.NewString()
			for _, acl := range batchACLs {
				aclsToDelete.Insert(acl.UUID)
			}
			swWithACLsPred := func(sw *nbdb.LogicalSwitch) bool {
				return aclsToDelete.HasAny(sw.ACLs...)
			}
			return libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(oc.nbClient, swWithACLsPred, batchACLs...)
		})
		if err != nil {
			return fmt.Errorf("failed to remove egress firewall acls from node logical switches: %v", err)
		}
	}
	return nil
}

// moveACLsToNamespacedPortGroups syncs db from the previous version where all ACLs were attached to the ClusterPortGroup
// to the new version where ACLs are attached to the namespace port groups.
func (oc *EFController) moveACLsToNamespacedPortGroups(existingEFNamespaces map[string]bool, efACLs []*nbdb.ACL) error {
	// find stale ACLs attached to a cluster port group, and move them to namespaced port groups
	clusterPG, err := libovsdbops.GetPortGroup(oc.nbClient, &nbdb.PortGroup{
		Name: libovsdbutil.GetPortGroupName(libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupCluster, defaultControllerOwner,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: types.ClusterPortGroupNameBase,
			})),
	})
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get cluster port group: %w", err)
	}
	staleUUIDs := sets.NewString(clusterPG.ACLs...)

	// move ACLs from cluster port group to per-namespace port group
	// old acl matches on the address set, which is still valid. match change from address set to port group will be
	// performed by the egress firewall handler.
	staleNamespaces := map[string][]*nbdb.ACL{}
	for _, acl := range efACLs {
		namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		// no key will be treated as an empty namespace and ACLs will just be deleted
		if staleUUIDs.Has(acl.UUID) {
			staleNamespaces[namespace] = append(staleNamespaces[namespace], acl)
		}
	}
	if len(staleNamespaces) == 0 {
		return nil
	}

	err = batching.BatchMap[*nbdb.ACL](aclChangePGBatchSize, staleNamespaces, func(batchNsACLs map[string][]*nbdb.ACL) error {
		var ops []ovsdb.Operation
		var err error
		for namespace, acls := range batchNsACLs {
			if namespace != "" && existingEFNamespaces[namespace] {
				pgName, err := oc.getNamespacePortGroupNameUnknownOwner(namespace)
				if err != nil {
					return fmt.Errorf("failed to get port group name for egress firewall ACL move with "+
						"namespace: %s, err: %w", namespace, err)
				}
				// re-attach from ClusterPortGroupNameBase to namespaced port group.
				// port group should exist, because namespace handler will create it.
				ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, acls...)
				if err != nil {
					return fmt.Errorf("failed to build cleanup ops: %w", err)
				}
			}
			// delete all EF ACLs from ClusterPortGroupNameBase
			ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops,
				clusterPG.Name, acls...)
			if err != nil {
				return fmt.Errorf("failed to build cleanup from ClusterPortGroup ops: %w", err)
			}
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to clean up egress firewall ACLs: %w", err)
		}
		return nil
	})
	return err
}

// isEgressFirewallACLEqual checks to see if an existing ACL matches a set of criteria
func isEgressFirewallACLEqual(acl1, acl2 *nbdb.ACL) bool {
	if acl1 == nil && acl2 == nil {
		return true
	}
	if acl1 == nil || acl2 == nil {
		return false
	}
	if acl1.Name == nil && acl2.Name == nil {
		if reflect.DeepEqual(acl1.ExternalIDs, acl2.ExternalIDs) {
			return true
		}
	}

	if acl1.Name == nil || acl2.Name == nil {
		return false
	}

	if fmt.Sprintf("%.63s", *acl1.Name) == fmt.Sprintf("%.63s", *acl2.Name) {
		return true
	}

	return false
}

// createEgressFirewallACLOps uses the previously generated elements and creates the
// acls for all node switches
func (oc *EFController) createEgressFirewallACLOps(ops []ovsdb.Operation, egressFirewallACL *nbdb.ACL, pgName string) ([]ovsdb.Operation, error) {
	var err error
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, oc.GetSamplingConfig(), egressFirewallACL)
	if err != nil {
		return nil, fmt.Errorf("failed to create egressFirewall ACL %#v: %v", egressFirewallACL, err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, egressFirewallACL)
	if err != nil {
		return nil, fmt.Errorf("failed to add egressFirewall ACL %#v to port group %s: %v",
			egressFirewallACL, pgName, err)
	}

	return ops, nil
}

func (oc *EFController) GetEgressFirewallACLDbIDsNoRule(namespace string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, defaultControllerOwner,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
		})
}

func (oc *EFController) GetEgressFirewallACLDbIDs(namespace string, ruleIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, defaultControllerOwner,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
			libovsdbops.RuleIndex:     strconv.Itoa(ruleIdx),
		})
}

func (oc *EFController) deleteEgressFirewallRule(namespace, pgName string, ruleIdx int) error {
	// Find ACLs for a given egressFirewall
	aclIDs := oc.GetEgressFirewallACLDbIDs(namespace, ruleIdx)
	pACL := libovsdbops.GetPredicate[*nbdb.ACL](aclIDs, nil)
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) == 0 {
		return nil
	}

	if len(egressFirewallACLs) > 1 {
		klog.Errorf("Duplicate ACL found for egress firewall %s, ruleIdx: %d", namespace, ruleIdx)
	}

	err = libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{pgName}, egressFirewallACLs...)
	return err
}

func (oc *EFController) setEgressFirewallStatus(egressFirewall *egressfirewallapi.EgressFirewall, numRemovedRules int, handlerErr error) error {
	var newMsg string
	if handlerErr != nil {
		newMsg = types.EgressFirewallErrorMsg + ": " + handlerErr.Error()
	} else {
		newMsg = types.EgressFirewallAppliedCorrectly
		key, err := cache.MetaNamespaceKeyFunc(egressFirewall)
		if err != nil {
			return err
		}
		ruleLength := len(egressFirewall.Spec.Egress)
		oc.ruleCounter.Store(key, uint32(ruleLength))
		metrics.UpdateEgressFirewallRuleCount(float64(ruleLength - numRemovedRules))
		metrics.IncrementEgressFirewallCount()
	}

	newMsg = types.GetZoneStatus(oc.zone, newMsg)
	needsUpdate := true
	for _, message := range egressFirewall.Status.Messages {
		if message == newMsg {
			// found previous status
			needsUpdate = false
			break
		}
	}
	if !needsUpdate {
		return nil
	}

	applyOptions := metav1.ApplyOptions{
		Force:        true,
		FieldManager: oc.zone,
	}

	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(egressfirewallapply.EgressFirewallStatus().
			WithMessages(newMsg))
	_, err := oc.kube.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, applyOptions)

	return err
}

func getNamespacePortGroupDbIDs(ns string, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
		})
}

func (oc *EFController) getNamespacePortGroupNameUnknownOwner(namespace string) (string, error) {
	activeNetwork, err := oc.networkManager.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to get active network for namespace %s: %v", namespace, err)
	}
	ownerController := activeNetwork.GetNetworkName() + "-network-controller"
	return libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, ownerController)), nil
}

func (oc *EFController) GetSamplingConfig() *libovsdbops.SamplingConfig {
	if oc.observManager != nil {
		return oc.observManager.SamplingConfig()
	}
	return nil
}

// getNamespaceACLLogging retrieves ACLLoggingLevels for the Namespace
func (oc *EFController) getNamespaceACLLogging(namespace string) (*libovsdbutil.ACLLoggingLevels, error) {
	ns, err := oc.namespaceLister.Get(namespace)
	if err != nil {
		return nil, err
	}
	if annotation, ok := ns.Annotations[util.AclLoggingAnnotation]; ok {
		if aclLogging, err := parseACLLogging(annotation); err == nil {
			klog.Infof("Namespace %s: ACL logging is set to deny=%s allow=%s", ns.Name, aclLogging.Deny, aclLogging.Allow)
			return &aclLogging, nil
		} else {
			klog.Warningf("Namespace %s: ACL logging contained malformed annotation %q, err: %q",
				namespace, ns.Annotations[util.AclLoggingAnnotation], err.Error())
		}
	}

	return &libovsdbutil.ACLLoggingLevels{
		Allow: "",
		Deny:  "",
	}, nil
}

// parseACLLogging parses the provided annotation values and sets aclLogging.Deny and
// aclLogging.Allow. If errors are encountered parsing the annotation, disable logging completely. If either
// value contains invalid input, disable logging for the respective key. This is needed to ensure idempotency.
// More details:
// *) If the provided annotation cannot be unmarshaled: Disable both Deny and Allow logging. Return an error.
// *) Valid values for "allow" and "deny" are  "alert", "warning", "notice", "info", "debug", "".
// *) Invalid values will return an error, and logging will be disabled for the respective key.
// *) In the following special cases, aclLogging.Deny and aclLogging.Allow. will both be reset to ""
//
//	without logging an error, meaning that logging will be switched off:
//	i) oc.aclLoggingEnabled == false
//	ii) annotation == ""
//	iii) annotation == "{}"
//
// *) If one of "allow" or "deny" can be parsed and has a valid value, but the other key is not present in the
//
//	annotation, then assume that this key should be disabled by setting its nsInfo value to "".
func parseACLLogging(annotation string) (libovsdbutil.ACLLoggingLevels, error) {
	var aclLevels libovsdbutil.ACLLoggingLevels
	var errs []error

	// If the annotation is "" or "{}", use empty strings. Otherwise, parse the annotation.
	if annotation != "" && annotation != "{}" {
		err := json.Unmarshal([]byte(annotation), &aclLevels)
		if err != nil {
			// Disable Allow and Deny logging to ensure idempotency.
			aclLevels.Allow = ""
			aclLevels.Deny = ""
			return aclLevels, fmt.Errorf("could not unmarshal namespace ACL annotation '%s', disabling logging, err: %q",
				annotation, err)
		}
	}

	// Valid log levels are the various pre-established levels or the empty string.
	validLogLevels := sets.NewString(nbdb.ACLSeverityAlert, nbdb.ACLSeverityWarning, nbdb.ACLSeverityNotice,
		nbdb.ACLSeverityInfo, nbdb.ACLSeverityDebug, "")

	// Set Deny logging.
	if !validLogLevels.Has(aclLevels.Deny) {
		aclLevels.Deny = ""
		errs = append(errs, fmt.Errorf("disabling deny logging due to invalid deny annotation. "+
			"%q is not a valid log severity", aclLevels.Deny))
	}

	// Set Allow logging.
	if !validLogLevels.Has(aclLevels.Allow) {
		aclLevels.Allow = ""
		errs = append(errs, fmt.Errorf("disabling allow logging due to an invalid allow annotation. "+
			"%q is not a valid log severity", aclLevels.Allow))
	}

	return aclLevels, utilerrors.Join(errs...)
}

// cloneEgressFirewall shallow copies the egressfirewallapi.EgressFirewall object provided.
// This concretely means that it create a new egressfirewallapi.EgressFirewall with the name and
// namespace set, but without any rules specified.
func cloneEgressFirewall(originalEgressfirewall *egressfirewallapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        originalEgressfirewall.Name,
		namespace:   originalEgressfirewall.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

// generateMatch generates the "match" section of ACL generation for egressFirewallRules.
// It is referentially transparent as all the elements have been validated before this function is called
// sample output:
// match=\"(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14\
func generateMatch(pgName string, destinations []matchTarget, dstPorts []egressfirewallapi.EgressFirewallPort) string {
	var dst string
	src := "inport == @" + pgName

	for _, entry := range destinations {
		if entry.value == "" {
			continue
		}
		ipDst, err := entry.toExpr()
		if err != nil {
			klog.Error(err)
			continue
		}
		if dst == "" {
			dst = ipDst
		} else {
			dst = strings.Join([]string{dst, ipDst}, " || ")
		}
	}
	match := fmt.Sprintf("(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}
	return match
}

// egressGetL4Match generates the rules for when ports are specified in an egressFirewall Rule
// since the ports can be specified in any order in an egressFirewallRule the best way to build up
// a single rule is to build up each protocol as you walk through the list and place the appropriate logic
// between the elements.
func egressGetL4Match(ports []egressfirewallapi.EgressFirewallPort) string {
	var udpString string
	var tcpString string
	var sctpString string
	for _, port := range ports {
		if corev1.Protocol(port.Protocol) == corev1.ProtocolUDP && udpString != "udp" {
			if port.Port == 0 {
				udpString = "udp"
			} else {
				udpString = fmt.Sprintf("%s udp.dst == %d ||", udpString, port.Port)
			}
		} else if corev1.Protocol(port.Protocol) == corev1.ProtocolTCP && tcpString != "tcp" {
			if port.Port == 0 {
				tcpString = "tcp"
			} else {
				tcpString = fmt.Sprintf("%s tcp.dst == %d ||", tcpString, port.Port)
			}
		} else if corev1.Protocol(port.Protocol) == corev1.ProtocolSCTP && sctpString != "sctp" {
			if port.Port == 0 {
				sctpString = "sctp"
			} else {
				sctpString = fmt.Sprintf("%s sctp.dst == %d ||", sctpString, port.Port)
			}
		}
	}
	// build the l4 match
	var l4Match string
	type tuple struct {
		protocolName     string
		protocolFormated string
	}
	list := []tuple{
		{
			protocolName:     "udp",
			protocolFormated: udpString,
		},
		{
			protocolName:     "tcp",
			protocolFormated: tcpString,
		},
		{
			protocolName:     "sctp",
			protocolFormated: sctpString,
		},
	}
	for _, entry := range list {
		if entry.protocolName == entry.protocolFormated {
			if l4Match == "" {
				l4Match = fmt.Sprintf("(%s)", entry.protocolName)
			} else {
				l4Match = fmt.Sprintf("%s || (%s)", l4Match, entry.protocolName)
			}
		} else {
			if l4Match == "" && entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("(%s && (%s))", entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			} else if entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("%s || (%s && (%s))", l4Match, entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			}
		}
	}
	return fmt.Sprintf("(%s)", l4Match)
}

func getV4ClusterSubnetsExclusion() string {
	var exclusions []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv4CIDR(clusterSubnet.CIDR) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip4", clusterSubnet.CIDR))
		}
	}
	return strings.Join(exclusions, "&&")
}

func getV6ClusterSubnetsExclusion() string {
	var exclusions []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip6", clusterSubnet.CIDR))
		}
	}
	return strings.Join(exclusions, "&&")
}

func GetEgressFirewallNamespacedName(egressFirewall *egressfirewallapi.EgressFirewall) string {
	return fmt.Sprintf("%v/%v", egressFirewall.Namespace, egressFirewall.Name)
}

func (m *matchTarget) toExpr() (string, error) {
	var match string
	switch m.kind {
	case matchKindV4CIDR:
		match = fmt.Sprintf("ip4.dst == %s", m.value)
		if m.clusterSubnetIntersection {
			match = fmt.Sprintf("%s && %s", match, getV4ClusterSubnetsExclusion())
		}
	case matchKindV6CIDR:
		match = fmt.Sprintf("ip6.dst == %s", m.value)
		if m.clusterSubnetIntersection {
			match = fmt.Sprintf("%s && %s", match, getV6ClusterSubnetsExclusion())
		}
	case matchKindV4AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip4.dst == $%s", m.value)
		}
	case matchKindV6AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip6.dst == $%s", m.value)
		}
	default:
		return "", fmt.Errorf("invalid MatchKind")
	}
	return match, nil
}
