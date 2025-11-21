package services

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/core"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// magic string used in vips to indicate that the node's physical
// ips should be substituted in
const placeholderNodeIPs = "node"

// lbConfig is the abstract desired load balancer configuration.
// vips and endpoints are mixed families.
type lbConfig struct {
	vips     []string        // just ip or the special value "node" for the node's physical IPs (i.e. NodePort)
	protocol corev1.Protocol // TCP, UDP, or SCTP
	inport   int32           // the incoming (virtual) port number

	clusterEndpoints util.LBEndpointsList            // addresses of cluster-wide endpoints
	nodeEndpoints    map[string]util.LBEndpointsList // node -> addresses of local endpoints

	// if true, then vips added on the router are in "local" mode
	// that means, skipSNAT, and remove any non-local endpoints.
	// (see below)
	externalTrafficLocal bool
	// if true, then vips added on the switch are in "local" mode
	// that means, remove any non-local endpoints.
	internalTrafficLocal bool
	// indicates if this LB is configuring service of type NodePort.
	hasNodePort bool
}

// makeNodeSwitchTargetAddrs computes the target Addrs (IP + Port) for load balancer rules on a node's switch.
// For services with ExternalTrafficPolicy=Local or InternalTrafficPolicy=Local, it filters
// targets to include only endpoints local to the specified node. Otherwise, it returns all
// cluster-wide endpoints. The v4Changed and v6Changed return values indicate whether the
// targets differ from the cluster-wide endpoints, which is used to determine if templates are needed.
func makeNodeSwitchTargetAddrs(node string, c *lbConfig) (ipv4Targets, ipv6Targets []Addr, v4Changed, v6Changed bool) {
	clusterEndpointsIPv4Targets := createTargets(c.clusterEndpoints.GetV4Destinations())
	clusterEndpointsIPv6Targets := createTargets(c.clusterEndpoints.GetV6Destinations())

	// For everything else, targets are created from cluster endpoints ...
	ipv4Targets = clusterEndpointsIPv4Targets
	ipv6Targets = clusterEndpointsIPv6Targets

	// ... but for ETP=Local or ITP=Local, targets are created from c.nodeEndpoints[node] if it exists, or are empty
	// otherwise.
	// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets.
	// NOTE: on the switches, filtered eps are used only by masqueradeVIP.
	// For InternalTrafficPolicy=Local, remove non-local endpoints from the switch targets only.
	if c.externalTrafficLocal || c.internalTrafficLocal {
		localIPv4Targets := []Addr{}
		localIPv6Targets := []Addr{}
		if localEndpoints, ok := c.nodeEndpoints[node]; ok {
			localIPv4Targets = createTargets(localEndpoints.GetV4Destinations())
			localIPv6Targets = createTargets(localEndpoints.GetV6Destinations())
		}
		ipv4Targets = localIPv4Targets
		ipv6Targets = localIPv6Targets
	}

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length.
	v4Changed = len(ipv4Targets) != len(clusterEndpointsIPv4Targets)
	v6Changed = len(ipv6Targets) != len(clusterEndpointsIPv6Targets)

	return
}

// makeNodeRouterTargetAddrs computes the target Addrs (IP + Port) for load balancer rules on a node's gateway router.
// For services with ExternalTrafficPolicy=Local, it filters targets to include only endpoints local
// to the specified node. Additionally, for any targets that match the node's own addresses, it replaces
// them with the host masquerade IP to enable proper hairpin traffic handling.
// The v4Changed and v6Changed return values indicate whether the targets differ from the cluster-wide
// endpoints, which is used to determine if templates are needed.
func makeNodeRouterTargetAddrs(node *nodeInfo, c *lbConfig, hostMasqueradeIPV4, hostMasqueradeIPV6 string) (ipv4Targets, ipv6Targets []Addr, v4Changed, v6Changed bool) {
	clusterEndpointsIPv4Targets := createTargets(c.clusterEndpoints.GetV4Destinations())
	clusterEndpointsIPv6Targets := createTargets(c.clusterEndpoints.GetV6Destinations())

	// For everything else, targets are created from cluster endpoints ...
	ipv4Targets = clusterEndpointsIPv4Targets
	ipv6Targets = clusterEndpointsIPv6Targets

	// ... but for ETP=Local, targets are created from c.nodeEndpoints[node] if it exists, or are empty
	// otherwise.
	// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets.
	// NOTE: on the switches, filtered eps are used only by masqueradeVIP.
	if c.externalTrafficLocal {
		localIPv4Targets := []Addr{}
		localIPv6Targets := []Addr{}
		if localEndpoints, ok := c.nodeEndpoints[node.name]; ok {
			localIPv4Targets = createTargets(localEndpoints.GetV4Destinations())
			localIPv6Targets = createTargets(localEndpoints.GetV6Destinations())
		}
		ipv4Targets = localIPv4Targets
		ipv6Targets = localIPv6Targets
	}

	// TODO: For all scenarios the lbAddress should be set to hostAddressesStr but this is breaking CI needs more investigation
	lbAddresses := node.hostAddressesStr()
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		lbAddresses = node.l3gatewayAddressesStr()
	}

	// Any targets local to the node need to have a special harpin IP added, but only for the router LB.
	ipv4Targets, v4Updated := updateIPsSlice(ipv4Targets, lbAddresses, []string{hostMasqueradeIPV4})
	ipv6Targets, v6Updated := updateIPsSlice(ipv6Targets, lbAddresses, []string{hostMasqueradeIPV6})

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length - OR the targets
	// were updated with the hairpin IP.
	v4Changed = len(ipv4Targets) != len(clusterEndpointsIPv4Targets) || v4Updated
	v6Changed = len(ipv6Targets) != len(clusterEndpointsIPv6Targets) || v6Updated

	return
}

// updateIPsSlice will search for values of oldIPs in the slice "s" and update it with newIPs values of same IP family.
func updateIPsSlice(s []Addr, oldIPs, newIPs []string) ([]Addr, bool) {
	n := make([]Addr, len(s))
	copy(n, s)
	updated := false
	for i, entry := range s {
		for _, oldIP := range oldIPs {
			if entry.IP == oldIP {
				for _, newIP := range newIPs {
					if utilnet.IsIPv6(net.ParseIP(oldIP)) {
						if utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = Addr{IP: newIP, Port: entry.Port}
							updated = true
							break
						}
					} else {
						if !utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = Addr{IP: newIP, Port: entry.Port}
							updated = true
							break
						}
					}
				}
				break
			}
		}
	}
	return n, updated
}

// just used for consistent ordering
var protos = []corev1.Protocol{
	corev1.ProtocolTCP,
	corev1.ProtocolUDP,
	corev1.ProtocolSCTP,
}

// buildServiceLBConfigs generates the abstract load balancer(s) configurations for each service. The abstract configurations
// are then expanded in buildClusterLBs and buildPerNodeLBs to the full list of OVN LBs desired.
//
// It creates three lists of configurations:
// - the per-node configs, which are load balancers that, for some reason must be expanded per-node. (see below for why)
// - the template configs, which are template load balancers that are similar across the whole cluster.
// - the cluster-wide configs, which are load balancers that can be the same across the whole cluster.
//
// For a "standard" ClusterIP service (possibly with ExternalIPS or external LoadBalancer Status IPs),
// a single cluster-wide LB will be created.
//
// Per-node LBs will be created for
// - services with NodePort set
// - services with host-network endpoints
// - services with ExternalTrafficPolicy=Local
// - services with InternalTrafficPolicy=Local
//
// Template LBs will be created for
//   - services with NodePort set but *without* ExternalTrafficPolicy=Local or
//     affinity timeout set.
func buildServiceLBConfigs(service *corev1.Service, endpointSlices []*discovery.EndpointSlice, nodeInfos []nodeInfo,
	useLBGroup, useTemplates bool) (perNodeConfigs, templateConfigs, clusterConfigs []lbConfig) {

	needsAffinityTimeout := hasSessionAffinityTimeOut(service)

	nodes := sets.New[string]()
	for _, n := range nodeInfos {
		nodes.Insert(n.name)
	}
	// get all the endpoints classified by port and by port,node
	needsLocalEndpoints := util.ServiceExternalTrafficPolicyLocal(service) || util.ServiceInternalTrafficPolicyLocal(service)
	// GetEndpointsForService will in theory never return an error (see its implementation and comments there). However,
	// if so, log it here and simply continue.
	portToClusterEndpoints, portToNodeToEndpoints, err := util.GetEndpointsForService(endpointSlices, service, nodes, true, needsLocalEndpoints)
	if err != nil {
		if service != nil {
			klog.Warningf("Failed to get endpoints for service %s/%s during LB config build: %v", service.Namespace, service.Name, err)
		} else {
			klog.Warningf("Failed to get endpoints for service during LB config build: %v", err)
		}
	}
	for _, svcPort := range service.Spec.Ports {
		svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
		clusterEndpoints, _ := portToClusterEndpoints.GetLBEndpoints(svcPortKey)
		nodeEndpoints := portToNodeToEndpoints.Get(svcPortKey)
		// if ExternalTrafficPolicy or InternalTrafficPolicy is local, then we need to do things a bit differently
		externalTrafficLocal := util.ServiceExternalTrafficPolicyLocal(service)
		internalTrafficLocal := util.ServiceInternalTrafficPolicyLocal(service)

		// NodePort services get a per-node load balancer, but with the node's physical IP as the vip
		// Thus, the vip "node" will be expanded later.
		// This is NEVER influenced by InternalTrafficPolicy
		if svcPort.NodePort != 0 {
			nodePortLBConfig := lbConfig{
				protocol:             svcPort.Protocol,
				inport:               svcPort.NodePort,
				vips:                 []string{placeholderNodeIPs}, // shortcut for all-physical-ips
				clusterEndpoints:     clusterEndpoints,
				nodeEndpoints:        nodeEndpoints,
				externalTrafficLocal: externalTrafficLocal,
				internalTrafficLocal: false, // always false for non-ClusterIPs
				hasNodePort:          true,
			}
			// Only "plain" NodePort services (no ETP, no affinity timeout)
			// can use load balancer templates.
			if !useLBGroup || !useTemplates || externalTrafficLocal || needsAffinityTimeout {
				perNodeConfigs = append(perNodeConfigs, nodePortLBConfig)
			} else {
				templateConfigs = append(templateConfigs, nodePortLBConfig)
			}
		}

		// Build up list of vips and externalVips
		vips := util.GetClusterIPs(service)
		externalVips := util.GetExternalAndLBIPs(service)

		// if ETP=Local, then treat ExternalIPs and LoadBalancer IPs specially
		// otherwise, they're just cluster IPs
		// This is NEVER influenced by InternalTrafficPolicy
		if externalTrafficLocal && len(externalVips) > 0 {
			externalIPConfig := lbConfig{
				protocol:             svcPort.Protocol,
				inport:               svcPort.Port,
				vips:                 externalVips,
				clusterEndpoints:     clusterEndpoints,
				nodeEndpoints:        nodeEndpoints,
				externalTrafficLocal: true,
				internalTrafficLocal: false, // always false for non-ClusterIPs
				hasNodePort:          false,
			}
			perNodeConfigs = append(perNodeConfigs, externalIPConfig)
		} else {
			vips = append(vips, externalVips...)
		}

		// Build the clusterIP config
		// This is NEVER influenced by ExternalTrafficPolicy
		clusterIPConfig := lbConfig{
			protocol:             svcPort.Protocol,
			inport:               svcPort.Port,
			vips:                 vips,
			clusterEndpoints:     clusterEndpoints,
			nodeEndpoints:        nodeEndpoints,
			externalTrafficLocal: false, // always false for ClusterIPs
			internalTrafficLocal: internalTrafficLocal,
			hasNodePort:          false,
		}

		// Normally, the ClusterIP LB is global (on all node switches and routers),
		// unless any of the following are true:
		// - Any of the endpoints are host-network
		// - ETP=local service backed by non-local-host-networked endpoints
		//
		// In that case, we need to create per-node LBs.
		if hasHostEndpoints(clusterEndpoints) || internalTrafficLocal {
			perNodeConfigs = append(perNodeConfigs, clusterIPConfig)
		} else {
			clusterConfigs = append(clusterConfigs, clusterIPConfig)
		}
	}

	return
}

func makeLBNameForNetwork(service *corev1.Service, proto corev1.Protocol, scope string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedLoadBalancerName(makeLBName(service, proto, scope))
}

// makeLBName creates the load balancer name - used to minimize churn
func makeLBName(service *corev1.Service, proto corev1.Protocol, scope string) string {
	return fmt.Sprintf("Service_%s/%s_%s_%s",
		service.Namespace, service.Name,
		proto, scope)
}

// buildClusterLBs takes a list of lbConfigs and aggregates them
// in to one ovn LB per protocol.
//
// It takes a list of (proto:[vips]:port -> [endpoints]) configs and re-aggregates
// them to a list of (proto:[vip:port -> [endpoint:port]])
// This load balancer is attached to all node switches. In shared-GW mode, it is also on all routers
// The input netInfo is needed to get the right LB groups and network IDs for the specified network.
func buildClusterLBs(service *corev1.Service, configs []lbConfig, nodeInfos []nodeInfo, useLBGroup bool, netInfo util.NetInfo) []LB {
	var nodeSwitches []string
	var nodeRouters []string
	var groups []string
	if useLBGroup {
		nodeSwitches = make([]string, 0)
		nodeRouters = make([]string, 0)
		groups = []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName)}
	} else {
		nodeSwitches = make([]string, 0, len(nodeInfos))
		nodeRouters = make([]string, 0, len(nodeInfos))
		groups = make([]string, 0)

		for _, node := range nodeInfos {
			nodeSwitches = append(nodeSwitches, node.switchName)
			// For shared gateway, add to the node's GWR as well.
			// The node may not have a gateway router - it might be waiting initialization, or
			// might have disabled GWR creation via the k8s.ovn.org/l3-gateway-config annotation
			if node.gatewayRouterName != "" {
				nodeRouters = append(nodeRouters, node.gatewayRouterName)
			}
		}
	}

	cbp := configsByProto(configs)

	out := []LB{}
	for _, proto := range protos {
		cfgs, ok := cbp[proto]
		if !ok {
			continue
		}
		lb := LB{
			Name:        makeLBNameForNetwork(service, proto, "cluster", netInfo),
			Protocol:    string(proto),
			ExternalIDs: getExternalIDsForLoadBalancer(service, netInfo),
			Opts:        lbOpts(service),

			Switches: nodeSwitches,
			Routers:  nodeRouters,
			Groups:   groups,
		}

		for _, config := range cfgs {
			if config.externalTrafficLocal {
				klog.Errorf("BUG: service %s/%s has routerLocalMode=true for cluster-wide lbConfig",
					service.Namespace, service.Name)
			}

			v4targets := createTargets(config.clusterEndpoints.GetV4Destinations())
			v6targets := createTargets(config.clusterEndpoints.GetV6Destinations())

			rules := make([]LBRule, 0, len(config.vips))
			for _, vip := range config.vips {
				if vip == placeholderNodeIPs {
					klog.Errorf("BUG: service %s/%s has a \"node\" vip for a cluster-wide lbConfig",
						service.Namespace, service.Name)
					continue
				}
				targets := v4targets
				if utilnet.IsIPv6String(vip) {
					targets = v6targets
				}

				rules = append(rules, LBRule{
					Source: Addr{
						IP:   vip,
						Port: config.inport,
					},
					Targets: targets,
				})
			}
			lb.Rules = append(lb.Rules, rules...)
		}

		out = append(out, lb)
	}
	return out
}

// buildTemplateLBs takes a list of lbConfigs and expands them to one template
// LB per protocol (per address family).
//
// Template LBs are created for nodeport services and are attached to each
// node's gateway router + switch via load balancer groups.  Their vips and
// backends are OVN chassis template variables that expand to the chassis'
// node IP and set of backends.
//
// Note:
// NodePort services with ETP=local or affinity timeout set still need
// non-template per-node LBs.
//
// The input netInfo is needed to get the right LB groups and network IDs for the specified network.
func buildTemplateLBs(service *corev1.Service, configs []lbConfig, nodes []nodeInfo,
	nodeIPv4Templates, nodeIPv6Templates *NodeIPsTemplates, netInfo util.NetInfo) []LB {

	cbp := configsByProto(configs)
	eids := getExternalIDsForLoadBalancer(service, netInfo)
	out := make([]LB, 0, len(configs))

	for _, proto := range protos {
		configs, ok := cbp[proto]
		if !ok {
			continue
		}

		switchV4Rules := make([]LBRule, 0, len(configs))
		switchV6Rules := make([]LBRule, 0, len(configs))
		routerV4Rules := make([]LBRule, 0, len(configs))
		routerV6Rules := make([]LBRule, 0, len(configs))

		optsV4 := lbTemplateOpts(service, corev1.IPv4Protocol)
		optsV6 := lbTemplateOpts(service, corev1.IPv6Protocol)

		for _, cfg := range configs {
			switchV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV4.AddressFamily, "node_switch_template", netInfo))
			switchV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV6.AddressFamily, "node_switch_template", netInfo))

			routerV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV4.AddressFamily, "node_router_template", netInfo))
			routerV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV6.AddressFamily, "node_router_template", netInfo))

			for range cfg.vips {
				klog.V(5).Infof("buildTemplateLBs() service %s/%s adding rules for network=%s",
					service.Namespace, service.Name, netInfo.GetNetworkName())

				// If all targets have exactly the same IPs on all nodes there's
				// no need to use a template, just use the same list of explicit
				// targets on all nodes.
				switchV4TargetNeedsTemplate := false
				switchV6TargetNeedsTemplate := false
				routerV4TargetNeedsTemplate := false
				routerV6TargetNeedsTemplate := false

				for _, node := range nodes {
					switchV4TargetAddrs, switchV6TargetAddrs, v4Changed, v6Changed := makeNodeSwitchTargetAddrs(node.name, &cfg)
					if v4Changed {
						switchV4TargetNeedsTemplate = true
					}
					if v6Changed {
						switchV6TargetNeedsTemplate = true
					}

					routerV4TargetAddrs, routerV6TargetAddrs, v4Changed, v6Changed := makeNodeRouterTargetAddrs(
						&node,
						&cfg,
						config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
						config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())
					if v4Changed {
						routerV4TargetNeedsTemplate = true
					}
					if v6Changed {
						routerV6TargetNeedsTemplate = true
					}

					switchV4TemplateTarget.Value[node.chassisID] = addrsToString(switchV4TargetAddrs)
					switchV6TemplateTarget.Value[node.chassisID] = addrsToString(switchV6TargetAddrs)

					routerV4TemplateTarget.Value[node.chassisID] = addrsToString(routerV4TargetAddrs)
					routerV6TemplateTarget.Value[node.chassisID] = addrsToString(routerV6TargetAddrs)
				}

				sharedV4Targets := []Addr{}
				sharedV6Targets := []Addr{}
				if !switchV4TargetNeedsTemplate || !routerV4TargetNeedsTemplate {
					sharedV4Targets = createTargets(cfg.clusterEndpoints.GetV4Destinations())
				}
				if !switchV6TargetNeedsTemplate || !routerV6TargetNeedsTemplate {
					sharedV6Targets = createTargets(cfg.clusterEndpoints.GetV6Destinations())
				}

				for _, nodeIPv4Template := range nodeIPv4Templates.AsTemplates() {

					if switchV4TargetNeedsTemplate {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: []Addr{{Template: switchV4TemplateTarget}},
						})
					} else {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: sharedV4Targets,
						})
					}

					if routerV4TargetNeedsTemplate {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: []Addr{{Template: routerV4TemplateTarget}},
						})
					} else {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: sharedV4Targets,
						})
					}
				}

				for _, nodeIPv6Template := range nodeIPv6Templates.AsTemplates() {

					if switchV6TargetNeedsTemplate {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: []Addr{{Template: switchV6TemplateTarget}},
						})
					} else {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: sharedV6Targets,
						})
					}

					if routerV6TargetNeedsTemplate {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: []Addr{{Template: routerV6TemplateTarget}},
						})
					} else {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: sharedV6Targets,
						})
					}
				}
			}
		}

		if nodeIPv4Templates.Len() > 0 {
			if len(switchV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_switch_template_IPv4", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)},
					Rules:       switchV4Rules,
					Templates:   getTemplatesFromRulesTargets(switchV4Rules),
				})
			}
			if len(routerV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router_template_IPv4", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)},
					Rules:       routerV4Rules,
					Templates:   getTemplatesFromRulesTargets(routerV4Rules),
				})
			}
		}

		if nodeIPv6Templates.Len() > 0 {
			if len(switchV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_switch_template_IPv6", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)},
					Rules:       switchV6Rules,
					Templates:   getTemplatesFromRulesTargets(switchV6Rules),
				})
			}
			if len(routerV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router_template_IPv6", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)},
					Rules:       routerV6Rules,
					Templates:   getTemplatesFromRulesTargets(routerV6Rules),
				})
			}
		}
	}

	merged := mergeLBs(out)
	if len(merged) != len(out) {
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d for network=%s",
			service.Namespace, service.Name,
			len(out), len(merged), netInfo.GetNetworkName())
	}

	return merged
}

// buildPerNodeLBs takes a list of lbConfigs and expands them to one LB per protocol per node
//
// Per-node lbs are created for
// - clusterip services with host-network endpoints, which are attached to each node's gateway router + switch
// - nodeport services are attached to each node's gateway router + switch, vips are the node's physical IPs (except if etp=local+ovnk backend pods)
// - any services with host-network endpoints
// - services with external IPs / LoadBalancer Status IPs
//
// HOWEVER, we need to replace, on each nodes gateway router only, any host-network endpoints with a special loopback address
// see https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/host_to_services_OpenFlow.md
// This is for host -> serviceip -> host hairpin
//
// For ExternalTrafficPolicy=local, all "External" IPs (NodePort, ExternalIPs, Loadbalancer Status) have:
// - targets filtered to only local targets
// - SkipSNAT enabled
// - NodePort LB on the switch will have masqueradeIP as the vip to handle etp=local for LGW case.
// This results in the creation of an additional load balancer on the GatewayRouters and NodeSwitches.
//
// The input netInfo is needed to get the right network IDs for the specified network.
func buildPerNodeLBs(service *corev1.Service, configs []lbConfig, nodes []nodeInfo, netInfo util.NetInfo) []LB {
	cbp := configsByProto(configs)
	eids := getExternalIDsForLoadBalancer(service, netInfo)

	out := make([]LB, 0, len(nodes)*len(configs))

	// output is one LB per node per protocol with one rule per vip
	for _, node := range nodes {
		for _, proto := range protos {
			configs, ok := cbp[proto]
			if !ok {
				continue
			}

			// attach to router & switch,
			// rules may or may not be different
			routerRules := make([]LBRule, 0, len(configs))
			noSNATRouterRules := make([]LBRule, 0)
			switchRules := make([]LBRule, 0, len(configs))

			for _, cfg := range configs {
				nodeSwitchV4TargetAddrs, nodeSwitchV6TargetAddrs, _, _ := makeNodeSwitchTargetAddrs(node.name, &cfg)

				nodeRouterV4TargetAddrs, nodeRouterV6TargetsAddrs, _, _ := makeNodeRouterTargetAddrs(
					&node,
					&cfg,
					config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
					config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

				clusterEndpointsSwitchV4targets := createTargets(cfg.clusterEndpoints.GetV4Destinations())
				clusterEndpointsSwitchV6targets := createTargets(cfg.clusterEndpoints.GetV6Destinations())

				// Substitute the special vip "node" for the node's physical ips
				// This is used for nodeport
				vips := make([]string, 0, len(cfg.vips))
				for _, vip := range cfg.vips {
					if vip == placeholderNodeIPs {
						if !node.nodePortDisabled {
							vips = append(vips, node.hostAddressesStr()...)
						}
					} else {
						vips = append(vips, vip)
					}
				}

				for _, vip := range vips {
					isv6 := utilnet.IsIPv6String((vip))

					if cfg.externalTrafficLocal && cfg.hasNodePort {
						// add special masqueradeIP as a vip if its nodePort svc with ETP=local
						mvip := config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
						targetsETP := nodeSwitchV4TargetAddrs
						if isv6 {
							mvip = config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
							targetsETP = nodeSwitchV6TargetAddrs
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: mvip, Port: cfg.inport},
							Targets: targetsETP,
						})
					}

					if cfg.internalTrafficLocal && util.IsClusterIP(vip) { // ITP only applicable to CIP
						targetsITP := nodeSwitchV4TargetAddrs
						if isv6 {
							targetsITP = nodeSwitchV6TargetAddrs
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: cfg.inport},
							Targets: targetsITP,
						})
					} else {
						// build switch rules
						targets := clusterEndpointsSwitchV4targets
						if isv6 {
							targets = clusterEndpointsSwitchV6targets
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: cfg.inport},
							Targets: targets,
						})
					}

					// There is also a per-router rule
					// with nodeRouterTargets that *may* be different
					nodeRouterTargets := nodeRouterV4TargetAddrs
					if isv6 {
						nodeRouterTargets = nodeRouterV6TargetsAddrs
					}
					rule := LBRule{
						Source:  Addr{IP: vip, Port: cfg.inport},
						Targets: nodeRouterTargets,
					}

					// in other words, is this ExternalTrafficPolicy=local?
					// if so, this gets a separate load balancer with SNAT disabled
					// (but there's no need to do this if the list of node router targets is empty)
					if cfg.externalTrafficLocal && len(nodeRouterTargets) > 0 {
						noSNATRouterRules = append(noSNATRouterRules, rule)
					} else {
						routerRules = append(routerRules, rule)
					}
				}
			}

			// If switch and router rules are identical, coalesce
			if reflect.DeepEqual(switchRules, routerRules) && len(switchRules) > 0 && node.gatewayRouterName != "" {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router+switch_"+node.name, netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        lbOpts(service),
					Routers:     []string{node.gatewayRouterName},
					Switches:    []string{node.switchName},
					Rules:       routerRules,
				})
			} else {
				if len(routerRules) > 0 && node.gatewayRouterName != "" {
					out = append(out, LB{
						Name:        makeLBNameForNetwork(service, proto, "node_router_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       routerRules,
					})
				}
				if len(noSNATRouterRules) > 0 && node.gatewayRouterName != "" {
					lb := LB{
						Name:        makeLBNameForNetwork(service, proto, "node_local_router_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       noSNATRouterRules,
					}
					lb.Opts.SkipSNAT = true
					out = append(out, lb)
				}

				if len(switchRules) > 0 {
					out = append(out, LB{
						Name:        makeLBNameForNetwork(service, proto, "node_switch_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Switches:    []string{node.switchName},
						Rules:       switchRules,
					})
				}
			}
		}
	}

	merged := mergeLBs(out)
	if len(merged) != len(out) {
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d for network=%s",
			service.Namespace, service.Name,
			len(out), len(merged), netInfo.GetNetworkName())
	}

	return merged
}

// configsByProto buckets a list of configs by protocol (tcp, udp, sctp)
func configsByProto(configs []lbConfig) map[corev1.Protocol][]lbConfig {
	out := map[corev1.Protocol][]lbConfig{}
	for _, config := range configs {
		out[config.protocol] = append(out[config.protocol], config)
	}
	return out
}

func getSessionAffinityTimeOut(service *corev1.Service) int32 {
	// NOTE: This if condition is actually not needed, present only for protection against nil value as good coding practice,
	// The API always puts the default value of 10800 whenever sessionAffinity == ClientIP if timeout is not explicitly set
	// There is no ClientIP session affinity without a timeout set.
	if service.Spec.SessionAffinityConfig == nil ||
		service.Spec.SessionAffinityConfig.ClientIP == nil ||
		service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds == nil {
		return core.DefaultClientIPServiceAffinitySeconds // default value
	}
	return *service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds
}

func hasSessionAffinityTimeOut(service *corev1.Service) bool {
	return service.Spec.SessionAffinity == corev1.ServiceAffinityClientIP &&
		getSessionAffinityTimeOut(service) != core.MaxClientIPServiceAffinitySeconds
}

// lbOpts generates the OVN load balancer options from the kubernetes Service.
func lbOpts(service *corev1.Service) LBOpts {
	affinity := service.Spec.SessionAffinity == corev1.ServiceAffinityClientIP
	lbOptions := LBOpts{
		SkipSNAT: false, // never service-wide, ExternalTrafficPolicy-specific
	}

	lbOptions.Reject = true
	lbOptions.EmptyLBEvents = false

	if config.Kubernetes.OVNEmptyLbEvents {
		if unidling.HasIdleAt(service) {
			lbOptions.Reject = false
			lbOptions.EmptyLBEvents = true
		}

		if unidling.IsOnGracePeriod(service) {
			lbOptions.Reject = false

			// Setting to true even if we don't need empty_lb_events from OVN during grace period
			// because OVN does not support having
			// <no_backends> event=false reject=false
			// Remove the following line when https://bugzilla.redhat.com/show_bug.cgi?id=2177173
			// is fixed
			lbOptions.EmptyLBEvents = true
		}
	}

	if affinity {
		lbOptions.AffinityTimeOut = getSessionAffinityTimeOut(service)
	}
	return lbOptions
}

func lbTemplateOpts(service *corev1.Service, addressFamily corev1.IPFamily) LBOpts {
	lbOptions := lbOpts(service)

	// Only template LBs need an explicit address family.
	lbOptions.AddressFamily = addressFamily
	lbOptions.Template = true
	return lbOptions
}

// mergeLBs joins two LBs together if it is safe to do so.
//
// an LB can be merged if the protocol, rules, and options are the same,
// and only the switches and routers are different.
func mergeLBs(lbs []LB) []LB {
	if len(lbs) == 1 {
		return lbs
	}
	out := make([]LB, 0, len(lbs))

outer:
	for _, lb := range lbs {
		for i := range out {
			// If mergeable, rather than inserting lb to out, just add switches, routers, groups
			// and drop
			if canMergeLB(lb, out[i]) {
				out[i].Switches = append(out[i].Switches, lb.Switches...)
				out[i].Routers = append(out[i].Routers, lb.Routers...)
				out[i].Groups = append(out[i].Groups, lb.Groups...)

				if !strings.HasSuffix(out[i].Name, "_merged") {
					out[i].Name += "_merged"
				}
				continue outer
			}
		}
		out = append(out, lb)
	}

	return out
}

// canMergeLB returns true if two LBs are mergeable.
// We know that the ExternalIDs will be the same, so we don't need to compare them.
// All that matters is the protocol and rules are the same.
func canMergeLB(a, b LB) bool {
	if a.Protocol != b.Protocol {
		return false
	}

	if !reflect.DeepEqual(a.Opts, b.Opts) {
		return false
	}

	// While rules are actually a set, we generate all our lbConfigs from a single source
	// so the ordering will be the same. Thus, we can cheat and just reflect.DeepEqual
	return reflect.DeepEqual(a.Rules, b.Rules)
}

// createTargets takes a list of util.IPPorts and converts them to a list of Addr.
func createTargets(ipps []util.IPPort) []Addr {
	out := make([]Addr, 0, len(ipps))
	for _, ipp := range ipps {
		out = append(out, Addr{IP: ipp.IP, Port: ipp.Port})
	}
	return out
}
