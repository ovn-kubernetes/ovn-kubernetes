package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// gatewayInit creates a gateway router for the local chassis.
func (oc *Controller) gatewayInit(nodeName string, clusterIPSubnet []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, sctpSupport bool, gwLRPIfAddrs, drLRPIfAddrs []*net.IPNet) error {

	gwLRPIPs := make([]net.IP, 0)
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPIPs = append(gwLRPIPs, gwLRPIfAddr.IP)
	}

	// Create a gateway router.
	gatewayRouter := types.GWRouterPrefix + nodeName
	physicalIPs := make([]string, len(l3GatewayConfig.IPAddresses))
	for i, ip := range l3GatewayConfig.IPAddresses {
		physicalIPs[i] = ip.IP.String()
	}

	logicalRouterOptions := map[string]string{
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         "true",
		"chassis":                       l3GatewayConfig.ChassisID,
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}
	// Local gateway mode does not need SNAT or routes on GR because GR is only used for multiple external gws
	// without SNAT. For normal N/S traffic, ingress/egress is mp0 on node switches
	if config.Gateway.Mode != config.GatewayModeLocal {
		// When there are multiple gateway routers (which would be the likely
		// default for any sane deployment), we need to SNAT traffic
		// heading to the logical space with the Gateway router's IP so that
		// return traffic comes back to the same gateway router.
		logicalRouterOptions["lb_force_snat_ip"] = "router_ip"
		logicalRouterOptions["snat-ct-zone"] = "0"
	}

	logicalRouter := nbdb.LogicalRouter{
		Name:        gatewayRouter,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelUpdates: []interface{}{
				&logicalRouter.Options,
				&logicalRouter.ExternalIDs,
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical router %v, err: %v", gatewayRouter, err)
	}

	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + gatewayRouter

	logicalSwitch := nbdb.LogicalSwitch{}
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": gwRouterPort,
		},
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.OVNJoinSwitch },
			OnModelMutations: func() []model.Mutation {
				return libovsdbops.OnReferentialModelMutation(&logicalSwitch.Ports, ovsdb.MutateOperationInsert, logicalSwitchPort)
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add port %q to logical switch %q, err: %v", gwSwitchPort, types.OVNJoinSwitch, err)
	}

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIPs[0])
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}

	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     gwRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			ExistingResult: &[]nbdb.LogicalRouterPort{},
		},
		{

			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: func() []model.Mutation {
				return libovsdbops.OnReferentialModelMutation(&logicalRouter.Ports, ovsdb.MutateOperationInsert, logicalRouterPort)
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port %q for gateway router %s, err: %v", gwRouterPort, gatewayRouter, err)
	}

	for _, entry := range clusterIPSubnet {
		drLRPIfAddr, err := util.MatchIPNetFamily(utilnet.IsIPv6CIDR(entry), drLRPIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed "+
				"router as the nexthop: %v",
				gatewayRouter, err)
		}

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix: entry.String(),
			Nexthop:  drLRPIfAddr.IP.String(),
		}

		tmpRouters := []nbdb.LogicalRouter{}
		if err := oc.nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter }).List(&tmpRouters); err != nil {
			return fmt.Errorf("unable to list logical router: %s, err: %v", gatewayRouter, err)
		}
		if len(tmpRouters) != 1 {
			return fmt.Errorf("unable to retrieve unique logical router: %s, found: %+v", gatewayRouter, tmpRouters)
		}

		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.IPPrefix == entry.String() && lrsr.Nexthop == drLRPIfAddr.IP.String() && libovsdbops.SliceHasStringItem(tmpRouters[0].StaticRoutes, lrsr.UUID)
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.IPPrefix,
					&logicalRouterStaticRoute.Nexthop,
				},
				ExistingResult: &[]nbdb.LogicalRouterStaticRoute{},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: func() []model.Mutation {
					return libovsdbops.OnReferentialModelMutation(&logicalRouter.StaticRoutes, ovsdb.MutateOperationInsert, logicalRouterStaticRoute)
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed router as the nexthop, err: %v", gatewayRouter, err)
		}
	}

	if err := oc.addExternalSwitch("",
		l3GatewayConfig.InterfaceID,
		nodeName,
		gatewayRouter,
		l3GatewayConfig.MACAddress.String(),
		types.PhysicalNetworkName,
		l3GatewayConfig.IPAddresses,
		l3GatewayConfig.VLANID); err != nil {
		return err
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		if err := oc.addExternalSwitch(types.EgressGWSwitchPrefix,
			l3GatewayConfig.EgressGWInterfaceID,
			nodeName,
			gatewayRouter,
			l3GatewayConfig.EgressGWMACAddress.String(),
			types.PhysicalNetworkExGwName,
			l3GatewayConfig.EgressGWIPAddresses,
			nil); err != nil {
			return err
		}
	}

	externalRouterPort := types.GWRouterToExtSwitchPrefix + gatewayRouter

	// Add static routes in GR with gateway router as the default next hop.
	for _, nextHop := range l3GatewayConfig.NextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   allIPs,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.OutputPort != nil && *lrsr.OutputPort == externalRouterPort && lrsr.Nexthop == nextHop.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
				},
				ExistingResult: &[]nbdb.LogicalRouterStaticRoute{},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: func() []model.Mutation {
					return libovsdbops.OnReferentialModelMutation(&logicalRouter.StaticRoutes, ovsdb.MutateOperationInsert, logicalRouterStaticRoute)
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
		}
	}

	// We need to add a route to the Gateway router's IP, on the
	// cluster router, to ensure that the return traffic goes back
	// to the same gateway router
	//
	// This can be removed once https://bugzilla.redhat.com/show_bug.cgi?id=1891516 is fixed.
	for _, gwLRPIP := range gwLRPIPs {

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwLRPIP.String(),
			Nexthop:  gwLRPIP.String(),
		}
		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.Nexthop == gwLRPIP.String() && lrsr.IPPrefix == gwLRPIP.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
					&logicalRouterStaticRoute.IPPrefix,
				},
				ExistingResult: &[]nbdb.LogicalRouterStaticRoute{},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					return libovsdbops.OnReferentialModelMutation(&logicalRouter.StaticRoutes, ovsdb.MutateOperationInsert, logicalRouterStaticRoute)
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
		}
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for _, hostSubnet := range hostSubnets {
		gwLRPIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), gwLRPIPs)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				types.OVNClusterRouter, err)
		}

		if config.Gateway.Mode != config.GatewayModeLocal {

			logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  gwLRPIP[0].String(),
			}
			opModels = []libovsdbops.OperationModel{
				{
					Model: &logicalRouterStaticRoute,
					ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
						return lrsr.Nexthop == gwLRPIP[0].String() && lrsr.IPPrefix == hostSubnet.String()
					},
					OnModelUpdates: []interface{}{
						&logicalRouterStaticRoute.Nexthop,
						&logicalRouterStaticRoute.IPPrefix,
					},
					ExistingResult: &[]nbdb.LogicalRouterStaticRoute{},
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
					OnModelMutations: func() []model.Mutation {
						return libovsdbops.OnReferentialModelMutation(&logicalRouter.StaticRoutes, ovsdb.MutateOperationInsert, logicalRouterStaticRoute)
					},
					ExistingResult: &[]nbdb.LogicalRouter{},
				},
			}
			if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
				return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
			}
		}
	}

	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	if !config.Gateway.DisableSNATMultipleGWs && config.Gateway.Mode != config.GatewayModeLocal {
		// Default SNAT rules.
		externalIPs := make([]net.IP, len(l3GatewayConfig.IPAddresses))
		for i, ip := range l3GatewayConfig.IPAddresses {
			externalIPs[i] = ip.IP
		}
		for _, entry := range clusterIPSubnet {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gatewayRouter, err)
			}
			if err := util.UpdateRouterSNAT(gatewayRouter, externalIP[0], entry); err != nil {
				return fmt.Errorf("failed to update NAT entry for pod subnet: %s, GR: %s, error: %v",
					entry.String(), gatewayRouter, err)
			}
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nats := []nbdb.NAT{}
			opModels = []libovsdbops.OperationModel{
				{
					ModelPredicate: func(nat *nbdb.NAT) bool {
						return nat.Type == "snat" && nat.LogicalIP == logicalSubnet.String()
					},
					ExistingResult: &nats,
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
					OnModelMutations: func() []model.Mutation {
						return libovsdbops.OnReferentialModelMutation(&logicalRouter.Nat, ovsdb.MutateOperationDelete, nats)
					},
					ExistingResult: &[]nbdb.LogicalRouter{},
				},
			}
			if err := oc.modelClient.Delete(opModels...); err != nil {
				return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s, for subnet: %s, err: %v", gatewayRouter, logicalSubnet, err)
			}
		}
	}
	return nil
}

// This DistributedGWPort guarantees to always have both IPv4 and IPv6 regardless of dual-stack
func addDistributedGWPort() error {
	masterChassisID, err := util.GetNodeChassisID()
	if err != nil {
		return fmt.Errorf("failed to get master's chassis ID error: %v", err)
	}

	// the distributed gateway port is always dual-stack and uses the IPv4 address to generate its mac
	dgpName := types.RouterToSwitchPrefix + types.NodeLocalSwitch
	dgpMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalDistributedGWPortIP)).String()
	dgpNetworkV4 := fmt.Sprintf("%s/%d", types.V4NodeLocalDistributedGWPortIP, types.V4NodeLocalNATSubnetPrefix)
	dgpNetworkV6 := fmt.Sprintf("%s/%d", types.V6NodeLocalDistributedGWPortIP, types.V6NodeLocalNATSubnetPrefix)

	// check if there is already a distributed gateway port
	dgpUUID, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "logical_router_port", "name="+dgpName)
	if err != nil {
		return fmt.Errorf("error executing find logical_router_port for distributed GW port, stderr: %q, %+v", stderr, err)
	}

	// if the port exists convert it to dual-stack if needed
	// otherwise create a new dual-stack port
	var nbctlArgs []string
	if len(dgpUUID) > 0 {
		klog.V(5).Infof("Distributed GW port already exists with uuid %s", dgpUUID)
		// update the mac address if necessary
		currentMac, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
			"get", "logical_router_port", dgpUUID, "mac")
		if err != nil {
			return err
		}
		if currentMac != dgpMac {
			_, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
				"set", "logical_router_port", dgpUUID, "mac=\""+dgpMac+"\"")
			if err != nil {
				return err
			}
			klog.V(5).Infof("Updated mac address of distributed GW port from %s to %s", currentMac, dgpMac)
		}
		// update the port networks if necessary
		currentNetworks, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
			"get", "logical_router_port", dgpUUID, "networks")
		if err != nil {
			return err
		}
		// only consider converting from single to dual-stack
		if len(strings.Split(currentNetworks, ",")) != 2 {
			_, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
				"set", "logical_router_port", dgpUUID, "networks=\""+dgpNetworkV4+"\",\""+dgpNetworkV6+"\"")
			if err != nil {
				return err
			}
			klog.V(5).Infof("Updated network addresses of distributed GW port from %s to %s,%s",
				currentNetworks, dgpNetworkV4, dgpNetworkV6)
		}
	} else {
		nbctlArgs = append(nbctlArgs, "lrp-add",
			types.OVNClusterRouter, dgpName, dgpMac, dgpNetworkV4, dgpNetworkV6,
		)
	}
	// set gateway chassis (the current master node) for distributed gateway port)
	nbctlArgs = append(nbctlArgs,
		"--", "--id=@gw", "create", "gateway_chassis", "chassis_name="+masterChassisID, "external_ids:dgp_name="+dgpName,
		fmt.Sprintf("name=%s_%s", dgpName, masterChassisID), "priority=100",
		"--", "set", "logical_router_port", dgpName, "gateway_chassis=@gw")
	stdout, stderr, err := util.RunOVNNbctl(nbctlArgs...)
	if err != nil {
		return fmt.Errorf("failed to set gateway chassis %s for distributed gateway port %s: "+
			"stdout: %q, stderr: %q, error: %v", masterChassisID, dgpName, stdout, stderr, err)
	}

	// connect the distributed gateway port to logical switch configured with localnet port
	nbctlArgs = []string{
		"--may-exist", "ls-add", types.NodeLocalSwitch,
	}
	// add localnet port to the logical switch
	lclNetPortname := "lnet-" + types.NodeLocalSwitch
	nbctlArgs = append(nbctlArgs,
		"--", "--may-exist", "lsp-add", types.NodeLocalSwitch, lclNetPortname,
		"--", "set", "logical_switch_port", lclNetPortname, "addresses=unknown", "type=localnet",
		"options:network_name="+types.LocalNetworkName)
	// connect the switch to the distributed router
	lspName := types.SwitchToRouterPrefix + types.NodeLocalSwitch
	nbctlArgs = append(nbctlArgs,
		"--", "--may-exist", "lsp-add", types.NodeLocalSwitch, lspName,
		"--", "set", "logical_switch_port", lspName, "type=router", "addresses=router",
		"options:nat-addresses=router", "options:router-port="+dgpName)
	stdout, stderr, err = util.RunOVNNbctl(nbctlArgs...)
	if err != nil {
		return fmt.Errorf("failed creating logical switch %s and its ports (%s, %s) "+
			"stdout: %q, stderr: %q, error: %v", types.NodeLocalSwitch, lclNetPortname, lspName, stdout, stderr, err)
	}
	// finally add an entry to the OVN SB MAC_Binding table, if not present, to capture the
	// MAC-IP binding of types.V4NodeLocalNATSubnetNextHop address or
	// types.V6NodeLocalNATSubnetNextHop address to its MAC. Normally, this will
	// be learnt and added by the chassis to which the distributed gateway port (DGP) is
	// bound. However, in our case we don't send any traffic out with the DGP port's IP
	// as source IP, so that binding will never be learnt and we need to seed it.

	var nodeLocalNatSubnetNextHop string
	dnatSnatNextHopMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalNATSubnetNextHop))
	nodeLocalNatSubnetNextHop = types.V4NodeLocalNATSubnetNextHop + " " + types.V6NodeLocalNATSubnetNextHop
	stdout, stderr, err = util.RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "MAC_Binding",
		"logical_port="+dgpName, fmt.Sprintf(`mac="%s"`, dnatSnatNextHopMac))
	if err != nil {
		return fmt.Errorf("failed to check existence of MAC_Binding entry of (%s, %s) for distributed router port %s "+
			"stderr: %q, error: %v", nodeLocalNatSubnetNextHop, dnatSnatNextHopMac, dgpName, stderr, err)
	}
	if stdout != "" {
		klog.Infof("The MAC_Binding entry of (%s, %s) exists on distributed router port %s with uuid %s",
			nodeLocalNatSubnetNextHop, dnatSnatNextHopMac, dgpName, stdout)
		return nil
	}

	for _, ip := range []string{types.V4NodeLocalNATSubnetNextHop, types.V6NodeLocalNATSubnetNextHop} {
		nextHop := net.ParseIP(ip)
		if err := util.CreateMACBinding(dgpName, types.OVNClusterRouter, dnatSnatNextHopMac, nextHop); err != nil {
			return fmt.Errorf("unable to create mac binding for DGP: %v", err)
		}
	}

	return nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (oc *Controller) addExternalSwitch(prefix, interfaceID, nodeName, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
	// Create the external switch for the physical interface to connect to.
	externalSwitch := fmt.Sprintf("%s%s%s", prefix, types.ExternalSwitchPrefix, nodeName)

	externalLogicalSwitch := nbdb.LogicalSwitch{
		Name: externalSwitch,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical switch %s, err: %v", externalSwitch, err)
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	externalLogicalSwitchPort := nbdb.LogicalSwitchPort{
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": physNetworkName,
		},
		Name: interfaceID,
	}
	if vlanID != nil {
		intVlanID := int(*vlanID)
		externalLogicalSwitchPort.TagRequest = &intVlanID
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalSwitchPort,
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPort.Addresses,
				&externalLogicalSwitchPort.Type,
				&externalLogicalSwitchPort.TagRequest,
				&externalLogicalSwitchPort.Options,
			},
			ExistingResult: &[]nbdb.LogicalSwitchPort{},
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: func() []model.Mutation {
				return libovsdbops.OnReferentialModelMutation(&externalLogicalSwitch.Ports, ovsdb.MutateOperationInsert, externalLogicalSwitchPort)
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", interfaceID, externalSwitch, err)
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address. In the case of `local` gateway mode, whenever ovnkube-node container
	// restarts a new br-local bridge will be created with a new `nicMacAddress`.
	externalRouterPort := types.GWRouterToExtSwitchPrefix + gatewayRouter

	externalRouterPortNetworks := []string{}
	for _, ip := range ipAddresses {
		externalRouterPortNetworks = append(externalRouterPortNetworks, ip.String())
	}
	externalLogicalRouterPort := nbdb.LogicalRouterPort{
		MAC: macAddress,
		ExternalIDs: map[string]string{
			"gateway-physical-ip": "yes",
		},
		Networks: externalRouterPortNetworks,
		Name:     externalRouterPort,
	}

	logicalRouter := nbdb.LogicalRouter{}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalRouterPort,
			OnModelUpdates: []interface{}{
				&externalLogicalRouterPort.MAC,
				&externalLogicalRouterPort.Networks,
			},
			ExistingResult: &[]nbdb.LogicalRouterPort{},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: func() []model.Mutation {
				return libovsdbops.OnReferentialModelMutation(&logicalRouter.Ports, ovsdb.MutateOperationInsert, externalLogicalRouterPort)
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port: %s to router %s, err: %v", externalRouterPort, gatewayRouter, err)
	}

	// Connect the external_switch to the router.
	externalSwitchPortToRouter := types.EXTSwitchToGWRouterPrefix + gatewayRouter

	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name: externalSwitchPortToRouter,
		Type: "router",
		Options: map[string]string{
			"router-port": externalRouterPort,
		},
		Addresses: []string{macAddress},
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalSwitchPortToRouter,
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPortToRouter.Addresses,
				&externalLogicalSwitchPortToRouter.Type,
				&externalLogicalSwitchPortToRouter.Options,
			},
			ExistingResult: &[]nbdb.LogicalSwitchPort{},
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: func() []model.Mutation {
				return libovsdbops.OnReferentialModelMutation(&externalLogicalSwitch.Ports, ovsdb.MutateOperationInsert, externalLogicalSwitchPortToRouter)
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", externalSwitchPortToRouter, externalSwitch, err)
	}
	return nil
}

func addPolicyBasedRoutes(nodeName, mgmtPortIP string, hostIfAddr *net.IPNet, otherHostAddrs []string) error {
	var l3Prefix string
	var natSubnetNextHop string
	if utilnet.IsIPv6(hostIfAddr.IP) {
		l3Prefix = "ip6"
		natSubnetNextHop = types.V6NodeLocalNATSubnetNextHop
	} else {
		l3Prefix = "ip4"
		natSubnetNextHop = types.V4NodeLocalNATSubnetNextHop
	}

	matches := sets.NewString()
	for _, hostIP := range append(otherHostAddrs, hostIfAddr.IP.String()) {
		// embed nodeName as comment so that it is easier to delete these rules later on.
		// logical router policy doesn't support external_ids to stash metadata
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.dst == %s /* %s */`,
			types.RouterToSwitchPrefix, nodeName, l3Prefix, hostIP, nodeName)
		matches = matches.Insert(matchStr)
	}
	if err := syncPolicyBasedRoutes(nodeName, matches, types.NodeSubnetPolicyPriority, mgmtPortIP); err != nil {
		return fmt.Errorf("unable to sync node subnet policies, err: %v", err)
	}

	if config.Gateway.Mode == config.GatewayModeLocal {

		// policy to allow host -> service -> hairpin back to host
		matchStr := fmt.Sprintf("%s.src == %s && %s.dst == %s /* %s */",
			l3Prefix, mgmtPortIP, l3Prefix, hostIfAddr.IP.String(), nodeName)
		if err := syncPolicyBasedRoutes(nodeName, sets.NewString(matchStr), types.MGMTPortPolicyPriority, natSubnetNextHop); err != nil {
			return fmt.Errorf("unable to sync management port policies, err: %v", err)
		}

		var matchDst string
		// Local gw mode needs to use DGP to do hostA -> service -> hostB
		var clusterL3Prefix string
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				clusterL3Prefix = "ip6"
			} else {
				clusterL3Prefix = "ip4"
			}
			if l3Prefix != clusterL3Prefix {
				continue
			}
			matchDst += fmt.Sprintf(" && %s.dst != %s", clusterL3Prefix, clusterSubnet.CIDR)
		}
		matchStr = fmt.Sprintf("%s.src == %s %s /* inter-%s */",
			l3Prefix, mgmtPortIP, matchDst, nodeName)
		if err := syncPolicyBasedRoutes(nodeName, sets.NewString(matchStr), types.InterNodePolicyPriority, natSubnetNextHop); err != nil {
			return fmt.Errorf("unable to sync inter-node policies, err: %v", err)
		}
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// if we are upgrading from Local to Shared gateway mode, we need to ensure the inter-node LRP is removed
		removeLRPolicies(nodeName, []string{types.InterNodePolicyPriority})
	}

	return nil
}

// This function syncs logical router policies given various criteria
// This function compares the following ovn-nbctl output:

// either

// 		72db5e49-0949-4d00-93e3-fe94442dd861,ip4.src == 10.244.0.2 && ip4.dst == 172.18.0.2 /* ovn-worker2 */,169.254.0.1
// 		6465e223-053c-4c74-a5f0-5f058c9f7a3e,ip4.src == 10.244.2.2 && ip4.dst == 172.18.0.3 /* ovn-worker */,169.254.0.1
// 		7debdcc6-ad5e-4825-9978-74bfbf8b7c27,ip4.src == 10.244.1.2 && ip4.dst == 172.18.0.4 /* ovn-control-plane */,169.254.0.1

// or

// 		c20ac671-704a-428a-a32b-44da2eec8456,"inport == ""rtos-ovn-worker2"" && ip4.dst == 172.18.0.2 /* ovn-worker2 */",10.244.0.2
// 		be7c8b53-f8ac-4051-b8f1-bfdb007d0956,"inport == ""rtos-ovn-worker"" && ip4.dst == 172.18.0.3 /* ovn-worker */",10.244.2.2
// 		fa8cf55d-a96c-4a53-9bf2-1c1fb1bc7a42,"inport == ""rtos-ovn-control-plane"" && ip4.dst == 172.18.0.4 /* ovn-control-plane */",10.244.1.2

// or

// 		822ab242-cce5-47b2-9c6f-f025f47e766a,ip4.src == 10.244.2.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker */,169.254.0.1
// 		a1b876f6-5ed4-4f88-b09c-7b4beed3b75f,ip4.src == 10.244.1.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-control-plane */,169.254.0.1
// 		0f5af297-74c8-4551-b10e-afe3b74bb000,ip4.src == 10.244.0.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker2 */,169.254.0.1

// The function checks to see if the mgmtPort IP has changed, or if match criteria has changed
// and removes stale policies for a node. It also adds new policies for a node at a specific priority.
// This is ugly (since the node is encoded as a comment in the match),
// but a necessary evil as any change to this would break upgrades and
// possible downgrades. We could make sure any upgrade encodes the node in
// the external_id, but since ovn-kubernetes isn't versioned, we won't ever
// know which version someone is running of this and when the switch to version
// N+2 is fully made.
func syncPolicyBasedRoutes(nodeName string, matches sets.String, priority, nexthop string) error {
	policiesCompare, err := findPolicyBasedRoutes(priority, "match,nexthops")
	if err != nil {
		return fmt.Errorf("unable to list policies, err: %v", err)
	}

	// create a map to track matches found
	matchTracker := sets.NewString(matches.List()...)

	// sync and remove unknown policies for this node/priority
	// also flag if desired policies are already found
	for _, policyCompare := range policiesCompare {
		if strings.Contains(policyCompare, fmt.Sprintf("%s\"", nodeName)) {
			// if the policy is for this node and has the wrong mgmtPortIP as nexthop, remove it
			// FIXME we currently assume that foundNexthops is a single ip, this may
			// change in the future.
			foundNexthops := strings.Split(policyCompare, ",")[2]
			if foundNexthops != "" && utilnet.IsIPv6String(foundNexthops) != utilnet.IsIPv6String(nexthop) {
				continue
			}
			if !strings.Contains(foundNexthops, nexthop) {
				uuid := strings.Split(policyCompare, ",")[0]
				if err := deletePolicyBasedRoutes(uuid, priority); err != nil {
					return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
						"error: %v", uuid, nodeName, types.OVNClusterRouter, err)
				}
				continue
			}
			desiredMatchFound := false
			for match := range matchTracker {
				if strings.Contains(policyCompare, match) {
					desiredMatchFound = true
					break
				}
			}
			// if the policy is for this node/priority and does not contain a valid match, remove it
			if !desiredMatchFound {
				uuid := strings.Split(policyCompare, ",")[0]
				if err := deletePolicyBasedRoutes(uuid, priority); err != nil {
					return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
						"error: %v", uuid, nodeName, types.OVNClusterRouter, err)
				}
				continue
			}
			// now check if the existing policy matches, remove it
			matchTracker.Delete(strings.Split(policyCompare, ",")[1])
		}
	}
	// cycle through all of the not found match criteria and create new policies
	for match := range matchTracker {
		if err := createPolicyBasedRoutes(match, priority, nexthop); err != nil {
			return fmt.Errorf("failed to add policy route '%s' for host %q on %s "+
				"error: %v", match, nodeName, types.OVNClusterRouter, err)
		}
	}
	return nil
}

func findPolicyBasedRoutes(priority, compareTo string) ([]string, error) {
	policyIDs, stderr, err := util.RunOVNNbctl(
		"--format=csv",
		"--data=bare",
		"--no-heading",
		fmt.Sprintf("--columns=_uuid,%s", compareTo),
		"find",
		"logical_router_policy",
		fmt.Sprintf("priority=%s", priority),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to find logical router policy, stderr: %s, err: %v", stderr, err)
	}
	if policyIDs == "" {
		return nil, nil
	}
	return strings.Split(policyIDs, "\n"), nil
}

func createPolicyBasedRoutes(match, priority, nexthops string) error {
	_, stderr, err := util.RunOVNNbctl(
		"--may-exist",
		"lr-policy-add",
		types.OVNClusterRouter,
		priority,
		match,
		"reroute",
		nexthops,
	)
	if err != nil {
		return fmt.Errorf("unable to create policy based routes, stderr: %s, err: %v", stderr, err)
	}
	return nil
}

func deletePolicyBasedRoutes(policyID, priority string) error {
	_, stderr, err := util.RunOVNNbctl(
		"lr-policy-del",
		types.OVNClusterRouter,
		policyID,
	)
	if err != nil {
		return fmt.Errorf("unable to delete logical router policy, stderr: %s, err: %v", stderr, err)
	}
	return nil
}

func (oc *Controller) addNodeLocalNatEntries(node *kapi.Node, mgmtPortMAC string, mgmtPortIfAddr *net.IPNet) error {
	var externalIP net.IP

	isIPv6 := utilnet.IsIPv6CIDR(mgmtPortIfAddr)
	annotationPresent := false
	externalIPs, err := util.ParseNodeLocalNatIPAnnotation(node)
	if err == nil {
		for _, ip := range externalIPs {
			if isIPv6 == utilnet.IsIPv6(ip) {
				klog.V(5).Infof("Found node local NAT IP %s in %v for the node %s, so reusing it", ip, externalIPs, node.Name)
				externalIP = ip
				annotationPresent = true
				break
			}
		}
	}
	if !annotationPresent {
		if isIPv6 {
			externalIP, err = oc.nodeLocalNatIPv6Allocator.AllocateNext()
		} else {
			externalIP, err = oc.nodeLocalNatIPv4Allocator.AllocateNext()
		}
		if err != nil {
			return fmt.Errorf("error allocating node local NAT IP for node %s: %v", node.Name, err)
		}
		externalIPs = append(externalIPs, externalIP)
		defer func() {
			// Release the allocation on error
			if err != nil {
				if isIPv6 {
					_ = oc.nodeLocalNatIPv6Allocator.Release(externalIP)
				} else {
					_ = oc.nodeLocalNatIPv4Allocator.Release(externalIP)
				}
			}
		}()
	}

	mgmtPortName := types.K8sPrefix + node.Name
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "lr-nat-del", types.OVNClusterRouter,
		"dnat_and_snat", externalIP.String())
	if err != nil {
		return fmt.Errorf("failed to delete dnat_and_snat entry for the management port on node %s, "+
			"stdout: %s, stderr: %q, error: %v", node.Name, stdout, stderr, err)
	}
	stdout, stderr, err = util.RunOVNNbctl("lr-nat-add", types.OVNClusterRouter, "dnat_and_snat",
		externalIP.String(), mgmtPortIfAddr.IP.String(), mgmtPortName, mgmtPortMAC)
	if err != nil {
		return fmt.Errorf("failed to add dnat_and_snat entry for the management port on node %s, "+
			"stdout: %s, stderr: %q, error: %v", node.Name, stdout, stderr, err)
	}

	if annotationPresent {
		return nil
	}
	// capture the node local NAT IP as a node annotation so that we can re-create it on onvkube-restart
	nodeAnnotations, err := util.CreateNodeLocalNatAnnotation(externalIPs)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for node local NAT IP %s",
			node.Name, externalIP.String())
	}
	err = oc.kube.SetAnnotationsOnNode(node.Name, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node local NAT IP annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}
