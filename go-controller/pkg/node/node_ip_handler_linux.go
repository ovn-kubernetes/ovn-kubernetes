//go:build linux
// +build linux

package node

import (
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type addressManager struct {
	nodeName           string
	nodeApiPrimaryAddr *net.IP // must store this for DPU
	watchFactory       factory.NodeWatchFactory
	addresses          sets.String
	nodeAnnotator      kube.Annotator
	mgmtPortConfig     *managementPortConfig
	gatewayBridge      *bridgeConfiguration
	OnChanged          func()
	sync.Mutex
	netEnumerator *addressManagerNetEnumerator
}

// Make addressManager mockable
// Shamelessly inspired from https://stackoverflow.com/questions/68940230/how-to-mock-net-interface
type addressManagerNetEnumerator struct {
	tickInterval                    int
	netInterfaceAddrs               func() ([]net.Addr, error)
	getNetworkInterfaceIPAddresses  func(iface string, kubeNodeIP net.IP) ([]*net.IPNet, error)
	netlinkAddrSubscribeWithOptions func(ch chan<- netlink.AddrUpdate, done <-chan struct{}, options netlink.AddrSubscribeOptions) error
}

func addressManagerDefaultEnumerator() *addressManagerNetEnumerator {
	return &addressManagerNetEnumerator{
		tickInterval:                    30,
		netInterfaceAddrs:               net.InterfaceAddrs,
		getNetworkInterfaceIPAddresses:  getNetworkInterfaceIPAddresses,
		netlinkAddrSubscribeWithOptions: netlink.AddrSubscribeWithOptions,
	}
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, k kube.Interface, config *managementPortConfig, watchFactory factory.NodeWatchFactory, gatewayBridge *bridgeConfiguration, amne *addressManagerNetEnumerator) *addressManager {
	mgr := &addressManager{
		nodeName:       nodeName,
		watchFactory:   watchFactory,
		addresses:      sets.NewString(),
		mgmtPortConfig: config,
		OnChanged:      func() {},
		gatewayBridge:  gatewayBridge,
	}
	if amne != nil {
		mgr.netEnumerator = amne
	} else {
		mgr.netEnumerator = addressManagerDefaultEnumerator()
	}
	mgr.nodeAnnotator = kube.NewNodeAnnotator(k, nodeName)
	mgr.sync()
	return mgr
}

// updates the address manager with a new IP
// returns true if there was an update
func (c *addressManager) addAddr(ip net.IP) bool {
	c.Lock()
	defer c.Unlock()
	if !c.addresses.Has(ip.String()) && c.isValidNodeIP(ip) {
		klog.Infof("Adding IP: %s, to node IP manager", ip)
		c.addresses.Insert(ip.String())
		return true
	}

	return false
}

// removes IP from address manager
// returns true if there was an update
func (c *addressManager) delAddr(ip net.IP) bool {
	c.Lock()
	defer c.Unlock()
	if c.addresses.Has(ip.String()) && c.isValidNodeIP(ip) {
		klog.Infof("Removing IP: %s, from node IP manager", ip)
		c.addresses.Delete(ip.String())
		return true
	}

	return false
}

// ListAddresses returns all the addresses we know about
func (c *addressManager) ListAddresses() []net.IP {
	c.Lock()
	defer c.Unlock()
	addrs := c.addresses.List()
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func (c *addressManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	var addrChan chan netlink.AddrUpdate
	addrSubscribeOptions := netlink.AddrSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during AddrSubscribe callback: %v", err)
			// Note: Not calling sync() from here: it is redudant and unsafe when stopChan is closed.
		},
	}

	subScribeFcn := func() (bool, error) {
		addrChan = make(chan netlink.AddrUpdate)
		if err := c.netEnumerator.netlinkAddrSubscribeWithOptions(addrChan, stopChan, addrSubscribeOptions); err != nil {
			return false, err
		}
		// sync the manager with current addresses on the node
		c.sync()
		return true, nil
	}

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		addressSyncTimer := time.NewTicker(time.Duration(c.netEnumerator.tickInterval) * time.Second)

		subscribed, err := subScribeFcn()
		if err != nil {
			klog.Error("Error during netlink subscribe for IP Manager: %v", err)
		}

		for {
			select {
			case a, ok := <-addrChan:
				addressSyncTimer.Reset(time.Duration(c.netEnumerator.tickInterval) * time.Second)
				if !ok {
					if subscribed, err = subScribeFcn(); err != nil {
						klog.Error("Error during netlink re-subscribe due to channel closing for IP Manager: %v", err)
					}
					continue
				}
				addrChanged := false
				if a.NewAddr {
					addrChanged = c.addAddr(a.LinkAddress.IP)
				} else {
					addrChanged = c.delAddr(a.LinkAddress.IP)
				}

				if addrChanged || !c.doesNodeHostAddressesMatch() || !c.doesNodeApiPrimaryAddrMatch() {
					klog.Infof("Added address %v. Updating node address annotation.", a.LinkAddress.IP)
					c.updateNodeAddressAnnotations()
				}
			case <-addressSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Node IP manager calling sync() explicitly")
					c.sync()
				} else {
					if subscribed, err = subScribeFcn(); err != nil {
						klog.Error("Error during netlink re-subscribe for IP Manager: %v", err)
					}
				}
			case <-stopChan:
				return
			}
		}
	}()

	klog.Info("Node IP manager is running")
}

func (c *addressManager) assignAddresses(nodeHostAddresses sets.String) bool {
	c.Lock()
	defer c.Unlock()

	if nodeHostAddresses.Equal(c.addresses) {
		return false
	}
	c.addresses = nodeHostAddresses
	return true
}

// doesNodeHostAddressesMatch returns false if annotation k8s.ovn.org/host-addresses
// does not match the current IP address set.
func (c *addressManager) doesNodeHostAddressesMatch() bool {
	c.Lock()
	defer c.Unlock()

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		klog.Errorf("Unable to get node from informer")
		return false
	}
	// check to see if ips on the node differ from what we stored
	// in annotation k8s.ovn.org/host-addresses
	nodeHostAddresses, err := util.ParseNodeHostAddresses(node)
	if err != nil {
		klog.Errorf("Unable to parse addresses from node host %s: %s", node.Name, err.Error())
		return false
	}

	return nodeHostAddresses.Equal(c.addresses)
}

// detects if the IP is valid for a node
// excludes things like local IPs, mgmt port ip
func (c *addressManager) isValidNodeIP(addr net.IP) bool {
	if addr == nil {
		return false
	}
	if addr.IsLinkLocalUnicast() {
		return false
	}
	if addr.IsLoopback() {
		return false
	}

	if utilnet.IsIPv4(addr) {
		if c.mgmtPortConfig.ipv4 != nil && c.mgmtPortConfig.ipv4.ifAddr.IP.Equal(addr) {
			return false
		}
	} else if utilnet.IsIPv6(addr) {
		if c.mgmtPortConfig.ipv6 != nil && c.mgmtPortConfig.ipv6.ifAddr.IP.Equal(addr) {
			return false
		}
	}

	return true
}

func (c *addressManager) sync() {
	// list all IP addresses on this system
	addrs, err := c.netEnumerator.netInterfaceAddrs()
	if err != nil {
		klog.Errorf("Failed to sync Node IP Manager: unable list all IPs on the node, error: %v", err)
		return
	}

	// eliminate any invalid or unusable IPs
	currAddresses := sets.NewString()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			klog.Errorf("Invalid IP address found on host: %s", addr.String())
			continue
		}
		if !c.isValidNodeIP(ip) {
			klog.V(5).Infof("Skipping non-useable IP address for host: %s", ip.String())
			continue
		}
		currAddresses.Insert(ip.String())
	}

	// report addrChanged only if the currAddresses set differs from c.addresses
	addrChanged := c.assignAddresses(currAddresses)
	if addrChanged || !c.doesNodeHostAddressesMatch() || !c.doesNodeApiPrimaryAddrMatch() {
		klog.Infof("Node address annotation being set to: %v addrChanged: %v", currAddresses, addrChanged)
		c.updateNodeAddressAnnotations()
	}
}

// updateNodeAddressAnnotations updates all relevant annotations for the node including
// k8s.ovn.org/host-addresses, k8s.ovn.org/node-primary-ifaddr, k8s.ovn.org/l3-gateway-config.
func (c *addressManager) updateNodeAddressAnnotations() {
	// Get node information
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		klog.Errorf("Unable to get node from informer: %v", err)
		return
	}
	nodeApiPrimaryAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		klog.Errorf("Failed to get node primary IP", nodeApiPrimaryAddrStr)
		return
	}
	nodeApiPrimaryAddr := net.ParseIP(nodeApiPrimaryAddrStr)
	if nodeApiPrimaryAddr == nil {
		klog.Errorf("Failed to parse kubernetes node IP address, err: %v", err)
		return
	}
	// Store this for later comparison
	c.nodeApiPrimaryAddr = &nodeApiPrimaryAddr

	// get updated interface IP addresses for the gateway bridge
	// getNetworkInterfaceIPAddresses will *only* return the primary IP address for a given interface
	// and will consider DPU
	// In case of DPU, nodeApiPrimaryAddr might be used
	ifAddrs, err := c.netEnumerator.getNetworkInterfaceIPAddresses(c.gatewayBridge.bridgeName, nodeApiPrimaryAddr)
	if err != nil {
		klog.Errorf("Failed to get gateway bridge IP addresses: %v", err)
		return
	}

	// update k8s.ovn.org/host-addresses
	if err := util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses); err != nil {
		klog.Errorf("Failed to set node annotations: %v", err)
		return
	}

	// sets both IPv4 and IPv6 primary IP addr in annotation k8s.ovn.org/node-primary-ifaddr
	// Note: this is not the API node's internal interface, but the primary IP on the gateway
	// bridge (cf. gateway_init.go)
	if err := util.SetNodePrimaryIfAddrs(c.nodeAnnotator, ifAddrs); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
		return
	}

	// update k8s.ovn.org/l3-gateway-config
	gatewayCfg, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		klog.Errorf("Unable to get node L3 gateway annotation, err: %v", err)
		return
	}
	gatewayCfg.IPAddresses = ifAddrs
	err = util.SetL3GatewayConfig(c.nodeAnnotator, gatewayCfg)
	if err != nil {
		klog.Errorf("Unable to set L3 gateway config, err: %v", err)
		return
	}

	// push all updates to the node
	if err := c.nodeAnnotator.Run(); err != nil {
		klog.Errorf("Failed to set node annotations, err: %v", err)
	}
	c.OnChanged()
}

// doesNodeApiPrimaryAddrMatch returns false if the node's primary API IP,
// i.e. the first NodeInternalIP or NodeExternalIP from node.Status.Addresses
// does not match the current address stored in c.nodeApiPrimaryAddr.
// This method doesn't really belong into the node address manager, but it is
// required here for DPU in case of an IP address change of the API IP.
func (c *addressManager) doesNodeApiPrimaryAddrMatch() bool {
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		klog.Errorf("Unable to get node from informer")
		return false
	}
	// check to see if ips on the node differ from what we stored
	// in annotation k8s.ovn.org/host-addresses
	nodeApiPrimaryAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		klog.Errorf("Failed to get node primary IP", nodeApiPrimaryAddrStr)
		return false
	}
	nodeApiPrimaryAddr := net.ParseIP(nodeApiPrimaryAddrStr)
	if nodeApiPrimaryAddr == nil {
		klog.Errorf("Failed to parse kubernetes node IP address, err: %v", err)
		return false
	}
	if c.nodeApiPrimaryAddr == nil {
		return false
	}
	return c.nodeApiPrimaryAddr.Equal(nodeApiPrimaryAddr)
}
