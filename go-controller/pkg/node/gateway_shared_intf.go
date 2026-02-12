package node

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/egressip"
	nodeipt "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	protoPrefixV4 = "ip"
	protoPrefixV6 = "ipv6"
	// etpSvcOpenFlowCookie identifies constant open flow rules added to the host OVS
	// bridge to move packets between host and external for etp=local traffic.
	// The hex number 0xe745ecf105, represents etp(e74)-service(5ec)-flows which makes it easier for debugging.
	etpSvcOpenFlowCookie = "0xe745ecf105"

	// nftablesUDNServicePreroutingChain is a base chain registered into the prerouting hook,
	// and it contains one rule that jumps to nftablesUDNServiceMarkChain.
	// Traffic from the default network's management interface is bypassed
	// to prevent enabling the default network access to the local node's UDN NodePort.
	nftablesUDNServicePreroutingChain = "udn-service-prerouting"

	// nftablesUDNServiceOutputChain is a base chain registered into the output hook
	// it contains one rule that jumps to nftablesUDNServiceMarkChain
	nftablesUDNServiceOutputChain = "udn-service-output"

	// nftablesUDNServiceMarkChain is a regular chain trying to match the incoming traffic
	// against the following UDN service verdict maps: nftablesUDNMarkNodePortsMap,
	// nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map
	nftablesUDNServiceMarkChain = "udn-service-mark"

	// nftablesUDNBGPOutputChain is a base chain used for blocking the local processes
	// from accessing any of the advertised UDN networks
	nftablesUDNBGPOutputChain = "udn-bgp-drop"

	// nftablesAdvertisedUDNsSetV[4|6] is a set containing advertised UDN subnets
	nftablesAdvertisedUDNsSetV4 = "advertised-udn-subnets-v4"
	nftablesAdvertisedUDNsSetV6 = "advertised-udn-subnets-v6"

	// nftablesUDNMarkNodePortsMap is a verdict maps containing
	// localNodeIP / protocol / port keys indicating traffic that
	// should be marked with a UDN specific value, which is used to direct the traffic
	// to the appropriate network.
	nftablesUDNMarkNodePortsMap = "udn-mark-nodeports"

	// nftablesUDNMarkExternalIPsV4Map and nftablesUDNMarkExternalIPsV6Map are verdict
	// maps containing loadBalancerIP / protocol / port keys indicating traffic that
	// should be marked with a UDN specific value, which is used to direct the traffic
	// to the appropriate network.
	nftablesUDNMarkExternalIPsV4Map = "udn-mark-external-ips-v4"
	nftablesUDNMarkExternalIPsV6Map = "udn-mark-external-ips-v6"
)

// configureUDNServicesNFTables configures the nftables chains, rules, and verdict maps
// that are used to set packet marks on externally exposed UDN services
func configureUDNServicesNFTables() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServiceMarkChain,
		Comment: knftables.PtrTo("UDN services packet mark"),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServiceMarkChain})

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServicePreroutingChain,
		Comment: knftables.PtrTo("UDN services packet mark - Prerouting"),

		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PreroutingHook),
		Priority: knftables.PtrTo(knftables.ManglePriority),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServicePreroutingChain})

	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServicePreroutingChain,
		Rule: knftables.Concat(
			"iifname", "!=", types.K8sMgmtIntfName,
			"jump", nftablesUDNServiceMarkChain,
		),
	})

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServiceOutputChain,
		Comment: knftables.PtrTo("UDN services packet mark - Output"),

		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.ManglePriority),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServiceOutputChain})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceOutputChain,
		Rule: knftables.Concat(
			"jump", nftablesUDNServiceMarkChain,
		),
	})

	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkNodePortsMap,
		Comment: knftables.PtrTo("UDN services NodePorts mark"),
		Type:    "inet_proto . inet_service : verdict",
	})
	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkExternalIPsV4Map,
		Comment: knftables.PtrTo("UDN services External IPs mark (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service : verdict",
	})
	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkExternalIPsV6Map,
		Comment: knftables.PtrTo("UDN services External IPs mark (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service : verdict",
	})

	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"fib daddr type local meta l4proto . th dport vmap", "@", nftablesUDNMarkNodePortsMap,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"ip daddr . meta l4proto . th dport vmap", "@", nftablesUDNMarkExternalIPsV4Map,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"ip6 daddr . meta l4proto . th dport vmap", "@", nftablesUDNMarkExternalIPsV6Map,
		),
	})

	return nft.Run(context.TODO(), tx)
}

// nodePortWatcherIptables manages iptables rules for shared gateway
// to ensure that services using NodePorts are accessible.
type nodePortWatcherIptables struct {
	networkManager networkmanager.Interface
}

func newNodePortWatcherIptables(networkManager networkmanager.Interface) *nodePortWatcherIptables {
	return &nodePortWatcherIptables{
		networkManager: networkManager,
	}
}

// nodePortWatcher manages OpenFlow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	dpuMode       bool
	gatewayIPv4   string
	gatewayIPv6   string
	gatewayIPLock sync.Mutex
	ofportPhys    string
	gwBridge      *bridgeconfig.BridgeConfiguration
	// Map of service name to programmed iptables/OF rules
	serviceInfo     map[ktypes.NamespacedName]*serviceConfig
	serviceInfoLock sync.Mutex
	ofm             *openflowManager
	nodeIPManager   *addressManager
	networkManager  networkmanager.Interface
	watchFactory    factory.NodeWatchFactory
}

type serviceConfig struct {
	// Contains the current service
	service *corev1.Service
	// hasLocalHostNetworkEp will be true for a service if it has at least one endpoint which is "hostnetworked&local-to-this-node".
	hasLocalHostNetworkEp bool
	// localEndpoints stores all the local non-host-networked endpoints for this service
	localEndpoints util.PortToLBEndpoints
}

type cidrAndFlags struct {
	ipNet             *net.IPNet
	flags             int
	preferredLifetime int
	validLifetime     int
}

func (npw *nodePortWatcher) updateGatewayIPs() {
	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(npw.gwBridge.GetIPs())

	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	npw.gatewayIPv4 = gatewayIPv4
	npw.gatewayIPv6 = gatewayIPv6
}

// updateServiceFlowCache handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (nodeport, external, ingress). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
func (npw *nodePortWatcher) updateServiceFlowCache(service *corev1.Service, netInfo util.NetInfo, add,
	hasLocalHostNetworkEp bool, localEndpoints util.PortToLBEndpoints) error {
	if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink && npw.ofportPhys == "" {
		// if LGW mode and no uplink gateway bridge, ingress traffic enters host from node physical interface instead of the breth0. Skip adding these service flows to br-ex.
		return nil
	}

	var netConfig *bridgeconfig.BridgeUDNConfiguration
	var actions string

	if add {
		netConfig = npw.ofm.getActiveNetwork(netInfo)
		if netConfig == nil {
			return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
		}
		actions = fmt.Sprintf("output:%s", netConfig.OfPortPatch)
	}

	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	var cookie, key string
	var err error
	var errors []error

	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)

	// cookie is only used for debugging purpose. so it is not fatal error if cookie is failed to be generated.
	for _, svcPort := range service.Spec.Ports {
		protocol := strings.ToLower(string(svcPort.Protocol))
		if svcPort.NodePort > 0 {
			flowProtocols := []string{}
			if config.IPv4Mode {
				flowProtocols = append(flowProtocols, protocol)
			}
			if config.IPv6Mode {
				flowProtocols = append(flowProtocols, protocol+"6")
			}
			for _, flowProtocol := range flowProtocols {
				key = strings.Join([]string{"NodePort", service.Namespace, service.Name, flowProtocol, fmt.Sprintf("%d", svcPort.NodePort)}, "_")
				// Delete if needed and skip to next protocol
				if !add {
					npw.ofm.deleteFlowsByKey(key)
					continue
				}
				cookie, err = svcToCookie(service.Namespace, service.Name, flowProtocol, svcPort.NodePort)
				if err != nil {
					klog.Warningf("Unable to generate cookie for nodePort svc: %s, %s, %s, %d, error: %v",
						service.Namespace, service.Name, flowProtocol, svcPort.Port, err)
					cookie = "0"
				}
				// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
				// set to Local, and the backend pod is HostNetworked. We need to add
				// Flows that will DNAT all traffic coming into nodeport to the nodeIP:Port and
				// ensure that the return traffic is UnDNATed to correct the nodeIP:Nodeport
				if isServiceTypeETPLocal && hasLocalHostNetworkEp {
					// Get the target port (this is important for named ports).
					svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
					lbe, err := localEndpoints.GetLBEndpoints(svcPortKey)
					if err != nil {
						errors = append(errors, err)
						continue
					}
					// The return traffic, matches on the flowProtocol + targetPort. That's different from cookie which
					// uses svcPort.NodePort.
					targetPort := lbe.Port
					targetPortCookie, err := svcToCookie(service.Namespace, service.Name, flowProtocol, targetPort)
					if err != nil {
						klog.Warningf("Unable to generate target port cookie for svc: %s, %s, %d, error: %v",
							service.Namespace, service.Name, targetPort, err)
						targetPortCookie = "0"
					}

					// case1 (see function description for details)
					var nodeportFlows []string
					klog.V(5).Infof("Adding flows on breth0 for Nodeport Service %s with targetPort %d in "+
						"Namespace: %s since ExternalTrafficPolicy=local", service.Name, targetPort, service.Namespace)
					// table 0, This rule matches on all traffic with dst port == NodePort, DNAT's the nodePort to the svc targetPort
					// If IPv6 make sure to choose the IPv6 node address for rule, otherwise send to the IPv4 node address.
					gatewayAddress := npw.gatewayIPv4
					if strings.Contains(flowProtocol, "6") {
						gatewayAddress = fmt.Sprintf("[%s]", npw.gatewayIPv6)
					}
					// This is just an additional safety check - we should never get into this situation.
					// If gatewayAddress is empty, we cannot install the following flow and we will continue with the
					// next flowProtocol.
					if gatewayAddress == "" || gatewayAddress == "[]" {
						err := fmt.Errorf("gateway IP not available for protocol %s", flowProtocol)
						errors = append(errors, err)
						continue
					}

					nodeportFlows = append(nodeportFlows,
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, "+
							"actions=ct(commit,zone=%d,nat(dst=%s:%d),table=6)",
							cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort,
							config.Default.HostNodePortConntrackZone, gatewayAddress, targetPort))

					nodeportFlows = append(nodeportFlows,
						// table 6, Sends the packet to the host. Note that the constant etp svc cookie is used since this flow would be
						// same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
							etpSvcOpenFlowCookie),
						// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						// Use targetPortCookie, as the flow will be the same if the target port + protocol are the same.
						fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%d, actions=ct(zone=%d nat,table=7)",
							targetPortCookie, flowProtocol, targetPort, config.Default.HostNodePortConntrackZone),
						// table 7, Sends the packet back out eth0 to the external client. Note that the constant etp svc
						// cookie is used since this would be same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=7, "+
							"actions=output:%s", etpSvcOpenFlowCookie, npw.ofportPhys))
					npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
				} else if config.Gateway.Mode == config.GatewayModeShared {
					// case2 (see function description for details)
					npw.ofm.updateFlowCacheEntry(key, []string{
						// table=0, matches on service traffic towards nodePort and sends it to OVN pipeline
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, "+
							"actions=%s",
							cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, actions),
						// table=0, matches on return traffic from service nodePort and sends it out to primary node interface (br-ex)
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, dl_src=%s, %s, tp_src=%d, "+
							"actions=output:%s",
							cookie, netConfig.OfPortPatch, npw.ofm.getDefaultBridgeMAC(), flowProtocol, svcPort.NodePort, npw.ofportPhys)})
				}
			}
		}

		// Flows for cloud load balancers on Azure/GCP
		// Established traffic is handled by default conntrack rules
		// NodePort/Ingress access in the OVS bridge will only ever come from outside of the host
		ingParsedIPs := make([]string, 0, len(service.Status.LoadBalancer.Ingress))
		for _, ing := range service.Status.LoadBalancer.Ingress {
			if len(ing.IP) > 0 {
				ip := utilnet.ParseIPSloppy(ing.IP)
				if ip == nil {
					errors = append(errors, fmt.Errorf("failed to parse Ingress IP: %q", ing.IP))
				} else {
					ingParsedIPs = append(ingParsedIPs, ip.String())
				}
			}
		}

		// flows for externalIPs
		extParsedIPs := make([]string, 0, len(service.Spec.ExternalIPs))
		for _, externalIP := range service.Spec.ExternalIPs {
			ip := utilnet.ParseIPSloppy(externalIP)
			if ip == nil {
				errors = append(errors, fmt.Errorf("failed to parse External IP: %q", externalIP))
			} else {
				extParsedIPs = append(extParsedIPs, ip.String())
			}
		}
		var ofPorts []string
		// don't get the ports unless we need to as it is a costly operation
		if (len(extParsedIPs) > 0 || len(ingParsedIPs) > 0) && add {
			ofPorts, err = util.GetOpenFlowPorts(npw.gwBridge.GetBridgeName(), false)
			if err != nil {
				// in the odd case that getting all ports from the bridge should not work,
				// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
				klog.Warningf("Unable to get port list from bridge. Using OvsLocalPort as output only: error: %v",
					err)
			}
		}
		if err = npw.createLbAndExternalSvcFlows(service, netConfig, &svcPort, add, hasLocalHostNetworkEp,
			localEndpoints, protocol, actions, ingParsedIPs, "Ingress", ofPorts); err != nil {
			errors = append(errors, err)
		}

		if err = npw.createLbAndExternalSvcFlows(service, netConfig, &svcPort, add, hasLocalHostNetworkEp,
			localEndpoints, protocol, actions, extParsedIPs, "External", ofPorts); err != nil {
			errors = append(errors, err)
		}
	}

	// Add flows for default network services that are accessible from UDN networks
	if util.IsNetworkSegmentationSupportEnabled() {
		// The flow added below has a higher priority than the per UDN service isolation flow:
		//   priority=200, table=2, ip, ip_src=169.254.0.<UDN>, actions=drop
		// This ordering ensures that traffic to UDN allowed default services goes to the default patch port.

		if util.IsUDNEnabledService(ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}.String()) {
			key = strings.Join([]string{"UDNAllowedSVC", service.Namespace, service.Name}, "_")
			if !add {
				npw.ofm.deleteFlowsByKey(key)
				return utilerrors.Join(errors...)
			}

			defaultNetConfig := npw.ofm.defaultBridge.GetActiveNetworkBridgeConfigCopy(types.DefaultNetworkName)
			var flows []string
			clusterIPs := util.GetClusterIPs(service)
			outputActions := fmt.Sprintf("output:%s", defaultNetConfig.OfPortPatch)
			if config.Gateway.VLANID != 0 {
				outputActions = fmt.Sprintf("mod_vlan_vid:%d,%s", config.Gateway.VLANID, outputActions)
			}

			for _, clusterIP := range clusterIPs {
				ipPrefix := protoPrefixV4
				if utilnet.IsIPv6String(clusterIP) {
					ipPrefix = protoPrefixV6
				}
				// table 2, user-defined network host -> OVN towards default cluster network services
				// sample flow: cookie=0xdeff105, duration=2319.685s, table=2, n_packets=496, n_bytes=67111, priority=300,
				//              ip,nw_dst=10.96.0.1 actions=mod_dl_dst:02:42:ac:12:00:03,output:"patch-breth0_ov"
				// This flow is used for UDNs and advertised UDNs to be able to reach kapi and dns services alone on default network
				flows = append(flows, fmt.Sprintf("cookie=%s, priority=300, table=2, %s, %s_dst=%s, "+
					"actions=set_field:%s->eth_dst,%s",
					nodetypes.DefaultOpenFlowCookie, ipPrefix, ipPrefix, clusterIP,
					npw.ofm.getDefaultBridgeMAC().String(), outputActions))

				if util.IsRouteAdvertisementsEnabled() {
					// if the network is advertised, then for the reply from kapi and dns services to go back
					// into the UDN's VRF we need flows that statically send this to the local port
					// sample flow: cookie=0xdeff105, duration=264.196s, table=0, n_packets=0, n_bytes=0, priority=490,ip,
					//              in_port="patch-breth0_ov",nw_src=10.96.0.10,actions=ct(table=3,zone=64001,nat)
					// this flow is meant to match all advertised UDNs and then the ip rules on the host will take
					// this packet into the corresponding UDNs
					// NOTE: We chose priority 490 to differentiate this flow from the flow at priority 500 added for the
					// non-advertised UDNs reponse for debugging purposes:
					// sample flow for non-advertised UDNs: cookie=0xdeff105, duration=684.087s, table=0, n_packets=0, n_bytes=0,
					//				idle_age=684, priority=500,ip,in_port=2,nw_src=10.96.0.0/16,nw_dst=169.254.0.0/17 actions=ct(table=3,zone=64001,nat)
					flows = append(flows, fmt.Sprintf("cookie=%s, priority=490, in_port=%s, %s, %s_src=%s,actions=ct(zone=%d,nat,table=3)",
						nodetypes.DefaultOpenFlowCookie, defaultNetConfig.OfPortPatch, ipPrefix, ipPrefix, clusterIP, config.Default.HostMasqConntrackZone))
				}
			}
			npw.ofm.updateFlowCacheEntry(key, flows)
		}
	}
	return utilerrors.Join(errors...)
}

// createLbAndExternalSvcFlows handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (externalIP and LoadBalancer types). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
// `protocol` is TCP/UDP/SCTP as set in the svc.Port
// `actions`: "send to patchport"
// `externalIPOrLBIngressIP` is either externalIP.IP or LB.status.ingress.IP
// `ipType` is either "External" or "Ingress"
func (npw *nodePortWatcher) createLbAndExternalSvcFlows(service *corev1.Service, netConfig *bridgeconfig.BridgeUDNConfiguration,
	svcPort *corev1.ServicePort, add bool, hasLocalHostNetworkEp bool, localEndpoints util.PortToLBEndpoints,
	protocol string, actions string, externalIPOrLBIngressIPs []string, ipType string, ofPorts []string) error {
	var errors []error

	for _, externalIPOrLBIngressIP := range externalIPOrLBIngressIPs {
		// each path has per IP generates about 4-5 flows. So we preallocate a slice with capacity.
		externalIPFlows := make([]string, 0, 5)

		// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
		// that dl_src is included in match criteria!

		flowProtocol := protocol
		nwDst := "nw_dst"
		nwSrc := "nw_src"
		if utilnet.IsIPv6String(externalIPOrLBIngressIP) {
			flowProtocol = protocol + "6"
			nwDst = "ipv6_dst"
			nwSrc = "ipv6_src"
		}
		cookie, err := svcToCookie(service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port)
		if err != nil {
			klog.Warningf("Unable to generate cookie for %s svc: %s, %s, %s, %d, error: %v",
				ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port, err)
			cookie = "0"
		}
		key := strings.Join([]string{ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, fmt.Sprintf("%d", svcPort.Port)}, "_")
		// Delete if needed and skip to next protocol
		if !add {
			npw.ofm.deleteFlowsByKey(key)
			continue
		}
		// add the ARP bypass flow regardless of service type or gateway modes since its applicable in all scenarios.
		arpFlow := npw.generateARPBypassFlow(ofPorts, netConfig.OfPortPatch, externalIPOrLBIngressIP, cookie)
		externalIPFlows = append(externalIPFlows, arpFlow)
		// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
		// set to Local, and the backend pod is HostNetworked. We need to add
		// Flows that will DNAT all external traffic destined for the lb/externalIP service
		// to the nodeIP / nodeIP:port of the host networked backend.
		// And then ensure that return traffic is UnDNATed correctly back
		// to the ingress / external IP
		isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
		if isServiceTypeETPLocal && hasLocalHostNetworkEp {
			// Get the target port (this is important for named ports).
			svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
			lbe, err := localEndpoints.GetLBEndpoints(svcPortKey)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			// cookie uses externalIPOrLBIngressIP and thus generates different cookies for each of the IP addresses.
			// The return traffic, however, matches on the flowProtocol + src port only. Therefore, generate a stable
			// cookie regardless of IP address and use this for the return flows.
			targetPort := lbe.Port
			targetPortCookie, err := svcToCookie(service.Namespace, service.Name, flowProtocol, targetPort)
			if err != nil {
				klog.Warningf("Unable to generate target port cookie for %s svc: %s, %s, %d, error: %v",
					ipType, service.Namespace, service.Name, targetPort, err)
				targetPortCookie = "0"
			}

			// case1 (see function description for details)
			klog.V(5).Infof("Adding flows on breth0 for %s Service %s in Namespace: %s since ExternalTrafficPolicy=local", ipType, service.Name, service.Namespace)
			// table 0, This rule matches on all traffic with dst ip == LoadbalancerIP / externalIP, DNAT's the nodePort to the svc targetPort
			// If IPv6 make sure to choose the IPv6 node address for rule, otherwise send to the IPv4 node address.
			gatewayAddress := npw.gatewayIPv4
			if strings.Contains(flowProtocol, "6") {
				gatewayAddress = fmt.Sprintf("[%s]", npw.gatewayIPv6)
			}
			// This is just an additional safety check - we should never get into this situation.
			// If gatewayAddress is empty, we cannot install the following flow and we will continue with the
			// next flowProtocol.
			if gatewayAddress == "" || gatewayAddress == "[]" {
				err := fmt.Errorf("gateway IP not available for protocol %s", flowProtocol)
				errors = append(errors, err)
				continue
			}

			externalIPFlows = append(externalIPFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
					"actions=ct(commit,zone=%d,nat(dst=%s:%d),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port,
					config.Default.HostNodePortConntrackZone, gatewayAddress, targetPort))

			externalIPFlows = append(externalIPFlows,
				// table 6, Sends the packet to Host. Note that the constant etp svc cookie is used since this flow would be
				// same for all such services.
				fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
					etpSvcOpenFlowCookie),
				// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs.
				// Use targetPortCookie, as the flow will be the same for each ExternalIP / LB status IP.
				fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%d, actions=ct(zone=%d nat,table=7)",
					targetPortCookie, flowProtocol, targetPort, config.Default.HostNodePortConntrackZone),
				// table 7, Sends the reply packet back out eth0 to the external client. Note that the constant etp svc
				// cookie is used since this would be same for all such services.
				fmt.Sprintf("cookie=%s, priority=110, table=7, actions=output:%s",
					etpSvcOpenFlowCookie, npw.ofportPhys))
		} else if config.Gateway.Mode == config.GatewayModeShared {
			// add the ICMP Fragmentation flow for shared gateway mode.
			icmpFlow := nodeutil.GenerateICMPFragmentationFlow(externalIPOrLBIngressIP, netConfig.OfPortPatch, npw.ofportPhys, cookie, 110)
			externalIPFlows = append(externalIPFlows, icmpFlow)
			// case2 (see function description for details)
			externalIPFlows = append(externalIPFlows,
				// table=0, matches on service traffic towards externalIP or LB ingress and sends it to OVN pipeline
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
					"actions=%s",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, actions),
				// table=0, matches on return traffic from service externalIP or LB ingress and sends it out to primary node interface (br-ex)
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, dl_src=%s, %s, %s=%s, tp_src=%d, "+
					"actions=output:%s",
					cookie, netConfig.OfPortPatch, npw.ofm.getDefaultBridgeMAC(), flowProtocol, nwSrc, externalIPOrLBIngressIP, svcPort.Port, npw.ofportPhys))
		}
		npw.ofm.updateFlowCacheEntry(key, externalIPFlows)
	}

	return utilerrors.Join(errors...)
}

// generate ARP/NS bypass flow which will send the ARP/NS request everywhere *but* to OVN
// OpenFlow will not do hairpin switching, so we can safely add the origin port to the list of ports, too
func (npw *nodePortWatcher) generateARPBypassFlow(ofPorts []string, ofPortPatch, ipAddr string, cookie string) string {
	addrResDst := "arp_tpa"
	addrResProto := "arp, arp_op=1"
	if utilnet.IsIPv6String(ipAddr) {
		addrResDst = "nd_target"
		addrResProto = "icmp6, icmp_type=135, icmp_code=0"
	}

	var arpFlow string
	var arpPortsFiltered []string
	if len(ofPorts) == 0 {
		// in the odd case that getting all ports from the bridge should not work,
		// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, nodetypes.OvsLocalPort)
	} else {
		// cover the case where breth0 has more than 3 ports, e.g. if an admin adds a 4th port
		// and the ExternalIP would be on that port
		// Use all ports except for ofPortPhys and the ofportPatch
		// Filtering ofPortPhys is for consistency / readability only, OpenFlow will not send
		// out the in_port normally (see man 7 ovs-actions)
		for _, port := range ofPorts {
			if port == ofPortPatch || port == npw.ofportPhys {
				continue
			}
			arpPortsFiltered = append(arpPortsFiltered, port)
		}

		// If vlan tagged traffic is received from physical interface, it has to be untagged before sending to access ports
		if config.Gateway.VLANID != 0 {
			match_vlan := fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
			arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s, %s=%s, "+
				"actions=strip_vlan,output:%s",
				cookie, npw.ofportPhys, match_vlan, addrResProto, addrResDst, ipAddr, strings.Join(arpPortsFiltered, ","))
		} else {
			arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
				"actions=output:%s",
				cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, strings.Join(arpPortsFiltered, ","))
		}
	}

	return arpFlow
}

// getAndDeleteServiceInfo returns the serviceConfig for a service and if it exists and then deletes the entry
func (npw *nodePortWatcher) getAndDeleteServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	delete(npw.serviceInfo, index)
	return out, exists
}

// getServiceInfo returns the serviceConfig for a service and if it exists
func (npw *nodePortWatcher) getServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	return out, exists
}

// getAndSetServiceInfo creates and sets the serviceConfig, returns if it existed and whatever was there
func (npw *nodePortWatcher) getAndSetServiceInfo(index ktypes.NamespacedName, service *corev1.Service, hasLocalHostNetworkEp bool, localEndpoints util.PortToLBEndpoints) (old *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	old, exists = npw.serviceInfo[index]
	var ptrCopy serviceConfig
	if exists {
		ptrCopy = *old
	}
	npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
	return &ptrCopy, exists
}

// addOrSetServiceInfo creates and sets the serviceConfig if it doesn't exist
func (npw *nodePortWatcher) addOrSetServiceInfo(index ktypes.NamespacedName, service *corev1.Service, hasLocalHostNetworkEp bool, localEndpoints util.PortToLBEndpoints) (exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if _, exists := npw.serviceInfo[index]; !exists {
		// Only set this if it doesn't exist
		npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
		return false
	}
	return true

}

// updateServiceInfo sets the serviceConfig for a service and returns the existing serviceConfig, if inputs are nil
// do not update those fields, if it does not exist return nil.
func (npw *nodePortWatcher) updateServiceInfo(index ktypes.NamespacedName, service *corev1.Service, hasLocalHostNetworkEp *bool, localEndpoints util.PortToLBEndpoints) (old *serviceConfig, exists bool) {

	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if old, exists = npw.serviceInfo[index]; !exists {
		klog.V(5).Infof("No serviceConfig found for service %s in namespace %s", index.Name, index.Namespace)
		return nil, exists
	}
	ptrCopy := *old
	if service != nil {
		npw.serviceInfo[index].service = service
	}

	if hasLocalHostNetworkEp != nil {
		npw.serviceInfo[index].hasLocalHostNetworkEp = *hasLocalHostNetworkEp
	}

	if localEndpoints != nil {
		npw.serviceInfo[index].localEndpoints = localEndpoints
	}

	return &ptrCopy, exists
}

// addServiceRules ensures the correct iptables rules and OpenFlow physical
// flows are programmed for a given service and endpoint configuration
func addServiceRules(service *corev1.Service, netInfo util.NetInfo, localEndpoints util.PortToLBEndpoints, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) error {
	// For dpu or Full mode
	var err error
	var errors []error
	var activeNetwork *bridgeconfig.BridgeUDNConfiguration
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, netInfo, true, svcHasLocalHostNetEndPnt, localEndpoints); err != nil {
			errors = append(errors, err)
		}
		npw.ofm.requestFlowSync()
		activeNetwork = npw.ofm.getActiveNetwork(netInfo)
		if activeNetwork == nil {
			return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
		}
	}

	if npw == nil || !npw.dpuMode {
		// add iptables/nftables rules only in full mode
		iptRules := getGatewayIPTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)
		if len(iptRules) > 0 {
			if err := insertIptRules(iptRules); err != nil {
				err = fmt.Errorf("failed to add iptables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
		nftElems := getGatewayNFTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)
		if netInfo.IsPrimaryNetwork() && activeNetwork != nil {
			nftElems = append(nftElems, getUDNNFTRules(service, activeNetwork)...)
		}
		if len(nftElems) > 0 {
			if err := nodenft.UpdateNFTElements(nftElems); err != nil {
				err = fmt.Errorf("failed to update nftables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
	}

	return utilerrors.Join(errors...)
}

// delServiceRules deletes all possible iptables rules and OpenFlow physical
// flows for a service
func delServiceRules(service *corev1.Service, localEndpoints util.PortToLBEndpoints, npw *nodePortWatcher) error {
	var err error
	var errors []error
	// full mode || dpu mode
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, nil, false, false, localEndpoints); err != nil {
			errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
		}
		npw.ofm.requestFlowSync()
	}

	if npw == nil || !npw.dpuMode {
		// Always try and delete all rules here in full mode & in host only mode. We don't touch iptables in dpu mode.
		// +--------------------------+-----------------------+-----------------------+--------------------------------+
		// | svcHasLocalHostNetEndPnt | ExternalTrafficPolicy | InternalTrafficPolicy |     Scenario for deletion      |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       |      deletes the MARK          |
		// |         false            |         cluster       |          local        |      rules for itp=local       |
		// |                          |                       |                       |       called from mangle       |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       |      deletes the REDIRECT      |
		// |         true             |         cluster       |          local        |      rules towards target      |
		// |                          |                       |                       |       port for itp=local       |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       | deletes the DNAT rules for     |
		// |         false            |          local        |          cluster      |    non-local-host-net          |
		// |                          |                       |                       | eps towards masqueradeIP +     |
		// |                          |                       |                       | DNAT rules towards clusterIP   |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       |    deletes the DNAT rules      |
		// |       false||true        |          cluster      |          cluster      |     towards clusterIP          |
		// |                          |                       |                       |       for the default case     |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       |      deletes all the rules     |
		// |       false||true        |          local        |          local        |   for etp=local + itp=local    |
		// |                          |                       |                       |   + default dnat towards CIP   |
		// +--------------------------+-----------------------+-----------------------+--------------------------------+

		iptRules := getGatewayIPTRules(service, localEndpoints, true)
		iptRules = append(iptRules, getGatewayIPTRules(service, localEndpoints, false)...)
		if len(iptRules) > 0 {
			if err := nodeipt.DelRules(iptRules); err != nil {
				err := fmt.Errorf("failed to delete iptables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
		nftElems := getGatewayNFTRules(service, localEndpoints, true)
		nftElems = append(nftElems, getGatewayNFTRules(service, localEndpoints, false)...)
		if len(nftElems) > 0 {
			if err := nodenft.DeleteNFTElements(nftElems); err != nil {
				err = fmt.Errorf("failed to delete nftables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}

		if util.IsNetworkSegmentationSupportEnabled() {
			// NOTE: The code below is not using nodenft.DeleteNFTElements because it first adds elements
			// before removing them, which fails for UDN NFT rules. These rules only have map keys,
			// not key-value pairs, making it impossible to add.
			// Attempt to delete the elements directly and handle the IsNotFound error.
			//
			// TODO: Switch to `nft destroy` when supported.
			nftElems = getUDNNFTRules(service, nil)
			if len(nftElems) > 0 {
				nft, err := nodenft.GetNFTablesHelper()
				if err != nil {
					return utilerrors.Join(append(errors, err)...)
				}

				tx := nft.NewTransaction()
				for _, elem := range nftElems {
					tx.Delete(elem)
				}

				if err := nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
					err = fmt.Errorf("failed to delete nftables rules for UDN service %s/%s: %v",
						service.Namespace, service.Name, err)
					errors = append(errors, err)
				}
			}
		}
	}

	return utilerrors.Join(errors...)
}

func serviceUpdateNotNeeded(old, new *corev1.Service) bool {
	return reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.ClusterIPs, old.Spec.ClusterIPs) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) &&
		reflect.DeepEqual(new.Spec.ExternalTrafficPolicy, old.Spec.ExternalTrafficPolicy) &&
		(new.Spec.InternalTrafficPolicy != nil && old.Spec.InternalTrafficPolicy != nil &&
			reflect.DeepEqual(*new.Spec.InternalTrafficPolicy, *old.Spec.InternalTrafficPolicy)) &&
		(new.Spec.AllocateLoadBalancerNodePorts != nil && old.Spec.AllocateLoadBalancerNodePorts != nil &&
			reflect.DeepEqual(*new.Spec.AllocateLoadBalancerNodePorts, *old.Spec.AllocateLoadBalancerNodePorts))
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *corev1.Service) error {
	var localEndpoints util.PortToLBEndpoints
	var hasLocalHostNetworkEp bool
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Adding service %s in namespace %s", service.Name, service.Namespace)
	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(service.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for service %s in namespace %s: %w", service.Name, service.Namespace, err)
	}

	if netInfo == nil {
		// network not active on our node
		return nil
	}

	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(service.Namespace, service.Name, netInfo.GetNetworkName())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during service add: %w",
				service.Namespace, service.Name, err)
		}
		klog.V(5).Infof("No endpointslice found for service %s in namespace %s during service Add",
			service.Name, service.Namespace)
		// No endpoint object exists yet so default to false
		hasLocalHostNetworkEp = false
	} else {
		nodeIPs, _ := npw.nodeIPManager.ListAddresses()
		localEndpoints = npw.GetLocalEligibleEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp = util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
	}
	// If something didn't already do it add correct Service rules
	if exists := npw.addOrSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints); !exists {
		klog.V(5).Infof("Service Add %s event in namespace %s came before endpoint event setting svcConfig",
			service.Name, service.Namespace)
		if err := addServiceRules(service, netInfo, localEndpoints, hasLocalHostNetworkEp, npw); err != nil {
			npw.getAndDeleteServiceInfo(name)
			return fmt.Errorf("AddService failed for nodePortWatcher: %w, trying delete: %v", err, delServiceRules(service, localEndpoints, npw))
		}
	} else {
		// Need to update flows here in case an attribute of the gateway has changed, such as MAC address
		klog.V(5).Infof("Updating already programmed rules for %s in namespace %s", service.Name, service.Namespace)
		if err = npw.updateServiceFlowCache(service, netInfo, true, hasLocalHostNetworkEp, localEndpoints); err != nil {
			return fmt.Errorf("failed to update flows for service %s/%s: %w", service.Namespace, service.Name, err)
		}
		npw.ofm.requestFlowSync()
	}
	return nil
}

func (npw *nodePortWatcher) UpdateService(old, new *corev1.Service) error {
	var err error
	var errors []error
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs, .Spec.Type, .Status.LoadBalancer.Ingress, "+
			".Spec.ExternalTrafficPolicy, .Spec.InternalTrafficPolicy", new.Name)
		return nil
	}
	// Update the service in svcConfig if we need to so that other handler
	// threads do the correct thing, leave hasLocalHostNetworkEp and localEndpoints alone in the cache
	svcConfig, exists := npw.updateServiceInfo(name, new, nil, nil)
	if !exists {
		klog.V(5).Infof("Service %s in namespace %s was deleted during service Update", old.Name, old.Namespace)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		// Delete old rules if needed, but don't delete svcConfig
		// so that we don't miss any endpoint update events here
		klog.V(5).Infof("Deleting old service rules for: %v", old)

		if err = delServiceRules(old, svcConfig.localEndpoints, npw); err != nil {
			errors = append(errors, err)
		}

		// Delete conntrack entries for UDP ports of the service if they have changed.
		// UDP is connectionless, so stale conntrack entries can cause packets to be routed to
		// old target ports even after service updates. Unlike TCP (which is stateful and manages
		// its own connection lifecycle), UDP conntrack entries must be explicitly cleaned up.
		// This behavior is extending the decision taken regarding endpoint deletion of UDP services.
		// For more details, see https://github.com/kubernetes/kubernetes/issues/108523#issuecomment-1074044415.
		if err = npw.deleteConntrackForUpdatedUDPServiceIPPorts(old, new); err != nil {
			errors = append(errors, fmt.Errorf("failed to delete conntrack entries for UDP ports of service %v: %v", name, err))
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		klog.V(5).Infof("Adding new service rules for: %v", new)

		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(new.Namespace)
		if err != nil {
			return fmt.Errorf("error getting active network for service %s in namespace %s: %w", new.Name, new.Namespace, err)
		}
		if netInfo == nil {
			// network not active on our node
			return utilerrors.Join(errors...)
		}

		if err = addServiceRules(new, netInfo, svcConfig.localEndpoints, svcConfig.hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
	}
	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcher: %v", err)
	}
	return nil

}

// deleteConntrackForUpdatedUDPServiceIPPorts deletes the conntrack entries for the updated UDP ports of the service.
// The conntrack entries are deleted for the old IP:port:targetPort combinations (IP:port:targetPort:nodePort for
// NodePort type services) if they are not present in the corresponding new combinations for External and LB IP,
// ClusterIP and Node IP.
func (npw *nodePortWatcher) deleteConntrackForUpdatedUDPServiceIPPorts(old, new *corev1.Service) error {
	// Skip if there are no service IPs and ports changed
	if apiequality.Semantic.DeepEqual(old.Spec.Ports, new.Spec.Ports) &&
		apiequality.Semantic.DeepEqual(old.Spec.ExternalIPs, new.Spec.ExternalIPs) &&
		apiequality.Semantic.DeepEqual(old.Spec.ClusterIP, new.Spec.ClusterIP) &&
		apiequality.Semantic.DeepEqual(old.Spec.ClusterIPs, new.Spec.ClusterIPs) &&
		apiequality.Semantic.DeepEqual(old.Status.LoadBalancer.Ingress, new.Status.LoadBalancer.Ingress) {
		return nil
	}

	klog.V(5).Infof("Service %s/%s IPs/ports changed, evaluating UDP conntrack cleanup", old.Namespace, old.Name)

	// Get the new IP:port:targetPort combinations for External and LB IP and ClusterIP. Get the new
	// Node IP:port:targetPort:nodePort combinations for Node IP. Delete the conntrack entries
	// for the old combinations that are not present in the corresponding new combinations.
	var deletedCount uint
	var errs []error

	// External and LB IPs
	newExternalIPs := util.GetExternalAndLBIPs(new)
	oldExternalIPs := util.GetExternalAndLBIPs(old)
	extIPsDeletedCount, extIPsErrs := deleteStaleUDPConntrackForUDPPortsAndIPs(new.Spec.Ports, newExternalIPs, old.Spec.Ports, oldExternalIPs, old.Namespace, old.Name, false)

	// Cluster IPs
	newClusterIPs := util.GetClusterIPs(new)
	oldClusterIPs := util.GetClusterIPs(old)
	clusterIPsDeletedCount, clusterIPsErrs := deleteStaleUDPConntrackForUDPPortsAndIPs(new.Spec.Ports, newClusterIPs, old.Spec.Ports, oldClusterIPs, old.Namespace, old.Name, false)

	// Node IPs
	var nodeIPs []net.IP
	if util.ServiceTypeHasNodePort(new) || util.ServiceTypeHasNodePort(old) {
		nodeIPs, _ = npw.nodeIPManager.ListAddresses()
	}
	var newNodeIPs []net.IP
	if util.ServiceTypeHasNodePort(new) {
		newNodeIPs = nodeIPs
	}
	var oldNodeIPs []net.IP
	if util.ServiceTypeHasNodePort(old) {
		oldNodeIPs = nodeIPs
	}
	nodePortDeletedCount, nodePortErrs := deleteStaleUDPConntrackForUDPPortsAndIPs(new.Spec.Ports, newNodeIPs, old.Spec.Ports, oldNodeIPs, old.Namespace, old.Name, true)

	deletedCount = extIPsDeletedCount + clusterIPsDeletedCount + nodePortDeletedCount
	errs = append(errs, extIPsErrs...)
	errs = append(errs, clusterIPsErrs...)
	errs = append(errs, nodePortErrs...)

	if deletedCount > 0 {
		klog.Infof("Deleted %d UDP conntrack entries for service %s/%s", deletedCount, old.Namespace, old.Name)
	}

	return utilerrors.Join(errs...)
}

// ipStringer is a constraint for types that can be converted to string IP representations
type ipStringer interface {
	~string | net.IP
}

// joinIPServicePort returns the IP:port:targetPort:nodePort combination of a service port for
// a NodePort type service, otherwise returns IP:port:targetPort combination of the service port.
func joinIPServicePort(ip string, servicePort corev1.ServicePort, isNodePort bool) string {
	joinString := fmt.Sprintf("%s:%d:%s", ip, servicePort.Port, servicePort.TargetPort.String())
	if isNodePort {
		joinString = fmt.Sprintf("%s:%d", joinString, servicePort.NodePort)
	}
	return joinString
}

// deleteStaleUDPConntrackForUDPPortsAndIPs deletes the conntrack entries for the updated UDP ports of the service.
// The conntrack entries are deleted for the old IP:port:targetPort combinations (IP:port:targetPort:nodePort for
// NodePort type service) that are not present in the corresponding new combinations.
func deleteStaleUDPConntrackForUDPPortsAndIPs[T ipStringer](
	newServicePorts []corev1.ServicePort,
	newIPs []T,
	oldServicePorts []corev1.ServicePort,
	oldIPs []T,
	namespace,
	name string,
	isNodePort bool,
) (uint, []error) {
	var errs []error
	var deletedCount uint

	// Get the new IP:port:targetPort combinations (IP:port:targetPort:nodePort for NodePort type service)
	newIPPortTargetPorts := sets.New[string]()
	for _, newPort := range newServicePorts {
		if newPort.Protocol != corev1.ProtocolUDP {
			continue
		}
		for _, newIP := range newIPs {
			newIPStr := fmt.Sprint(newIP)
			newIPPortTargetPorts.Insert(joinIPServicePort(newIPStr, newPort, isNodePort))
		}
	}

	// Delete the conntrack entries for the old IP:port:targetPort combinations (IP:port:targetPort:nodePort for
	// NodePort type service) that are not present in the corresponding new combinations
	for _, oldPort := range oldServicePorts {
		if oldPort.Protocol != corev1.ProtocolUDP {
			continue
		}
		port := oldPort.Port
		ipType := "svcVIP"
		portType := "svcPort"
		if isNodePort {
			port = oldPort.NodePort
			ipType = "nodeIP"
			portType = "nodePort"
		}
		for _, oldIP := range oldIPs {
			oldIPStr := fmt.Sprint(oldIP)
			if !newIPPortTargetPorts.Has(joinIPServicePort(oldIPStr, oldPort, isNodePort)) {
				deleted, err := util.DeleteConntrackServicePort(oldIPStr, port, oldPort.Protocol,
					netlink.ConntrackOrigDstIP, nil)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to delete conntrack entry for service %s/%s with %s %s, %s %d, protocol %s: %v",
						namespace, name, ipType, oldIPStr, portType, port, oldPort.Protocol, err))
				} else {
					deletedCount += deleted
				}
			}
		}
	}

	return deletedCount, errs
}

// deleteConntrackForServiceVIP deletes the conntrack entries for the provided svcVIP:svcPort by comparing them to ConntrackOrigDstIP:ConntrackOrigDstPort
func deleteConntrackForServiceVIP(svcVIPs []string, svcPorts []corev1.ServicePort, ns, name string) error {
	for _, svcVIP := range svcVIPs {
		for _, svcPort := range svcPorts {
			if _, err := util.DeleteConntrackServicePort(svcVIP, svcPort.Port, svcPort.Protocol,
				netlink.ConntrackOrigDstIP, nil); err != nil {
				return fmt.Errorf("failed to delete conntrack entry for service %s/%s with svcVIP %s, svcPort %d, protocol %s: %v",
					ns, name, svcVIP, svcPort.Port, svcPort.Protocol, err)
			}
		}
	}
	return nil
}

// deleteConntrackForService deletes the conntrack entries corresponding to the service VIPs of the provided service
func (npw *nodePortWatcher) deleteConntrackForService(service *corev1.Service) error {
	// remove conntrack entries for LB VIPs and External IPs
	externalIPs := util.GetExternalAndLBIPs(service)
	if err := deleteConntrackForServiceVIP(externalIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	if util.ServiceTypeHasNodePort(service) {
		// remove conntrack entries for NodePorts
		nodeIPs, _ := npw.nodeIPManager.ListAddresses()
		for _, nodeIP := range nodeIPs {
			for _, svcPort := range service.Spec.Ports {
				if _, err := util.DeleteConntrackServicePort(nodeIP.String(), svcPort.NodePort, svcPort.Protocol,
					netlink.ConntrackOrigDstIP, nil); err != nil {
					return fmt.Errorf("failed to delete conntrack entry for service %s/%s with nodeIP %s, nodePort %d, protocol %s: %v",
						service.Namespace, service.Name, nodeIP, svcPort.NodePort, svcPort.Protocol, err)
				}
			}
		}
	}
	// remove conntrack entries for ClusterIPs
	clusterIPs := util.GetClusterIPs(service)
	if err := deleteConntrackForServiceVIP(clusterIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	return nil
}

func (npw *nodePortWatcher) DeleteService(service *corev1.Service) error {
	var err error
	var errors []error
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Deleting service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	if svcConfig, exists := npw.getAndDeleteServiceInfo(name); exists {
		if err = delServiceRules(svcConfig.service, svcConfig.localEndpoints, npw); err != nil {
			errors = append(errors, err)
		}
	} else {
		klog.Warningf("Delete service: no service found in cache for endpoint %s in namespace %s", service.Name, service.Namespace)
	}
	// Remove all conntrack entries for the serviceVIPs of this service irrespective of protocol stack
	// since service deletion is considered as unplugging the network cable and hence graceful termination
	// is not guaranteed. See https://github.com/kubernetes/kubernetes/issues/108523#issuecomment-1074044415.
	if err = npw.deleteConntrackForService(service); err != nil {
		errors = append(errors, fmt.Errorf("failed to delete conntrack entry for service %v: %v", name, err))
	}

	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcher: %v", err)
	}
	return nil

}

func (npw *nodePortWatcher) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	var keepIPTRules []nodeipt.Rule
	var keepNFTSetElems, keepNFTMapElems []*knftables.Element
	for _, serviceInterface := range services {
		name := ktypes.NamespacedName{Namespace: serviceInterface.(*corev1.Service).Namespace, Name: serviceInterface.(*corev1.Service).Name}

		service, ok := serviceInterface.(*corev1.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// don't process headless service
		if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
			continue
		}

		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(service.Namespace)
		if err != nil {
			// During startup sync, avoid failing the entire processExisting loop for namespaces that
			// require a UDN but have no primary NAD yet (or it has been deleted). Those services will
			// be reconciled later via regular add/update events once the NAD exists.
			if util.IsInvalidPrimaryNetworkError(err) {
				continue
			}
			errors = append(errors, err)
			continue
		}
		if netInfo == nil {
			// network not active on our node
			continue
		}

		epSlices, err := npw.watchFactory.GetServiceEndpointSlices(service.Namespace, service.Name, netInfo.GetNetworkName())
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("error retrieving all endpointslices for service %s/%s during SyncServices: %w",
					service.Namespace, service.Name, err)
			}
			klog.V(5).Infof("No endpointslice found for service %s in namespace %s during sync", service.Name, service.Namespace)
			continue
		}
		nodeIPs, _ := npw.nodeIPManager.ListAddresses()
		localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
		npw.getAndSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints)

		// Delete OF rules for service if they exist
		if err = npw.updateServiceFlowCache(service, netInfo, false, hasLocalHostNetworkEp, localEndpoints); err != nil {
			errors = append(errors, err)
		}
		if err = npw.updateServiceFlowCache(service, netInfo, true, hasLocalHostNetworkEp, localEndpoints); err != nil {
			errors = append(errors, err)
		}
		// Add correct netfilter rules only for Full mode
		if !npw.dpuMode {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, localEndpoints, hasLocalHostNetworkEp)...)
			keepNFTSetElems = append(keepNFTSetElems, getGatewayNFTRules(service, localEndpoints, hasLocalHostNetworkEp)...)
			if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
				netConfig := npw.ofm.getActiveNetwork(netInfo)
				if netConfig == nil {
					return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
				}
				keepNFTMapElems = append(keepNFTMapElems, getUDNNFTRules(service, netConfig)...)
			}
		}
	}

	// sync OF rules once
	npw.ofm.requestFlowSync()
	// sync netfilter rules once only for Full mode
	if !npw.dpuMode {
		// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
		for _, chain := range []string{iptableITPChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
			if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
				errors = append(errors, err)
			}
		}
		if err = recreateIPTRules("mangle", iptableITPChain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}

		nftableManagementPortSets := []string{
			types.NFTMgmtPortNoSNATNodePorts,
			types.NFTMgmtPortNoSNATServicesV4,
			types.NFTMgmtPortNoSNATServicesV6,
		}
		for _, set := range nftableManagementPortSets {
			if err = recreateNFTSet(set, keepNFTSetElems); err != nil {
				errors = append(errors, err)
			}
		}
		if util.IsNetworkSegmentationSupportEnabled() {
			for _, nftMap := range []string{nftablesUDNMarkNodePortsMap, nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map} {
				if err = recreateNFTMap(nftMap, keepNFTMapElems); err != nil {
					errors = append(errors, err)
				}
			}
		}
	}
	return utilerrors.Join(errors...)
}

func (npw *nodePortWatcher) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var svc *corev1.Service

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(epSlice.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for endpointslice %s in namespace %s: %w", epSlice.Name, epSlice.Namespace, err)
	}
	if netInfo == nil {
		// network not active on our node
		return nil
	}

	if util.IsNetworkSegmentationSupportEnabled() && !util.IsEndpointSliceForNetwork(epSlice, netInfo) {
		return nil
	}

	svcNamespacedName, err := util.ServiceFromEndpointSlice(epSlice, netInfo.GetNetworkName())
	if err != nil || svcNamespacedName == nil {
		return err
	}

	svc, err = npw.watchFactory.GetService(svcNamespacedName.Namespace, svcNamespacedName.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving service %s/%s during endpointslice add: %w",
				svcNamespacedName.Namespace, svcNamespacedName.Name, err)
		}
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		klog.V(5).Infof("No service found for endpointslice %s in namespace %s during endpointslice add",
			epSlice.Name, epSlice.Namespace)
		return nil
	}

	if !util.ServiceTypeHasClusterIP(svc) || !util.IsClusterIPSet(svc) {
		return nil
	}

	klog.V(5).Infof("Adding endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	nodeIPs, _ := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(svc.Namespace, svc.Name, netInfo.GetNetworkName())
	if err != nil {
		// No need to continue adding the new endpoint slice, if we can't retrieve all slices for this service
		return fmt.Errorf("error retrieving endpointslices for service %s/%s during endpointslice add: %w", svc.Namespace, svc.Name, err)
	}
	localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)

	// Here we make sure the correct rules are programmed whenever an AddEndpointSlice event is
	// received, only alter flows if we need to, i.e if cache wasn't set or if it was and
	// hasLocalHostNetworkEp or localEndpoints state (for LB svc where NPs=0) changed, to prevent flow churn
	out, exists := npw.getServiceInfo(*svcNamespacedName)
	if !exists {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is creating rules", epSlice.Name, epSlice.Namespace)
		if err = addServiceRules(svc, netInfo, localEndpoints, hasLocalHostNetworkEp, npw); err != nil {
			return err
		}
		npw.addOrSetServiceInfo(*svcNamespacedName, svc, hasLocalHostNetworkEp, localEndpoints)
		return nil
	}

	if out.hasLocalHostNetworkEp != hasLocalHostNetworkEp ||
		(!util.LoadBalancerServiceHasNodePortAllocation(svc) && !reflect.DeepEqual(out.localEndpoints, localEndpoints)) {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is updating rules", epSlice.Name, epSlice.Namespace)
		if err = delServiceRules(svc, out.localEndpoints, npw); err != nil {
			errors = append(errors, err)
		}
		if err = addServiceRules(svc, netInfo, localEndpoints, hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		} else {
			npw.updateServiceInfo(*svcNamespacedName, svc, &hasLocalHostNetworkEp, localEndpoints)
		}
		return utilerrors.Join(errors...)
	}
	return nil

}

func (npw *nodePortWatcher) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var hasLocalHostNetworkEp = false

	networkName := types.DefaultNetworkName
	if util.IsNetworkSegmentationSupportEnabled() {
		if netName, ok := epSlice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation]; ok {
			networkName = netName
		}
	}

	klog.V(5).Infof("Deleting endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	// remove rules for endpoints and add back normal ones
	namespacedName, err := util.ServiceFromEndpointSlice(epSlice, networkName)
	if err != nil || namespacedName == nil {
		return err
	}
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(namespacedName.Namespace, namespacedName.Name, networkName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice delete on %s: %w",
				namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
		}
		// an endpoint slice that we retry to delete will be gone from the api server, so don't return here
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice delete on %s (did we previously fail to delete it?)",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name)
		epSlices = []*discovery.EndpointSlice{epSlice}
	}

	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice delete: %v",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
	}
	localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	if svcConfig, exists := npw.updateServiceInfo(*namespacedName, nil, &hasLocalHostNetworkEp, localEndpoints); exists {
		// Lock the cache mutex here so we don't miss a service delete during an endpoint delete
		// we have to do this because deleting and adding iptables rules is slow.
		npw.serviceInfoLock.Lock()
		defer npw.serviceInfoLock.Unlock()

		if err = delServiceRules(svcConfig.service, svcConfig.localEndpoints, npw); err != nil {
			errors = append(errors, err)
		}

		// Get network info after deleting old rules, before adding new ones.
		// This ensures old rules are cleaned up even if namespace/network is deleted,
		// and allows graceful handling of deletion race conditions.
		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(namespacedName.Namespace)
		if err != nil {
			// If the UDN was deleted or not processed yet, skip adding new service rules
			if util.IsInvalidPrimaryNetworkError(err) {
				klog.V(5).Infof("Skipping addServiceRules for %s/%s during endpoint slice delete: primary network unavailable: %v",
					namespacedName.Namespace, namespacedName.Name, err)
				return utilerrors.Join(errors...)
			}
			errors = append(errors, fmt.Errorf("error getting active network for service %s/%s: %w", namespacedName.Namespace, namespacedName.Name, err))
			return utilerrors.Join(errors...)
		}
		if netInfo == nil {
			// network not active on our node
			return utilerrors.Join(errors...)
		}

		if err = addServiceRules(svcConfig.service, netInfo, localEndpoints, hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
		return utilerrors.Join(errors...)
	}
	return nil
}

// GetLocalEligibleEndpointAddresses returns eligible endpoints that are local to the node.
// This method uses util.GetEndpointsForService, the same as the services Controller via buildServiceLBConfigs,
// meaning that the nodePortWatcher and the services Controller now use common logic to build their service endpoints.
func (npw *nodePortWatcher) GetLocalEligibleEndpointAddresses(endpointSlices []*discovery.EndpointSlice,
	service *corev1.Service) util.PortToLBEndpoints {
	s := sets.Set[string]{}
	s.Insert(npw.nodeIPManager.nodeName)
	_, portToNodeToLBEndpoints, err := util.GetEndpointsForService(endpointSlices, service, s, false, true)
	if err != nil {
		if service != nil {
			klog.Warningf("Failed to get local endpoints for service %s/%s on node %s: %v",
				service.Namespace, service.Name, npw.nodeIPManager.nodeName, err)
		} else {
			klog.Warningf("Failed to get local endpoints on node %s: %v", npw.nodeIPManager.nodeName, err)
		}
	}

	return portToNodeToLBEndpoints.GetNode(npw.nodeIPManager.nodeName)
}

func (npw *nodePortWatcher) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	// TODO (tssurya): refactor bits in this function to ensure add and delete endpoint slices are not called repeatedly
	// Context: Both add and delete endpointslice are calling delServiceRules followed by addServiceRules which makes double
	// the number of calls than needed for an update endpoint slice
	var err error
	var errors []error

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(newEpSlice.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for endpointslice %s in namespace %s: %w", newEpSlice.Name, newEpSlice.Namespace, err)
	}
	if netInfo == nil {
		// network not active on our node
		return nil
	}

	if util.IsNetworkSegmentationSupportEnabled() && !util.IsEndpointSliceForNetwork(newEpSlice, netInfo) {
		return nil
	}

	namespacedName, err := util.ServiceFromEndpointSlice(newEpSlice, netInfo.GetNetworkName())
	if err != nil || namespacedName == nil {
		return err
	}
	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice update: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	oldEndpointAddresses := util.GetEligibleEndpointAddressesFromSlices([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newEndpointAddresses := util.GetEligibleEndpointAddressesFromSlices([]*discovery.EndpointSlice{newEpSlice}, svc)
	if reflect.DeepEqual(oldEndpointAddresses, newEndpointAddresses) {
		return nil
	}

	klog.V(5).Infof("Updating endpointslice %s in namespace %s", oldEpSlice.Name, oldEpSlice.Namespace)

	var serviceInfo *serviceConfig
	var exists bool
	if serviceInfo, exists = npw.getServiceInfo(*namespacedName); !exists {
		// When a service is updated from externalName to nodeport type, it won't be
		// in nodePortWatcher cache (npw): in this case, have the new nodeport IPtable rules
		// installed.
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
	} else if len(newEndpointAddresses) == 0 {
		// With no endpoint addresses in new endpointslice, delete old endpoint rules
		// and add normal ones back
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
	}

	// Update rules and service cache if hasHostNetworkEndpoints status changed or localEndpoints changed
	nodeIPs, _ := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(newEpSlice.Namespace, namespacedName.Name, netInfo.GetNetworkName())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice update on %s: %w",
				namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
		}
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice update on %s: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	// Delete old endpoint slice and add new one when local endpoints have changed or the presence of local host-network
	// endpoints has changed. For this second comparison, check first between the old endpoint slice and all current
	// endpointslices for this service. This is a partial comparison, in case serviceInfo is not set. When it is set, compare
	// between /all/ old endpoint slices and all new ones.
	oldLocalEndpoints := npw.GetLocalEligibleEndpointAddresses([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newLocalEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEpOld := util.HasLocalHostNetworkEndpoints(oldLocalEndpoints, nodeIPs)
	hasLocalHostNetworkEpNew := util.HasLocalHostNetworkEndpoints(newLocalEndpoints, nodeIPs)

	localEndpointsHaveChanged := serviceInfo != nil && !reflect.DeepEqual(serviceInfo.localEndpoints, newLocalEndpoints)
	localHostNetworkEndpointsPresenceHasChanged := hasLocalHostNetworkEpOld != hasLocalHostNetworkEpNew ||
		serviceInfo != nil && serviceInfo.hasLocalHostNetworkEp != hasLocalHostNetworkEpNew

	if localEndpointsHaveChanged || localHostNetworkEndpointsPresenceHasChanged {
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
		return utilerrors.Join(errors...)
	}

	return utilerrors.Join(errors...)
}

func (npwipt *nodePortWatcherIptables) AddService(service *corev1.Service) error {
	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	netInfo, err := npwipt.networkManager.GetActiveNetworkForNamespace(service.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for service %s in namespace %s: %w", service.Name, service.Namespace, err)
	}
	if netInfo == nil {
		// network not active on our node
		return nil
	}

	if err := addServiceRules(service, netInfo, nil, false, nil); err != nil {
		return fmt.Errorf("AddService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) UpdateService(old, new *corev1.Service) error {
	var err error
	var errors []error
	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to "+
			"any of .Spec.Ports, .Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs,"+
			" .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		if err = delServiceRules(old, nil, nil); err != nil {
			errors = append(errors, err)
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		netInfo, err := npwipt.networkManager.GetActiveNetworkForNamespace(new.Namespace)
		if err != nil {
			return fmt.Errorf("error getting active network for service %s in namespace %s: %w", new.Name, new.Namespace, err)
		}
		if netInfo == nil {
			// network not active on our node
			return utilerrors.Join(errors...)
		}

		if err = addServiceRules(new, netInfo, nil, false, nil); err != nil {
			errors = append(errors, err)
		}
	}
	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcherIptables: %v", err)
	}
	return nil

}

func (npwipt *nodePortWatcherIptables) DeleteService(service *corev1.Service) error {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	if err := delServiceRules(service, nil, nil); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	keepIPTRules := []nodeipt.Rule{}
	keepNFTElems := []*knftables.Element{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*corev1.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// don't process headless service
		if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
			continue
		}
		netInfo, err := npwipt.networkManager.GetActiveNetworkForNamespace(service.GetNamespace())
		if err != nil {
			// During startup sync, avoid failing the entire processExisting loop for namespaces that
			// require a UDN but have no primary NAD yet (or it has been deleted). Those services will
			// be reconciled later via regular add/update events once the NAD exists.
			if util.IsInvalidPrimaryNetworkError(err) {
				continue
			}
			errors = append(errors, err)
			continue
		}
		if netInfo == nil {
			// network not on our node
			continue
		}
		// Add correct iptables rules.
		// TODO: ETP and ITP is not implemented for smart NIC mode.
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, nil, false)...)
		keepNFTElems = append(keepNFTElems, getGatewayNFTRules(service, nil, false)...)
	}

	// sync rules once
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}
	}

	nftableManagementPortSets := []string{
		types.NFTMgmtPortNoSNATNodePorts,
		types.NFTMgmtPortNoSNATServicesV4,
		types.NFTMgmtPortNoSNATServicesV6,
	}
	for _, set := range nftableManagementPortSets {
		if err = recreateNFTSet(set, keepNFTElems); err != nil {
			errors = append(errors, err)
		}
	}

	return utilerrors.Join(errors...)
}

func newGateway(
	nodeName string,
	subnets []*net.IPNet,
	gwNextHops []net.IP,
	gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet,
	nodeAnnotator kube.Annotator,
	mgmtPort managementport.Interface,
	kube kube.Interface,
	watchFactory factory.NodeWatchFactory,
	routeManager *routemanager.Controller,
	linkManager *linkmanager.Controller,
	networkManager networkmanager.Interface,
	gatewayMode config.GatewayMode,
) (*gateway, error) {
	klog.Info("Creating new gateway")
	gw := &gateway{
		nextHops: gwNextHops,
	}

	if gatewayMode == config.GatewayModeLocal {
		if err := initLocalGateway(subnets, mgmtPort); err != nil {
			return nil, fmt.Errorf("failed to initialize new local gateway, err: %w", err)
		}
	}

	advertised := util.IsPodNetworkAdvertisedAtNode(networkManager.GetNetwork(types.DefaultNetworkName), nodeName)
	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, gwNextHops, subnets, gwIPs, advertised, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if exGwBridge != nil {
		gw.readyFunc = func() (bool, error) {
			if !gwBridge.IsGatewayReady() {
				return false, nil
			}
			if !exGwBridge.IsGatewayReady() {
				return false, nil
			}
			return true, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			if !gwBridge.IsGatewayReady() {
				return false, nil
			}
			return true, nil
		}
	}

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Gateway Openflow Manager")
		err := gwBridge.ConfigureBridgePorts()
		if err != nil {
			return err
		}
		if exGwBridge != nil {
			err = exGwBridge.ConfigureBridgePorts()
			if err != nil {
				return err
			}
		}
		if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect && config.Gateway.Mode != config.GatewayModeDisabled {
			gw.bridgeEIPAddrManager = egressip.NewBridgeEIPAddrManager(nodeName, gwBridge.GetBridgeName(), linkManager, kube, watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())
			gwBridge.SetEIPMarkIPs(gw.bridgeEIPAddrManager.GetCache())
		}
		gw.nodeIPManager = newAddressManager(nodeName, kube, mgmtPort, watchFactory, gwBridge)

		if config.OvnKubeNode.Mode == types.NodeModeFull {
			// Delete stale masquerade resources if there are any. This is to make sure that there
			// are no Linux resources with IP from old masquerade subnet when masquerade subnet
			// gets changed as part of day2 operation.
			if err := deleteStaleMasqueradeResources(gwBridge.GetGatewayIface(), nodeName, watchFactory); err != nil {
				return fmt.Errorf("failed to remove stale masquerade resources: %w", err)
			}

			if err := setNodeMasqueradeIPOnExtBridge(gwBridge.GetGatewayIface()); err != nil {
				return fmt.Errorf("failed to set the node masquerade IP on the ext bridge %s: %v", gwBridge.GetGatewayIface(), err)
			}

			if err := addMasqueradeRoute(routeManager, gwBridge.GetGatewayIface(), nodeName, gwIPs, watchFactory); err != nil {
				return fmt.Errorf("failed to set the node masquerade route to OVN: %v", err)
			}

			// Masquerade config mostly done on node, update annotation
			if err := updateMasqueradeAnnotation(nodeName, kube); err != nil {
				return fmt.Errorf("failed to update masquerade subnet annotation on node: %s, error: %v", nodeName, err)
			}
		}

		gw.openflowManager, err = newGatewayOpenFlowManager(gwBridge, exGwBridge)
		if err != nil {
			return err
		}

		// resync flows on IP change
		gw.nodeIPManager.OnChanged = func() {
			klog.V(5).Info("Node addresses changed, re-syncing bridge flows")
			if err := gw.openflowManager.updateBridgeFlowCache(gw.nodeIPManager.ListAddresses()); err != nil {
				// very unlikely - somehow node has lost its IP address
				klog.Errorf("Failed to re-generate gateway flows after address change: %v", err)
			}
			if gw.nodePortWatcher != nil {
				npw, _ := gw.nodePortWatcher.(*nodePortWatcher)
				npw.updateGatewayIPs()
			}
			// Services create OpenFlow flows as well, need to update them all
			if gw.servicesRetryFramework != nil {
				if errs := gw.addAllServices(); len(errs) > 0 {
					err := utilerrors.Join(errs...)
					klog.Errorf("Failed to sync all services after node IP change: %v", err)
				}
			}
			gw.openflowManager.requestFlowSync()
		}

		if config.Gateway.NodeportEnable {
			klog.Info("Creating Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge, gw.openflowManager, gw.nodeIPManager, watchFactory, networkManager)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}

		if err := addHostMACBindings(gwBridge.GetGatewayIface()); err != nil {
			return fmt.Errorf("failed to add MAC bindings for service routing: %w", err)
		}

		return nil
	}
	gw.watchFactory = watchFactory.(*factory.WatchFactory)
	klog.Info("Gateway Creation Complete")
	return gw, nil
}

func newNodePortWatcher(
	gwBridge *bridgeconfig.BridgeConfiguration,
	ofm *openflowManager,
	nodeIPManager *addressManager,
	watchFactory factory.NodeWatchFactory,
	networkManager networkmanager.Interface,
) (*nodePortWatcher, error) {

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get",
		"interface", gwBridge.GetUplinkName(), "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwBridge.GetUplinkName(), stderr, err)
	}

	// In the shared gateway mode, the NodePort service is handled by the OpenFlow flows configured
	// on the OVS bridge in the host. These flows act only on the packets coming in from outside
	// of the node. If someone on the node is trying to access the NodePort service, those packets
	// will not be processed by the OpenFlow flows, so we need to add iptable rules that DNATs the
	// NodePortIP:NodePort to ClusterServiceIP:Port. We don't need to do this on DPU.
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		if config.Gateway.Mode == config.GatewayModeLocal {
			if err := initLocalGatewayIPTables(); err != nil {
				return nil, err
			}
		} else if config.Gateway.Mode == config.GatewayModeShared {
			if err := initSharedGatewayIPTables(); err != nil {
				return nil, err
			}
		}
		if util.IsNetworkSegmentationSupportEnabled() {
			if err := configureUDNServicesNFTables(); err != nil {
				return nil, fmt.Errorf("unable to configure UDN nftables: %w", err)
			}
		}
		if util.IsRouteAdvertisementsEnabled() {
			if err := configureAdvertisedUDNIsolationNFTables(); err != nil {
				return nil, fmt.Errorf("unable to configure UDN isolation nftables: %w", err)
			}
		}

		var subnets []*net.IPNet
		for _, subnet := range config.Default.ClusterSubnets {
			subnets = append(subnets, subnet.CIDR)
		}
		subnets = append(subnets, config.Kubernetes.ServiceCIDRs...)
		if config.Gateway.DisableForwarding {
			if err := initExternalBridgeServiceForwardingRules(subnets); err != nil {
				return nil, fmt.Errorf("failed to add accept rules in forwarding table for bridge %s: err %v", gwBridge.GetGatewayIface(), err)
			}
		} else {
			if err := delExternalBridgeServiceForwardingRules(subnets); err != nil {
				return nil, fmt.Errorf("failed to delete accept rules in forwarding table for bridge %s: err %v", gwBridge.GetGatewayIface(), err)
			}
		}
	}

	// used to tell addServiceRules which rules to add
	dpuMode := false
	if config.OvnKubeNode.Mode != types.NodeModeFull {
		dpuMode = true
	}

	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gwBridge.GetIPs())

	npw := &nodePortWatcher{
		dpuMode:        dpuMode,
		gatewayIPv4:    gatewayIPv4,
		gatewayIPv6:    gatewayIPv6,
		ofportPhys:     ofportPhys,
		gwBridge:       gwBridge,
		serviceInfo:    make(map[ktypes.NamespacedName]*serviceConfig),
		nodeIPManager:  nodeIPManager,
		ofm:            ofm,
		watchFactory:   watchFactory,
		networkManager: networkManager,
	}
	return npw, nil
}

func cleanupSharedGateway() error {
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// NicToBridge() may be created before-hand, only delete the patch port here
		stdout, stderr, err := util.RunOVSVsctl("--columns=name", "--no-heading", "find", "port",
			"external_ids:ovn-localnet-port!=_")
		if err != nil {
			return fmt.Errorf("failed to get ovn-localnet-port port stderr:%s (%v)", stderr, err)
		}
		ports := strings.Fields(strings.Trim(stdout, "\""))
		for _, port := range ports {
			_, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", strings.Trim(port, "\""))
			if err != nil {
				return fmt.Errorf("failed to delete port %s stderr:%s (%v)", port, stderr, err)
			}
		}

		// Get the OVS bridge name from ovn-bridge-mappings
		stdout, stderr, err = util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
			"external_ids:ovn-bridge-mappings")
		if err != nil {
			return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
		}

		// skip the existing mapping setting for the specified physicalNetworkName
		bridgeName := ""
		bridgeMappings := strings.Split(stdout, ",")
		for _, bridgeMapping := range bridgeMappings {
			m := strings.Split(bridgeMapping, ":")
			if network := m[0]; network == types.PhysicalNetworkName {
				bridgeName = m[1]
				break
			}
		}
		if len(bridgeName) == 0 {
			return nil
		}

		_, stderr, err = util.AddOFFlowWithSpecificAction(bridgeName, util.NormalAction)
		if err != nil {
			return fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", bridgeName, stderr, err)
		}
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		cleanupSharedGatewayIPTChains()
	}
	return nil
}

func svcToCookie(namespace string, name string, token string, port int32) (string, error) {
	id := fmt.Sprintf("%s%s%s%d", namespace, name, token, port)
	h := fnv.New64a()
	_, err := h.Write([]byte(id))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("0x%x", h.Sum64()), nil
}

func addMasqueradeRoute(routeManager *routemanager.Controller, netIfaceName, nodeName string, ifAddrs []*net.IPNet, watchFactory factory.NodeWatchFactory) error {
	var ipv4, ipv6 net.IP
	findIPs := func(ips []net.IP) error {
		var err error
		if config.IPv4Mode && ipv4 == nil {
			ipv4, err = util.MatchFirstIPFamily(false, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		if config.IPv6Mode && ipv6 == nil {
			ipv6, err = util.MatchFirstIPFamily(true, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		return nil
	}

	// Try first with the node status IPs and fallback to the interface IPs. The
	// fallback is a workaround for instances where the node status might not
	// have the minimum set of IPs we need (for example, when ovnkube is
	// restarted after enabling an IP family without actually restarting kubelet
	// with a new configuration including an IP address for that family). Node
	// status IPs are preferred though because a user might add arbitrary IP
	// addresses to the interface that we don't really want to use and might
	// cause problems.

	var nodeIPs []net.IP
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return err
	}
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type != corev1.NodeInternalIP {
			continue
		}
		nodeIP := utilnet.ParseIPSloppy(nodeAddr.Address)
		nodeIPs = append(nodeIPs, nodeIP)
	}

	err = findIPs(nodeIPs)
	if err != nil {
		klog.Warningf("Unable to add OVN masquerade route to host using source node status IPs: %v", err)
		// fallback to the interface IPs
		var ifIPs []net.IP
		for _, ifAddr := range ifAddrs {
			ifIPs = append(ifIPs, ifAddr.IP)
		}
		err := findIPs(ifIPs)
		if err != nil {
			return fmt.Errorf("unable to add OVN masquerade route to host using interface IPs: %v", err)
		}
	}

	netIfaceLink, err := util.LinkSetUp(netIfaceName)
	if err != nil {
		return fmt.Errorf("unable to find shared gw bridge interface: %s", netIfaceName)
	}
	mtu := 0
	if ipv4 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv4)
		err = routeManager.Add(netlink.Route{LinkIndex: netIfaceLink.Attrs().Index, Dst: masqIPNet, MTU: mtu, Src: ipv4})
		if err != nil {
			return fmt.Errorf("failed to add OVN Masquerade route: %w", err)
		}
	}

	if ipv6 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/128", config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv6)
		err = routeManager.Add(netlink.Route{LinkIndex: netIfaceLink.Attrs().Index, Dst: masqIPNet, MTU: mtu, Src: ipv6})
		if err != nil {
			return fmt.Errorf("failed to add OVN Masquerade route: %w", err)
		}
	}
	return nil
}

func setNodeMasqueradeIPOnExtBridge(extBridgeName string) error {
	extBridge, err := util.LinkSetUp(extBridgeName)
	if err != nil {
		return err
	}

	var bridgeCIDRs []cidrAndFlags
	if config.IPv4Mode {
		_, masqIPNet, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
		masqIPNet.IP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: 0})
	}

	if config.IPv6Mode {
		_, masqIPNet, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)
		masqIPNet.IP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP
		// Deprecate the IPv6 host masquerade IP address to ensure its not used in source address selection except
		// if a route explicitly sets its src IP as this masquerade IP. See RFC 3484 for more details for linux src address selection.
		// Currently, we set a route with destination as the service CIDR with source IP as the host masquerade IP.
		// Also, ideally we would only set the preferredLifetime to 0, but because this is the default value of this type, the netlink lib
		// will only propagate preferred lifetime to netlink if either preferred lifetime or valid lifetime is set greater than 0.
		// Set valid lifetime to max will achieve our goal of setting preferred lifetime 0.
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: unix.IFA_F_NODAD, preferredLifetime: 0,
			validLifetime: math.MaxUint32})
	}

	for _, bridgeCIDR := range bridgeCIDRs {
		if exists, err := util.LinkAddrExist(extBridge, bridgeCIDR.ipNet); err == nil && !exists {
			if err := util.LinkAddrAdd(extBridge, bridgeCIDR.ipNet, bridgeCIDR.flags, bridgeCIDR.preferredLifetime,
				bridgeCIDR.validLifetime); err != nil {
				return fmt.Errorf("failed to set node masq IP on bridge %s because unable to add address %s: %v",
					extBridgeName, bridgeCIDR.ipNet.String(), err)
			}
		} else if err == nil && exists && utilnet.IsIPv6(bridgeCIDR.ipNet.IP) {
			// FIXME(mk): remove this logic when it is no longer possible to upgrade from a version which doesn't have
			// a deprecated ipv6 host masq addr

			// Deprecate IPv6 address to prevent connections from using it as its source address. For connections towards
			// a service VIP, routes exist to explicitly add this address as source.
			isDeprecated, err := util.IsDeprecatedAddr(extBridge, bridgeCIDR.ipNet)
			if err != nil {
				return fmt.Errorf("failed to set node masq IP on bridge %s because unable to detect if address %s is deprecated: %v",
					extBridgeName, bridgeCIDR.ipNet.String(), err)
			}
			if !isDeprecated {
				if err = util.LinkAddrDel(extBridge, bridgeCIDR.ipNet); err != nil {
					klog.Warningf("Failed to delete stale masq IP %s on bridge %s because unable to delete it: %v",
						bridgeCIDR.ipNet.String(), extBridgeName, err)
				}
				if err = util.LinkAddrAdd(extBridge, bridgeCIDR.ipNet, bridgeCIDR.flags, bridgeCIDR.preferredLifetime,
					bridgeCIDR.validLifetime); err != nil {
					return err
				}
			}
		} else if err != nil {
			return fmt.Errorf(
				"failed to check existence of addr %s in bridge %s: %v", bridgeCIDR.ipNet, extBridgeName, err)
		}
	}

	return nil
}

func addHostMACBindings(bridgeName string) error {
	// Add a neighbour entry on the K8s node to map dummy next-hop masquerade
	// addresses with MACs. This is required because these addresses do not
	// exist on the network and will not respond to an ARP/ND, so to route them
	// we need an entry.
	// Additionally, the OVN Masquerade IP is not assigned to its interface, so
	// we also need a fake entry for that.
	link, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", bridgeName, err)
	}

	var neighborIPs []string
	if config.IPv4Mode {
		neighborIPs = append(neighborIPs, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String())
	}
	if config.IPv6Mode {
		neighborIPs = append(neighborIPs, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String())
	}
	for _, ip := range neighborIPs {
		klog.Infof("Ensuring IP Neighbor entry for: %s", ip)
		dummyNextHopMAC := util.IPAddrToHWAddr(net.ParseIP(ip))
		if exists, err := util.LinkNeighExists(link, net.ParseIP(ip), dummyNextHopMAC); err == nil && !exists {
			// LinkNeighExists checks if the mac also matches, but it is possible there is a stale entry
			// still in the neighbor cache which would prevent add. Therefore execute a delete first.
			if err = util.LinkNeighDel(link, net.ParseIP(ip)); err != nil {
				klog.Warningf("Failed to remove IP neighbor entry for ip %s, on iface %s: %v",
					ip, bridgeName, err)
			}
			if err = util.LinkNeighAdd(link, net.ParseIP(ip), dummyNextHopMAC); err != nil {
				return fmt.Errorf("failed to configure neighbor: %s, on iface %s: %v",
					ip, bridgeName, err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to configure neighbor:%s, on iface %s: %v", ip, bridgeName, err)
		}
	}
	return nil
}

func updateMasqueradeAnnotation(nodeName string, kube kube.Interface) error {
	_, v4MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
	_, v6MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)
	nodeAnnotation, err := util.CreateNodeMasqueradeSubnetAnnotation(nil, v4MasqueradeCIDR, v6MasqueradeCIDR)
	if err != nil {
		return fmt.Errorf("unable to generate masquerade subnet annotation update: %w", err)
	}
	if err := kube.SetAnnotationsOnNode(nodeName, nodeAnnotation); err != nil {
		return fmt.Errorf("unable to set node masquerade subnet annotation update: %w", err)
	}
	return nil
}

// deleteStaleMasqueradeResources removes stale Linux resources when config.Gateway.V4MasqueradeSubnet
// or config.Gateway.V6MasqueradeSubnet gets changed at day 2.
func deleteStaleMasqueradeResources(bridgeName, nodeName string, wf factory.NodeWatchFactory) error {
	var staleMasqueradeIPs config.MasqueradeIPsConfig
	node, err := wf.GetNode(nodeName)
	if err != nil {
		return err
	}
	subnets, err := util.ParseNodeMasqueradeSubnet(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// no annotation set, must be initial bring up, nothing to clean
			return nil
		}
		return err
	}

	var v4ConfiguredMasqueradeNet, v6ConfiguredMasqueradeNet *net.IPNet

	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ConfiguredMasqueradeNet = subnet
		} else if utilnet.IsIPv4CIDR(subnet) {
			v4ConfiguredMasqueradeNet = subnet
		} else {
			return fmt.Errorf("invalid subnet for masquerade annotation: %s", subnet)
		}
	}

	if v4ConfiguredMasqueradeNet != nil && config.Gateway.V4MasqueradeSubnet != v4ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV4MasqueradeIPs(v4ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V4MasqueradeIPs: %s", err)
		}
	}
	if v6ConfiguredMasqueradeNet != nil && config.Gateway.V6MasqueradeSubnet != v6ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV6MasqueradeIPs(v6ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V6MasqueradeIPs: %s", err)
		}
	}
	link, err := util.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", bridgeName, err)
	}

	if staleMasqueradeIPs.V4HostMasqueradeIP != nil || staleMasqueradeIPs.V6HostMasqueradeIP != nil {
		if err = deleteMasqueradeResources(link, &staleMasqueradeIPs); err != nil {
			klog.Errorf("Unable to delete masquerade resources! Some configuration for the masquerade subnet "+
				"may be left on the node and may cause issues! Errors: %v", err)
		}
	}

	return nil
}

// deleteMasqueradeResources removes following Linux resources given a config.MasqueradeIPsConfig
// struct and netlink.Link:
// - neighbour object for IPv4 and IPv6 OVNMasqueradeIP and DummyNextHopMasqueradeIP.
// - masquerade route added by addMasqueradeRoute function while starting up the gateway.
// - iptables rules created for masquerade subnet based on ipForwarding and Gateway mode.
// - stale HostMasqueradeIP address from gateway bridge
func deleteMasqueradeResources(link netlink.Link, staleMasqueradeIPs *config.MasqueradeIPsConfig) error {
	var subnets []*net.IPNet
	var neighborIPs []net.IP
	var aggregatedErrors []error
	klog.Infof("Stale masquerade resources detected, cleaning IPs: %s, %s, %s, %s",
		staleMasqueradeIPs.V4HostMasqueradeIP,
		staleMasqueradeIPs.V6HostMasqueradeIP,
		staleMasqueradeIPs.V4OVNMasqueradeIP,
		staleMasqueradeIPs.V6OVNMasqueradeIP)
	if config.IPv4Mode && staleMasqueradeIPs.V4HostMasqueradeIP != nil {
		// Delete any stale masquerade IP from external bridge.
		hostMasqIPNet, err := util.LinkAddrGetIPNet(link, staleMasqueradeIPs.V4HostMasqueradeIP)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("unable to get IPNet from link %s: %w", link, err))
		}
		if hostMasqIPNet != nil {
			if err := util.LinkAddrDel(link, hostMasqIPNet); err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove masquerade IP from bridge %s: %w", link, err))
			}
		}

		_, masqIPNet, err := net.ParseCIDR(fmt.Sprintf("%s/32", staleMasqueradeIPs.V4OVNMasqueradeIP.String()))
		if err != nil {
			aggregatedErrors = append(aggregatedErrors,
				fmt.Errorf("failed to parse V4OVNMasqueradeIP %s: %v", staleMasqueradeIPs.V4OVNMasqueradeIP.String(), err))
		}
		subnets = append(subnets, masqIPNet)
		neighborIPs = append(neighborIPs, staleMasqueradeIPs.V4OVNMasqueradeIP, staleMasqueradeIPs.V4DummyNextHopMasqueradeIP)
		if err := nodeipt.DelRules(getStaleMasqueradeIptablesRules(staleMasqueradeIPs.V4OVNMasqueradeIP)); err != nil {
			aggregatedErrors = append(aggregatedErrors,
				fmt.Errorf("failed to delete forwarding iptables rules for stale masquerade subnet %s: ", err))
		}
	}

	if config.IPv6Mode && staleMasqueradeIPs.V6HostMasqueradeIP != nil {
		// Delete any stale masquerade IP from external bridge.
		hostMasqIPNet, err := util.LinkAddrGetIPNet(link, staleMasqueradeIPs.V6HostMasqueradeIP)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("unable to get IPNet from link %s: %w", link, err))
		}
		if hostMasqIPNet != nil {
			if err := util.LinkAddrDel(link, hostMasqIPNet); err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove masquerade IP from bridge %s: %w", link, err))
			}
		}

		_, masqIPNet, err := net.ParseCIDR(fmt.Sprintf("%s/128", staleMasqueradeIPs.V6OVNMasqueradeIP.String()))
		if err != nil {
			return fmt.Errorf("failed to parse V6OVNMasqueradeIP %s: %v", staleMasqueradeIPs.V6OVNMasqueradeIP.String(), err)
		}
		subnets = append(subnets, masqIPNet)
		neighborIPs = append(neighborIPs, staleMasqueradeIPs.V6OVNMasqueradeIP, staleMasqueradeIPs.V6DummyNextHopMasqueradeIP)
		if err := nodeipt.DelRules(getStaleMasqueradeIptablesRules(staleMasqueradeIPs.V6OVNMasqueradeIP)); err != nil {
			return fmt.Errorf("failed to delete forwarding iptables rules for stale masquerade subnet %s: ", err)
		}
	}

	for _, ip := range neighborIPs {
		if err := util.LinkNeighDel(link, ip); err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove IP neighbour entry for ip %s, "+
				"on iface %s: %v", ip, link.Attrs().Name, err))
		}
	}

	if len(subnets) != 0 {
		if err := util.LinkRoutesDel(link, subnets); err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to list addresses for the link %s: %v", link.Attrs().Name, err))
		}
	}

	return utilerrors.Join(aggregatedErrors...)
}

// configureAdvertisedUDNIsolationNFTables configures nftables to drop traffic generated locally towards advertised UDN subnets.
// It sets up a nftables chain named nftablesUDNBGPOutputChain in the output hook with filter priority which drops
// traffic originating from the local node destined to nftablesAdvertisedUDNsSet.
// It creates nftablesAdvertisedUDNsSet[v4|v6] set which stores the subnets.
// Results in:
//
//	set advertised-udn-subnets-v4 {
//	  type ipv4_addr
//	  flags interval
//	  comment "advertised UDN V4 subnets"
//	}
//	set advertised-udn-subnets-v6 {
//	  type ipv6_addr
//	  flags interval
//	  comment "advertised UDN V6 subnets"
//	}
//	chain udn-bgp-drop {
//	  comment "Drop traffic generated locally towards advertised UDN subnets"
//	   type filter hook output priority filter; policy accept;
//	   ct state new ip daddr @advertised-udn-subnets-v4 counter packets 0 bytes 0 drop
//	   ct state new ip6 daddr @advertised-udn-subnets-v6 counter packets 0 bytes 0 drop
//	 }
func configureAdvertisedUDNIsolationNFTables() error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:    nftablesUDNBGPOutputChain,
		Comment: knftables.PtrTo("Drop traffic generated locally towards advertised UDN subnets"),

		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNBGPOutputChain})

	// TODO: clean up any stale entries in advertised-udn-subnets-v[4|6]
	set := &knftables.Set{
		Name:    nftablesAdvertisedUDNsSetV4,
		Comment: knftables.PtrTo("advertised UDN V4 subnets"),
		Type:    "ipv4_addr",
		Flags:   []knftables.SetFlag{knftables.IntervalFlag},
	}
	tx.Add(set)

	set = &knftables.Set{
		Name:    nftablesAdvertisedUDNsSetV6,
		Comment: knftables.PtrTo("advertised UDN V6 subnets"),
		Type:    "ipv6_addr",
		Flags:   []knftables.SetFlag{knftables.IntervalFlag},
	}
	tx.Add(set)

	tx.Add(&knftables.Rule{
		Chain: nftablesUDNBGPOutputChain,
		Rule:  knftables.Concat("ct state new", fmt.Sprintf("ip daddr @%s", nftablesAdvertisedUDNsSetV4), counterIfDebug, "drop"),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNBGPOutputChain,
		Rule:  knftables.Concat("ct state new", fmt.Sprintf("ip6 daddr @%s", nftablesAdvertisedUDNsSetV6), counterIfDebug, "drop"),
	})
	return nft.Run(context.TODO(), tx)
}
