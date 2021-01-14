package node

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init() error
	Run(<-chan struct{}, *sync.WaitGroup)
}

type gateway struct {
	// loadBalancerHealthChecker is a health check server for load-balancer type services
	loadBalancerHealthChecker informer.ServiceAndEndpointsEventHandler
	// portClaimWatcher is for reserving ports for virtual IPs allocated by the cluster on the host
	portClaimWatcher informer.ServiceEventHandler
	// nodePortWatcher is used in Shared GW mode to handle nodePort flows in shared OVS bridge
	nodePortWatcher informer.ServiceEventHandler
	// localPortWatcher is used in Local GW mode to handle iptables rules and routes for services
	localPortWatcher informer.ServiceEventHandler
	openflowManager  *openflowManager
	initFunc         func() error
	readyFunc        func() (bool, error)
}

func (g *gateway) AddService(svc *kapi.Service) error {
	var err error
	if g.portClaimWatcher != nil {
		err = g.portClaimWatcher.AddService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		err = g.loadBalancerHealthChecker.AddService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.nodePortWatcher != nil {
		err = g.nodePortWatcher.AddService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.localPortWatcher != nil {
		err = g.localPortWatcher.AddService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	return err
}

func (g *gateway) UpdateService(old, new *kapi.Service) error {
	var err error
	if g.portClaimWatcher != nil {
		err = g.portClaimWatcher.UpdateService(old, new)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		err = g.loadBalancerHealthChecker.UpdateService(old, new)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.nodePortWatcher != nil {
		err = g.nodePortWatcher.UpdateService(old, new)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.localPortWatcher != nil {
		err = g.localPortWatcher.UpdateService(old, new)
		if err != nil {
			klog.Error(err)
		}
	}
	return err
}

func (g *gateway) DeleteService(svc *kapi.Service) error {
	var err error
	if g.portClaimWatcher != nil {
		err = g.portClaimWatcher.DeleteService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		err = g.loadBalancerHealthChecker.DeleteService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.nodePortWatcher != nil {
		err = g.nodePortWatcher.DeleteService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	if g.localPortWatcher != nil {
		err = g.localPortWatcher.DeleteService(svc)
		if err != nil {
			klog.Error(err)
		}
	}
	return err
}

func (g *gateway) AddEndpoints(ep *kapi.Endpoints) error {
	if g.loadBalancerHealthChecker != nil {
		return g.loadBalancerHealthChecker.AddEndpoints(ep)
	}
	return nil
}

func (g *gateway) UpdateEndpoints(old, new *kapi.Endpoints) error {
	if g.loadBalancerHealthChecker != nil {
		return g.loadBalancerHealthChecker.UpdateEndpoints(old, new)
	}
	return nil
}

func (g *gateway) DeleteEndpoints(ep *kapi.Endpoints) error {
	if g.loadBalancerHealthChecker != nil {
		return g.loadBalancerHealthChecker.DeleteEndpoints(ep)
	}
	return nil
}

func (g *gateway) Init() error {
	return g.initFunc()
}

func (g *gateway) Run(stopChan <-chan struct{}, wg *sync.WaitGroup) {
	if g.openflowManager != nil {
		klog.Info("Spawning Conntrack Rule Check Thread")
		wg.Add(1)
		defer wg.Done()
		g.openflowManager.Run(stopChan)
	}
}

func gatewayInitInternal(nodeName, gwIntf string, subnets []*net.IPNet, gwNextHops []net.IP, nodeAnnotator kube.Annotator) (
	string, string, net.HardwareAddr, []*net.IPNet, error) {

	var bridgeName string
	var uplinkName string
	var brCreated bool
	var err error

	if bridgeName, _, err = util.RunOVSVsctl("--", "port-to-br", gwIntf); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err = util.GetNicName(bridgeName)
		if err != nil {
			return bridgeName, uplinkName, nil, nil, err
		}
	} else if _, _, err := util.RunOVSVsctl("--", "br-exists", gwIntf); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err = util.NicToBridge(gwIntf)
		if err != nil {
			return bridgeName, uplinkName, nil, nil, fmt.Errorf("failed to convert %s to OVS bridge: %v", gwIntf, err)
		}
		uplinkName = gwIntf
		gwIntf = bridgeName
		brCreated = true
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err = getIntfName(gwIntf)
		if err != nil {
			return bridgeName, uplinkName, nil, nil, err
		}
		bridgeName = gwIntf
	}

	// Now, we get IP addresses from OVS bridge. If IP does not exist,
	// error out.
	ips, err := getNetworkInterfaceIPAddresses(gwIntf)
	if err != nil {
		return bridgeName, uplinkName, nil, nil, fmt.Errorf("failed to get interface details for %s (%v)",
			gwIntf, err)
	}
	ifaceID, macAddress, err := bridgedGatewayNodeSetup(nodeName, bridgeName, gwIntf,
		types.PhysicalNetworkName, brCreated)
	if err != nil {
		return bridgeName, uplinkName, nil, nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	err = setupLocalNodeAccessBridge(nodeName, subnets)
	if err != nil {
		return bridgeName, uplinkName, nil, nil, err
	}
	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return bridgeName, uplinkName, nil, nil, err
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		Mode:           config.GatewayModeShared,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     macAddress,
		IPAddresses:    ips,
		NextHops:       gwNextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
		VLANID:         &config.Gateway.VLANID,
	})
	return bridgeName, uplinkName, macAddress, ips, err
}

func gatewayReady(patchPort string) (bool, error) {
	// Get ofport of patchPort
	ofportPatch, _, err := util.RunOVSVsctl("--if-exists", "get", "interface", patchPort, "ofport")
	if err != nil || len(ofportPatch) == 0 {
		return false, nil
	}
	klog.Info("Gateway is ready")
	return true, nil
}
