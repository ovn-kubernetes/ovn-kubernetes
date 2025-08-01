package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ip"
	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/libovsdb/client"

	honode "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	config "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/ovspinning"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	nodetypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

type CommonNodeNetworkControllerInfo struct {
	client                 clientset.Interface
	Kube                   kube.Interface
	watchFactory           factory.NodeWatchFactory
	recorder               record.EventRecorder
	name                   string
	apbExternalRouteClient adminpolicybasedrouteclientset.Interface
	// route manager that creates and manages routes
	routeManager *routemanager.Controller
}

// BaseNodeNetworkController structure per-network fields and network specific configuration
type BaseNodeNetworkController struct {
	CommonNodeNetworkControllerInfo

	// network information
	util.ReconcilableNetInfo

	// podNADToDPUCDMap tracks the NAD/DPU_ConnectionDetails mapping for all NADs that each pod requests.
	// Key is pod.UUID; value is nadToDPUCDMap (of map[string]*util.DPUConnectionDetails). Key of nadToDPUCDMap
	// is nadName; value is DPU_ConnectionDetails when VF representor is successfully configured for that
	// given NAD. DPU mode only
	// Note that we assume that Pod's Network Attachment Selection Annotation will not change over time.
	podNADToDPUCDMap sync.Map

	// stopChan and WaitGroup per controller
	stopChan chan struct{}
	wg       *sync.WaitGroup
}

func newCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, kube kube.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface,
	wf factory.NodeWatchFactory, eventRecorder record.EventRecorder, name string, routeManager *routemanager.Controller) *CommonNodeNetworkControllerInfo {

	return &CommonNodeNetworkControllerInfo{
		client:                 kubeClient,
		Kube:                   kube,
		apbExternalRouteClient: apbExternalRouteClient,
		watchFactory:           wf,
		name:                   name,
		recorder:               eventRecorder,
		routeManager:           routeManager,
	}
}

// NewCommonNodeNetworkControllerInfo creates and returns the base node network controller info
func NewCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface, wf factory.NodeWatchFactory,
	eventRecorder record.EventRecorder, name string, routeManager *routemanager.Controller) *CommonNodeNetworkControllerInfo {
	return newCommonNodeNetworkControllerInfo(kubeClient, &kube.Kube{KClient: kubeClient}, apbExternalRouteClient, wf, eventRecorder, name, routeManager)
}

// DefaultNodeNetworkController is the object holder for utilities meant for node management of default network
type DefaultNodeNetworkController struct {
	BaseNodeNetworkController

	mgmtPortController managementport.Controller
	Gateway            Gateway

	// Node healthcheck server for cloud load balancers
	healthzServer *proxierHealthUpdater
	routeManager  *routemanager.Controller
	linkManager   *linkmanager.Controller

	// retry framework for namespaces, used for the removal of stale conntrack entries for external gateways
	retryNamespaces *retry.RetryFramework
	// retry framework for endpoint slices, used for the removal of stale conntrack entries for services
	retryEndpointSlices *retry.RetryFramework

	// retry framework for nodes, used for updating routes/nftables rules for node PMTUD guarding
	retryNodes *retry.RetryFramework

	apbExternalRouteNodeController *apbroute.ExternalGatewayNodeController

	networkManager networkmanager.Interface

	cniServer *cni.Server

	udnHostIsolationManager *UDNHostIsolationManager

	nodeAddress net.IP
	sbZone      string

	ovsClient client.Client
}

func newDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, stopChan chan struct{},
	wg *sync.WaitGroup, routeManager *routemanager.Controller, ovsClient client.Client) *DefaultNodeNetworkController {

	c := &DefaultNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             &util.DefaultNetInfo{},
			stopChan:                        stopChan,
			wg:                              wg,
		},
		routeManager: routeManager,
		ovsClient:    ovsClient,
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		c.udnHostIsolationManager = NewUDNHostIsolationManager(config.IPv4Mode, config.IPv6Mode,
			cnnci.watchFactory.PodCoreInformer(), cnnci.name, cnnci.recorder)
	}
	c.linkManager = linkmanager.NewController(cnnci.name, config.IPv4Mode, config.IPv6Mode, c.updateGatewayMAC)
	return c
}

// NewDefaultNodeNetworkController creates a new network controller for node management of the default network
func NewDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, networkManager networkmanager.Interface, ovsClient client.Client) (*DefaultNodeNetworkController, error) {
	var err error
	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	nc := newDefaultNodeNetworkController(cnnci, stopChan, wg, cnnci.routeManager, ovsClient)

	if len(config.Kubernetes.HealthzBindAddress) != 0 {
		klog.Infof("Enable node proxy healthz server on %s", config.Kubernetes.HealthzBindAddress)
		nc.healthzServer, err = newNodeProxyHealthzServer(
			nc.name, config.Kubernetes.HealthzBindAddress, nc.recorder, nc.watchFactory)
		if err != nil {
			return nil, fmt.Errorf("could not create node proxy healthz server: %w", err)
		}
	}

	nc.apbExternalRouteNodeController, err = apbroute.NewExternalNodeController(
		nc.watchFactory.PodCoreInformer(),
		nc.watchFactory.NamespaceInformer(),
		nc.watchFactory.APBRouteInformer(),
		stopChan)
	if err != nil {
		return nil, err
	}

	nc.networkManager = networkManager

	nc.initRetryFrameworkForNode()

	err = setupRemoteNodeNFTSets()
	if err != nil {
		return nil, fmt.Errorf("failed to setup PMTUD nftables sets: %w", err)
	}

	err = setupPMTUDNFTChain()
	if err != nil {
		return nil, fmt.Errorf("failed to setup PMTUD nftables chain: %w", err)
	}

	return nc, nil
}

func (nc *DefaultNodeNetworkController) initRetryFrameworkForNode() {
	nc.retryNamespaces = nc.newRetryFrameworkNode(factory.NamespaceExGwType)
	nc.retryEndpointSlices = nc.newRetryFrameworkNode(factory.EndpointSliceForStaleConntrackRemovalType)
	nc.retryNodes = nc.newRetryFrameworkNode(factory.NodeType)
}

func (oc *DefaultNodeNetworkController) shouldReconcileNetworkChange(old, new util.NetInfo) bool {
	wasPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(old, oc.name)
	isPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(new, oc.name)
	return wasPodNetworkAdvertisedAtNode != isPodNetworkAdvertisedAtNode
}

func (oc *DefaultNodeNetworkController) Reconcile(netInfo util.NetInfo) error {
	// inspect changes first
	reconcilePodNetwork := oc.shouldReconcileNetworkChange(oc.ReconcilableNetInfo, netInfo)

	// reconcile subcontrollers
	if reconcilePodNetwork {
		isPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(netInfo, oc.name)
		if oc.Gateway != nil {
			oc.Gateway.SetDefaultPodNetworkAdvertised(isPodNetworkAdvertisedAtNode)
			err := oc.Gateway.Reconcile()
			if err != nil {
				return fmt.Errorf("failed to reconcile gateway: %v", err)
			}
		}

		if oc.mgmtPortController != nil {
			oc.mgmtPortController.Reconcile()
		}
	}

	// Update network information. We can do this now because gateway and
	// management port reconciliation done above does not rely on NetInfo
	err := util.ReconcileNetInfo(oc.ReconcilableNetInfo, netInfo)
	if err != nil {
		return fmt.Errorf("failed to reconcile network %s: %v", oc.GetNetworkName(), err)
	}

	return nil
}

func clearOVSFlowTargets() error {
	_, _, err := util.RunOVSVsctl(
		"--",
		"clear", "bridge", "br-int", "netflow",
		"--",
		"clear", "bridge", "br-int", "sflow",
		"--",
		"clear", "bridge", "br-int", "ipfix",
	)
	if err != nil {
		return err
	}
	return nil
}

// collectorsString joins all HostPort entry into a string that is acceptable as
// target by the ovs-vsctl command. If an entry has an empty host, it uses the Node IP
func collectorsString(node *corev1.Node, targets []config.HostPort) (string, error) {
	if len(targets) == 0 {
		return "", errors.New("collector targets can't be empty")
	}
	var joined strings.Builder
	for n, v := range targets {
		if n == 0 {
			joined.WriteByte('"')
		} else {
			joined.WriteString(`","`)
		}
		var host string
		if v.Host != nil && len(*v.Host) != 0 {
			host = v.Host.String()
		} else {
			var err error
			if host, err = util.GetNodePrimaryIP(node); err != nil {
				return "", fmt.Errorf("composing flow collectors' IPs: %w", err)
			}
		}
		joined.WriteString(util.JoinHostPortInt32(host, v.Port))
	}
	joined.WriteByte('"')
	return joined.String(), nil
}

func setOVSFlowTargets(node *corev1.Node) error {
	if len(config.Monitoring.NetFlowTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.NetFlowTargets)
		if err != nil {
			return fmt.Errorf("error joining NetFlow targets: %w", err)
		}

		_, stderr, err := util.RunOVSVsctl(
			"--",
			"--id=@netflow",
			"create",
			"netflow",
			fmt.Sprintf("targets=[%s]", collectors),
			"active_timeout=60",
			"--",
			"set", "bridge", "br-int", "netflow=@netflow",
		)
		if err != nil {
			return fmt.Errorf("error setting NetFlow: %v\n  %q", err, stderr)
		}
	}
	if len(config.Monitoring.SFlowTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.SFlowTargets)
		if err != nil {
			return fmt.Errorf("error joining SFlow targets: %w", err)
		}

		_, stderr, err := util.RunOVSVsctl(
			"--",
			"--id=@sflow",
			"create",
			"sflow",
			"agent="+types.SFlowAgent,
			fmt.Sprintf("targets=[%s]", collectors),
			"--",
			"set", "bridge", "br-int", "sflow=@sflow",
		)
		if err != nil {
			return fmt.Errorf("error setting SFlow: %v\n  %q", err, stderr)
		}
	}
	if len(config.Monitoring.IPFIXTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.IPFIXTargets)
		if err != nil {
			return fmt.Errorf("error joining IPFIX targets: %w", err)
		}

		args := []string{
			"--",
			"--id=@ipfix",
			"create",
			"ipfix",
			fmt.Sprintf("targets=[%s]", collectors),
			fmt.Sprintf("cache_active_timeout=%d", config.IPFIX.CacheActiveTimeout),
		}
		if config.IPFIX.CacheMaxFlows != 0 {
			args = append(args, fmt.Sprintf("cache_max_flows=%d", config.IPFIX.CacheMaxFlows))
		}
		if config.IPFIX.Sampling != 0 {
			args = append(args, fmt.Sprintf("sampling=%d", config.IPFIX.Sampling))
		}
		args = append(args, "--", "set", "bridge", "br-int", "ipfix=@ipfix")
		_, stderr, err := util.RunOVSVsctl(args...)
		if err != nil {
			return fmt.Errorf("error setting IPFIX: %v\n  %q", err, stderr)
		}
	}
	return nil
}

// validateEncapIP returns false if there is an error or if the given IP is not known local IP address.
func validateEncapIP(encapIP string) (bool, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return false, fmt.Errorf("failed to get all the links on the node: %v", err)
	}
	for _, link := range links {
		addrs, err := util.GetFilteredInterfaceAddrs(link, config.IPv4Mode, config.IPv6Mode)
		if err != nil {
			return false, err
		}
		for _, addr := range addrs {
			if addr.IP.String() == encapIP {
				return true, nil
			}
		}
	}
	return false, nil
}

func setupOVNNode(node *corev1.Node) error {
	var err error

	nodePrimaryIP, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return fmt.Errorf("failed to obtain local primary IP from node %q: %v", node.Name, err)
	}

	encapIP := config.Default.EncapIP
	if encapIP == "" {
		config.Default.EffectiveEncapIP = nodePrimaryIP
	} else {
		// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma.
		config.Default.EffectiveEncapIP = encapIP
		ovnEncapIps := strings.Split(encapIP, ",")
		for _, ovnEncapIp := range ovnEncapIps {
			if ip := net.ParseIP(strings.TrimSpace(ovnEncapIp)); ip == nil {
				return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIp, encapIP)
			}
		}
		// if there are more than one encap IPs, it must be configured explicitly. otherwise:
		if len(ovnEncapIps) == 1 {
			encapIP = ovnEncapIps[0]
			if encapIP == nodePrimaryIP {
				// the current encap IP is node primary IP, unset config.Default.EncapIP to indicate it is
				// implicitly configured through the old external_ids:ovn-encap-ip value and needs to be updated
				// if node primary IP changes.
				config.Default.EncapIP = ""
			} else {
				// the encap IP may be incorrectly set or;
				// previous implicitly set with the old primary node IP through the old external_ids:ovn-encap-ip value,
				// that has changed when ovnkube-node is down.
				// validate it to see if it is still a valid local IP address.
				valid, err := validateEncapIP(encapIP)
				if err != nil {
					return fmt.Errorf("invalid encap IP %s: %v", encapIP, err)
				}
				if !valid {
					return fmt.Errorf("invalid encap IP %s: does not exist", encapIP)
				}
			}
		}
	}

	setExternalIdsCmd := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", config.Default.EffectiveEncapIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:ovn-bridge-remote-probe-interval=%d",
			config.Default.OpenFlowProbe),
		// bundle-idle-timeout default value is 10s, it should be set
		// as high as the ovn-bridge-remote-probe-interval to allow ovn-controller
		// to finish computation specially with complex acl configuration with port range.
		fmt.Sprintf("other_config:bundle-idle-timeout=%d",
			config.Default.OpenFlowProbe),
		// If Interconnect feature is enabled, we want to tell ovn-controller to
		// make this node/chassis as an interconnect gateway.
		fmt.Sprintf("external_ids:ovn-is-interconn=%s", strconv.FormatBool(config.OVNKubernetesFeature.EnableInterconnect)),
		fmt.Sprintf("external_ids:ovn-monitor-all=%t", config.Default.MonitorAll),
		fmt.Sprintf("external_ids:ovn-ofctrl-wait-before-clear=%d", config.Default.OfctrlWaitBeforeClear),
		fmt.Sprintf("external_ids:ovn-enable-lflow-cache=%t", config.Default.LFlowCacheEnable),
		// when creating tunnel ports set local_ip, helps ensures multiple interfaces and ipv6 will work
		"external_ids:ovn-set-local-ip=\"true\"",
	}

	if config.Default.LFlowCacheLimit > 0 {
		setExternalIdsCmd = append(setExternalIdsCmd,
			fmt.Sprintf("external_ids:ovn-limit-lflow-cache=%d", config.Default.LFlowCacheLimit),
		)
	}

	if config.Default.LFlowCacheLimitKb > 0 {
		setExternalIdsCmd = append(setExternalIdsCmd,
			fmt.Sprintf("external_ids:ovn-memlimit-lflow-cache-kb=%d", config.Default.LFlowCacheLimitKb),
		)
	}

	// In the case of DPU, the hostname should be that of the DPU and not
	// the K8s Node's. So skip setting the incorrect hostname.
	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		setExternalIdsCmd = append(setExternalIdsCmd, fmt.Sprintf("external_ids:hostname=\"%s\"", node.Name))
	}

	_, stderr, err := util.RunOVSVsctl(setExternalIdsCmd...)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}

	// clear stale ovs flow targets if needed
	err = clearOVSFlowTargets()
	if err != nil {
		return fmt.Errorf("error clearing stale ovs flow targets: %q", err)
	}
	// set new ovs flow targets if needed
	err = setOVSFlowTargets(node)
	if err != nil {
		return fmt.Errorf("error setting ovs flow targets: %q", err)
	}

	return nil
}

func setEncapPort(ctx context.Context) error {
	systemID, err := util.GetNodeChassisID()
	if err != nil {
		return err
	}
	var uuid string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true,
		func(_ context.Context) (bool, error) {
			uuid, _, err = util.RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "Encap",
				fmt.Sprintf("chassis_name=%s", systemID))
			return len(uuid) != 0, err
		})
	if err != nil {
		return err
	}
	if len(uuid) == 0 {
		return fmt.Errorf("unable to find encap uuid to set geneve port for chassis %s", systemID)
	}
	_, stderr, errSet := util.RunOVNSbctl("set", "encap", uuid,
		fmt.Sprintf("options:dst_port=%d", config.Default.EncapPort),
	)
	if errSet != nil {
		return fmt.Errorf("error setting OVS encap-port: %v\n  %q", errSet, stderr)
	}
	return nil
}

func isOVNControllerReady() (bool, error) {
	// check node's connection status
	runDir := util.GetOvnRunDir()
	pid, err := os.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		return false, fmt.Errorf("unknown pid for ovn-controller process: %v", err)
	}
	ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSuffix(string(pid), "\n"))
	ret, _, err := util.RunOVSAppctl("-t", ctlFile, "connection-status")
	if err != nil {
		return false, fmt.Errorf("could not get connection status: %w", err)
	}
	klog.Infof("Node connection status = %s", ret)
	if ret != "connected" {
		return false, nil
	}

	// check whether br-int exists on node
	_, _, err = util.RunOVSVsctl("--", "br-exists", "br-int")
	if err != nil {
		return false, nil
	}

	// check by dumping br-int flow entries
	stdout, _, err := util.RunOVSOfctl("dump-aggregate", "br-int")
	if err != nil {
		klog.V(5).Infof("Error dumping aggregate flows: %v", err)
		return false, nil
	}
	hasFlowCountZero := strings.Contains(stdout, "flow_count=0")
	if hasFlowCountZero {
		klog.V(5).Info("Got a flow count of 0 when dumping flows for node")
		return false, nil
	}

	return true, nil
}

// getEnvNameFromResourceName gets the device plugin env variable from the device plugin resource name.
func getEnvNameFromResourceName(resource string) string {
	res1 := strings.ReplaceAll(resource, ".", "_")
	res2 := strings.ReplaceAll(res1, "/", "_")
	return "PCIDEVICE_" + strings.ToUpper(res2)
}

// getDeviceIdsFromEnv gets the list of device IDs from the device plugin env variable.
func getDeviceIdsFromEnv(envName string) ([]string, error) {
	envVar := os.Getenv(envName)
	if len(envVar) == 0 {
		return nil, fmt.Errorf("unexpected empty env variable: %s", envName)
	}
	deviceIds := strings.Split(envVar, ",")
	return deviceIds, nil
}

// handleDevicePluginResources tries to retrieve any device plugin resources passed in via arguments and device plugin env variables.
func handleDevicePluginResources() error {
	mgmtPortEnvName := getEnvNameFromResourceName(config.OvnKubeNode.MgmtPortDPResourceName)
	deviceIds, err := getDeviceIdsFromEnv(mgmtPortEnvName)
	if err != nil {
		return err
	}
	// The reason why we want to store the Device Ids in a map is prepare for various features that
	// require network resources such as the Management Port or Bypass Port. It is likely that these
	// features share the same device pool.
	config.OvnKubeNode.DPResourceDeviceIdsMap = make(map[string][]string)
	config.OvnKubeNode.DPResourceDeviceIdsMap[config.OvnKubeNode.MgmtPortDPResourceName] = deviceIds
	klog.V(5).Infof("Setting DPResourceDeviceIdsMap for %s using env %s with device IDs %v",
		config.OvnKubeNode.MgmtPortDPResourceName, mgmtPortEnvName, deviceIds)
	return nil
}

// reserveDeviceId takes the first device ID from a list of device IDs
// This function will not execute during runtime, only once at startup thus there
// is no undesirable side-effects of multiple allocations (causing pressure on the
// garbage collector)
func reserveDeviceId(deviceIds []string) (string, []string) {
	ret := deviceIds[0]
	deviceIds = deviceIds[1:]
	return ret, deviceIds
}

// handleNetdevResources tries to retrieve any device plugin interfaces to be used by the system such as the management port.
func handleNetdevResources(resourceName string) (string, error) {
	var deviceId string
	deviceIdsMap := &config.OvnKubeNode.DPResourceDeviceIdsMap
	if len((*deviceIdsMap)[resourceName]) > 0 {
		deviceId, (*deviceIdsMap)[resourceName] = reserveDeviceId((*deviceIdsMap)[resourceName])
	} else {
		return "", fmt.Errorf("insufficient device IDs for resource: %s", resourceName)
	}
	netdevice, err := util.GetNetdevNameFromDeviceId(deviceId, v1.DeviceInfo{})
	if err != nil {
		return "", err
	}
	return netdevice, nil
}

func exportManagementPortAnnotation(netdevName string, nodeAnnotator kube.Annotator) error {
	klog.Infof("Exporting management port annotation for netdev '%v'", netdevName)
	deviceID, err := util.GetDeviceIDFromNetdevice(netdevName)
	if err != nil {
		return err
	}
	vfindex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(deviceID)
	if err != nil {
		return err
	}
	pfindex, err := util.GetSriovnetOps().GetPfIndexByVfPciAddress(deviceID)
	if err != nil {
		return err
	}

	return util.SetNodeManagementPortAnnotation(nodeAnnotator, pfindex, vfindex)
}

func importManagementPortAnnotation(node *corev1.Node) (string, error) {
	klog.Infof("Import management port annotation on node '%v'", node.Name)
	pfId, vfId, err := util.ParseNodeManagementPortAnnotation(node)

	if err != nil {
		return "", err
	}
	klog.Infof("Imported pfId '%v' and FuncId '%v' for node '%v'", pfId, vfId, node.Name)

	return util.GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprintf("%d", pfId), fmt.Sprintf("%d", vfId))
}

// Take care of alternative names for the netdevName by making sure we
// use the link attribute name as well as handle the case when netdevName
// was renamed to types.K8sMgmtIntfName
func getManagementPortNetDev(netdevName string) (string, error) {
	link, err := util.GetNetLinkOps().LinkByName(netdevName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return "", fmt.Errorf("failed to lookup %s link: %v", netdevName, err)
		}
		// this may not the first time invoked on the node after reboot
		// netdev may have already been renamed to ovn-k8s-mp0.
		link, err = util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return "", fmt.Errorf("failed to get link device for %s. %v", netdevName, err)
		}
	}

	if link.Attrs().Name != netdevName {
		klog.Infof("'%v' != '%v' (link.Attrs().Name != netdevName)", link.Attrs().Name, netdevName)
	}
	return link.Attrs().Name, err
}

func getMgmtPortAndRepNameModeFull() (string, string, error) {
	if config.OvnKubeNode.MgmtPortNetdev == "" {
		return "", "", nil
	}
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return "", "", err
	}
	deviceID, err := util.GetDeviceIDFromNetdevice(netdevName)
	if err != nil {
		return "", "", fmt.Errorf("failed to get device id for %s: %v", netdevName, err)
	}
	rep, err := util.GetFunctionRepresentorName(deviceID)
	if err != nil {
		return "", "", err
	}
	return netdevName, rep, err
}

// In DPU mode, read the annotation from the host side which should have been
// exported by ovn-k running in DPU host mode.
func getMgmtPortAndRepNameModeDPU(node *corev1.Node) (string, string, error) {
	rep, err := importManagementPortAnnotation(node)
	if err != nil {
		return "", "", err
	}
	return "", rep, err
}

func getMgmtPortAndRepNameModeDPUHost() (string, string, error) {
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return "", "", err
	}
	return netdevName, "", nil
}

func getMgmtPortAndRepName(node *corev1.Node) (string, string, error) {
	switch config.OvnKubeNode.Mode {
	case types.NodeModeFull:
		return getMgmtPortAndRepNameModeFull()
	case types.NodeModeDPU:
		return getMgmtPortAndRepNameModeDPU(node)
	case types.NodeModeDPUHost:
		return getMgmtPortAndRepNameModeDPUHost()
	default:
		return "", "", fmt.Errorf("unexpected config.OvnKubeNode.Mode '%v'", config.OvnKubeNode.Mode)
	}
}

func createNodeManagementPortController(
	node *corev1.Node,
	subnets []*net.IPNet,
	nodeAnnotator kube.Annotator,
	routeManager *routemanager.Controller,
	netInfo util.NetInfo,
) (managementport.Controller, error) {
	netdevName, rep, err := getMgmtPortAndRepName(node)
	if err != nil {
		return nil, err
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		err := exportManagementPortAnnotation(netdevName, nodeAnnotator)
		if err != nil {
			return nil, err
		}
	}
	return managementport.NewManagementPortController(node, subnets, netdevName, rep, routeManager, netInfo)
}

// getOVNSBZone returns the zone name stored in the Southbound db.
// It returns the default zone name if "options:name" is not set in the SB_Global row
func getOVNSBZone() (string, error) {
	dbZone, stderr, err := util.RunOVNSbctl("get", "SB_Global", ".", "options:name")
	if err != nil {
		if strings.Contains(stderr, "ovn-sbctl: no key \"name\" in SB_Global record") {
			// If the options:name is not present, assume default zone
			return types.OvnDefaultZone, nil
		}
		return "", err
	}

	return dbZone, nil
}

/** HACK BEGIN **/
// TODO(tssurya): Remove this HACK a few months from now.
// checkOVNSBNodeLRSR returns true if the logical router static route for the
// the given nodeSubnet is present in the SBDB
func checkOVNSBNodeLRSR(nodeSubnet *net.IPNet) bool {
	var matchv4, matchv6 string
	v6 := true
	v4 := true
	if config.IPv6Mode && utilnet.IsIPv6CIDR(nodeSubnet) {
		matchv6 = fmt.Sprintf("match=\"reg7 == 0 && ip6.dst == %s\"", nodeSubnet)
		stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "logical_flow", matchv6)
		klog.Infof("Upgrade Hack: checkOVNSBNodeLRSR for node - %s : match %s : stdout - %s : stderr - %s : err %v",
			nodeSubnet, matchv6, stdout, stderr, err)
		v6 = (err == nil && stderr == "" && stdout != "")
	}
	if config.IPv4Mode && !utilnet.IsIPv6CIDR(nodeSubnet) {
		matchv4 = fmt.Sprintf("match=\"reg7 == 0 && ip4.dst == %s\"", nodeSubnet)
		stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "logical_flow", matchv4)
		klog.Infof("Upgrade Hack: checkOVNSBNodeLRSR for node - %s : match %s : stdout - %s : stderr - %s : err %v",
			nodeSubnet, matchv4, stdout, stderr, err)
		v4 = (err == nil && stderr == "" && stdout != "")
	}
	return v6 && v4
}

func fetchLBNames() string {
	stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "name", "find", "Load_Balancer")
	if err != nil || stderr != "" {
		klog.Errorf("Upgrade hack: fetchLBNames could not fetch services %v/%v", err, stderr)
		return stdout // will be empty and we will retry
	}
	klog.Infof("Upgrade Hack: fetchLBNames: stdout - %s : stderr - %s : err %v", stdout, stderr, err)
	return stdout
}

// lbExists returns true if the OVN load balancer for the corresponding namespace/name
// was created
func lbExists(lbNames, namespace, name string) bool {
	stitchedServiceName := "Service_" + namespace + "/" + name
	match := strings.Contains(lbNames, stitchedServiceName)
	klog.Infof("Upgrade Hack: lbExists for service - %s/%s/%s : match - %v",
		namespace, name, stitchedServiceName, match)
	return match
}

func portExists(namespace, name string) bool {
	lspName := fmt.Sprintf("logical_port=%s", util.GetLogicalPortName(namespace, name))
	stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "Port_Binding", lspName)
	klog.Infof("Upgrade Hack: portExists for pod - %s/%s : stdout - %s : stderr - %s", namespace, name, stdout, stderr)
	return err == nil && stderr == "" && stdout != ""
}

/** HACK END **/

// Init executes the first steps to start the DefaultNodeNetworkController.
// It is split from Start() and executed before SecondaryNodeNetworkController (SNNC),
// to allow SNNC to reference the openflow manager created in Init.
func (nc *DefaultNodeNetworkController) Init(ctx context.Context) error {
	klog.Infof("Initializing the default node network controller")

	var err error
	var node *corev1.Node
	var subnets []*net.IPNet
	var cniServer *cni.Server

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("Setting klog \"loglevel\" to 5 failed, err: %v", err)
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		if err = configureGlobalForwarding(); err != nil {
			return err
		}
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Bootstrap flows in OVS if just normal flow is present
		if err := bootstrapOVSFlows(nc.name); err != nil {
			return fmt.Errorf("failed to bootstrap OVS flows: %w", err)
		}
	}

	if node, err = nc.watchFactory.GetNode(nc.name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", nc.name, err)
	}

	nodeAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return err
	}
	nodeAddr := net.ParseIP(nodeAddrStr)
	if nodeAddr == nil {
		return fmt.Errorf("failed to parse kubernetes node IP address. %v", nodeAddrStr)
	}

	// Make sure that the node zone matches with the Southbound db zone.
	// Wait for 300s before giving up
	var sbZone string
	var err1 error

	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		// There is no SBDB to connect to in DPU Host mode, so we will just take the default input config zone
		sbZone = config.Default.Zone
	} else {
		err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(_ context.Context) (bool, error) {
			sbZone, err = getOVNSBZone()
			if err != nil {
				err1 = fmt.Errorf("failed to get the zone name from the OVN Southbound db server, err : %w", err)
				return false, nil
			}

			if config.Default.Zone != sbZone {
				err1 = fmt.Errorf("node %s zone %s mismatch with the Southbound zone %s", nc.name, config.Default.Zone, sbZone)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("timed out waiting for the node zone %s to match the OVN Southbound db zone, err: %v, err1: %v", config.Default.Zone, err, err1)
		}

		// if its nonIC OR IC=true and if its phase1 OR if its IC to IC upgrades
		if !config.OVNKubernetesFeature.EnableInterconnect || sbZone == types.OvnDefaultZone || util.HasNodeMigratedZone(node) { // if its nonIC or if its phase1
			for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
				if err := auth.SetDBAuth(); err != nil {
					return err
				}
			}
		}

		err = setupOVNNode(node)
		if err != nil {
			return err
		}
		if nc.udnHostIsolationManager != nil {
			if err = nc.udnHostIsolationManager.Start(ctx); err != nil {
				return err
			}
		} else {
			if err = CleanupUDNHostIsolation(); err != nil {
				return fmt.Errorf("failed cleaning up UDN host isolation: %w", err)
			}
		}
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(_ context.Context) (bool, error) {
		if node, err = nc.watchFactory.GetNode(nc.name); err != nil {
			klog.Infof("Waiting to retrieve node %s: %v", nc.name, err)
			return false, nil
		}
		subnets, err = util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		if err != nil {
			klog.Infof("Waiting for node %s to start, no annotation found on node for subnet: %v", nc.name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for node's: %q logical switch: %v", nc.name, err)
	}
	klog.Infof("Node %s ready for ovn initialization with subnet %s", nc.name, util.JoinIPNets(subnets, ","))

	// Create CNI Server
	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		kclient, ok := nc.Kube.(*kube.Kube)
		if !ok {
			return fmt.Errorf("cannot get kubeclient for starting CNI server")
		}
		cniServer, err = cni.NewCNIServer(nc.watchFactory, kclient.KClient, nc.networkManager, nc.ovsClient)
		if err != nil {
			return err
		}
		nc.cniServer = cniServer
	}

	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, node.Name)

	// Setup management ports
	nc.mgmtPortController, err = createNodeManagementPortController(
		node,
		subnets,
		nodeAnnotator,
		nc.routeManager,
		nc.GetNetInfo(),
	)
	if err != nil {
		return err
	}
	nc.nodeAddress = nodeAddr

	if err := util.SetNodeZone(nodeAnnotator, sbZone); err != nil {
		return fmt.Errorf("failed to set node zone annotation for node %s: %w", nc.name, err)
	}

	encapIPList := sets.New[string]()
	encapIPList.Insert(strings.Split(config.Default.EffectiveEncapIP, ",")...)
	if err := util.SetNodeEncapIPs(nodeAnnotator, encapIPList); err != nil {
		return fmt.Errorf("failed to set node-encap-ips annotation for node %s: %w", nc.name, err)
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	// Connect ovn-controller to SBDB
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
			if err := auth.SetDBAuth(); err != nil {
				return fmt.Errorf("unable to set the authentication towards OVN local dbs")
			}
		}
	}

	// First part of gateway initialization. It will be completed by (nc *DefaultNodeNetworkController) Start()
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// IPv6 is not supported in DPU enabled nodes, error out if ovnkube is not set in IPv4 mode
		if config.IPv6Mode && config.OvnKubeNode.Mode == types.NodeModeDPU {
			return fmt.Errorf("IPv6 mode is not supported on a DPU enabled node")
		}
		// Initialize gateway for OVS internal port or representor management port
		gw, err := nc.initGatewayPreStart(subnets, nodeAnnotator, nc.mgmtPortController)
		if err != nil {
			return err
		}
		nc.Gateway = gw
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("Reset of initial klog \"loglevel\" failed, err: %v", err)
	}
	nc.sbZone = sbZone

	return nil
}

// Start learns the subnets assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (nc *DefaultNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting the default node network controller")

	var err error
	var node *corev1.Node

	if nc.mgmtPortController == nil {
		return fmt.Errorf("default node network controller hasn't been pre-started")
	}

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("Setting klog \"loglevel\" to 5 failed, err: %v", err)
	}

	if node, err = nc.watchFactory.GetNode(nc.name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", nc.name, err)
	}

	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, node.Name)
	waiter := newStartupWaiter()

	// Use the device from environment when the DP resource name is specified.
	if config.OvnKubeNode.MgmtPortDPResourceName != "" {
		if err := handleDevicePluginResources(); err != nil {
			return err
		}

		netdevice, err := handleNetdevResources(config.OvnKubeNode.MgmtPortDPResourceName)
		if err != nil {
			return err
		}

		if config.OvnKubeNode.MgmtPortNetdev != "" {
			klog.Warningf("MgmtPortNetdev is set explicitly (%s), overriding with resource...",
				config.OvnKubeNode.MgmtPortNetdev)
		}
		config.OvnKubeNode.MgmtPortNetdev = netdevice
		klog.V(5).Infof("Using MgmtPortNetdev (Netdev %s) passed via resource %s",
			config.OvnKubeNode.MgmtPortNetdev, config.OvnKubeNode.MgmtPortDPResourceName)
	}

	// Complete gateway initialization
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		// Resolve gateway interface from PCI address when configured as "derive-from-mgmt-port"
		// This performs the following steps:
		// Get the management port network device name
		// Retrieve the PCI address of the management port device
		// Get the Physical Function (PF) PCI address from the Virtual Function (VF) PCI address
		// Retrieve all network devices associated with the PF PCI address
		// Select the first available network device as the gateway interface
		if config.Gateway.Interface == types.DeriveFromMgmtPort {
			netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
			if err != nil {
				return err
			}
			pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
			if err != nil {
				return err
			}
			pfPciAddr, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
			if err != nil {
				return err
			}
			netdevs, err := util.GetSriovnetOps().GetNetDevicesFromPci(pfPciAddr)
			if err != nil {
				return err
			}
			if len(netdevs) == 0 {
				return fmt.Errorf("no netdevs found for pci address %s", pfPciAddr)
			}
			netdevName = netdevs[0]
			config.Gateway.Interface = netdevName
		}
		err = nc.initGatewayDPUHost(nc.nodeAddress, nodeAnnotator)
		if err != nil {
			return err
		}
	} else {
		gw := nc.Gateway.(*gateway)
		if err := nc.initGatewayMainStart(gw, waiter); err != nil {
			return err
		}
	}

	// If EncapPort is not the default tell sbdb to use specified port.
	// We set the encap port after annotating the zone name so that ovnkube-controller has come up
	// and configured the chassis in SBDB (ovnkube-controller waits for ovnkube-node to set annotation
	// for at least one node in the given zone)
	// NOTE: ovnkube-node in DPU-host mode has no SBDB to connect to. The encap port will be handled by the
	// ovnkube-node running in DPU mode on behalf of the host.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.Default.EncapPort != config.DefaultEncapPort {
		if err := setEncapPort(ctx); err != nil {
			return err
		}
	}

	/** HACK BEGIN **/
	// TODO(tssurya): Remove this HACK a few months from now. This has been added only to
	// minimize disruption for upgrades when moving to interconnect=true.
	// We want the legacy ovnkube-master to wait for remote ovnkube-node to
	// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
	// considering a node as remote when we upgrade from "global" (1 zone IC)
	// zone to multi-zone. This is so that network disruption for the existing workloads
	// is negligible and until the point where ovnkube-node flips the switch to connect
	// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
	// OVN/OVS flows are intact.
	// STEP1: ovnkube-node start's up in remote zone and sets the "k8s.ovn.org/zone-name" above.
	// STEP2: We delay the flip of connection for ovnkube-node(ovn-controller) to the new remote SBDB
	//        until the new remote ovnkube-controller has finished programming all the K8s core objects
	//        like routes, services and pods. Until then the ovnkube-node will talk to legacy SBDB.
	// STEP3: Once we get the signal that the new SBDB is ready, we set the "k8s.ovn.org/remote-zone-migrated" annotation
	// STEP4: We call setDBAuth to now point to new SBDB
	// STEP5: Legacy ovnkube-master sees "k8s.ovn.org/remote-zone-migrated" annotation on this node and now knows that
	//        this node has remote-zone-migrated successfully and tears down old setup and creates new IC resource
	//        plumbing (takes 80ms based on what we saw in CI runs so we might still have that small window of disruption).
	// NOTE: ovnkube-node in DPU host mode doesn't go through upgrades for OVN-IC and has no SBDB to connect to. Thus this part shall be skipped.
	var syncNodes, syncServices, syncPods bool
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.OVNKubernetesFeature.EnableInterconnect && nc.sbZone != types.OvnDefaultZone && !util.HasNodeMigratedZone(node) {
		klog.Info("Upgrade Hack: Interconnect is enabled")
		var err1 error
		start := time.Now()
		err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(_ context.Context) (bool, error) {
			// we loop through all the nodes in the cluster and ensure ovnkube-controller has finished creating the LRSR required for pod2pod overlay communication
			if !syncNodes {
				nodes, err := nc.watchFactory.GetNodes()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving node %s: %v", nc.name, err)
					return false, nil
				}
				for _, node := range nodes {
					node := *node
					if nc.name != node.Name && util.GetNodeZone(&node) != config.Default.Zone && !util.NoHostSubnet(&node) {
						nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(&node, types.DefaultNetworkName)
						if err != nil {
							if util.IsAnnotationNotSetError(err) {
								klog.Infof("Skipping node %q. k8s.ovn.org/node-subnets annotation was not found", node.Name)
								continue
							}
							err1 = fmt.Errorf("unable to fetch node-subnet annotation for node %s: err, %v", node.Name, err)
							return false, nil
						}
						for _, nodeSubnet := range nodeSubnets {
							klog.Infof("Upgrade Hack: node %s, subnet %s", node.Name, nodeSubnet)
							if !checkOVNSBNodeLRSR(nodeSubnet) {
								err1 = fmt.Errorf("upgrade hack: unable to find LRSR for node %s", node.Name)
								return false, nil
							}
						}
					}
				}
				klog.Infof("Upgrade Hack: Syncing nodes took %v", time.Since(start))
				syncNodes = true
			}
			// we loop through all existing services in the cluster and ensure ovnkube-controller has finished creating LoadBalancers required for services to work
			if !syncServices {
				services, err := nc.watchFactory.GetServices()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving the services %v", err)
					return false, nil
				}
				lbNames := fetchLBNames()
				for _, s := range services {
					// don't process headless service
					if !util.ServiceTypeHasClusterIP(s) || !util.IsClusterIPSet(s) {
						continue
					}
					if !lbExists(lbNames, s.Namespace, s.Name) {
						return false, nil
					}
				}
				klog.Infof("Upgrade Hack: Syncing services took %v", time.Since(start))
				syncServices = true
			}
			if !syncPods {
				pods, err := nc.watchFactory.GetAllPods()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving the services %v", err)
					return false, nil
				}
				for _, p := range pods {
					if !util.PodScheduled(p) || util.PodCompleted(p) || util.PodWantsHostNetwork(p) {
						continue
					}
					if p.Spec.NodeName != nc.name {
						// remote pod
						continue
					}
					if !portExists(p.Namespace, p.Name) {
						return false, nil
					}
				}
				klog.Infof("Upgrade Hack: Syncing pods took %v", time.Since(start))
				syncPods = true
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("upgrade hack: failed while waiting for the remote ovnkube-controller to be ready: %v, %v", err, err1)
		}
		if err := util.SetNodeZoneMigrated(nodeAnnotator, nc.sbZone); err != nil {
			return fmt.Errorf("upgrade hack: failed to set node zone annotation for node %s: %w", nc.name, err)
		}
		if err := nodeAnnotator.Run(); err != nil {
			return fmt.Errorf("upgrade hack: failed to set node %s annotations: %w", nc.name, err)
		}
		klog.Infof("ovnkube-node %s finished annotating node with remote-zone-migrated; took: %v", nc.name, time.Since(start))
		for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
			if err := auth.SetDBAuth(); err != nil {
				return fmt.Errorf("upgrade hack: Unable to set the authentication towards OVN local dbs")
			}
		}
		klog.Infof("Upgrade hack: ovnkube-node %s finished setting DB Auth; took: %v", nc.name, time.Since(start))
	}
	/** HACK END **/

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	err = nc.Gateway.Start()
	if err != nil {
		return fmt.Errorf("failed to start gateway: %w", err)
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	// Note(adrianc): DPU deployments are expected to support the new shared gateway changes, upgrade flow
	// is not needed. Future upgrade flows will need to take DPUs into account.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		if config.OvnKubeNode.Mode == types.NodeModeFull {
			// Configure route for svc towards shared gateway interface
			if err := configureSvcRouteViaInterface(nc.routeManager, nc.Gateway.GetGatewayIface(), DummyNextHopIPs()); err != nil {
				return err
			}
		}
	}

	if config.HybridOverlay.Enabled {
		// Not supported with DPUs, enforced in config
		// TODO(adrianc): Revisit above comment
		nodeController, err := honode.NewNode(
			nc.Kube,
			nc.name,
			nc.watchFactory.NodeInformer(),
			nc.watchFactory.LocalPodInformer(),
			informer.NewDefaultEventHandler,
			false,
		)
		if err != nil {
			return err
		}
		nc.wg.Add(1)
		go func() {
			defer nc.wg.Done()
			nodeController.Run(nc.stopChan)
		}()
	} else {
		// attempt to cleanup the possibly stale bridge
		_, stderr, err := util.RunOVSVsctl("--if-exists", "del-br", "br-ext")
		if err != nil {
			klog.Errorf("Deletion of bridge br-ext failed: %v (%v)", err, stderr)
		}
		_, stderr, err = util.RunOVSVsctl("--if-exists", "del-port", "br-int", "int")
		if err != nil {
			klog.Errorf("Deletion of port int on  br-int failed: %v (%v)", err, stderr)
		}
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("Reset of initial klog \"loglevel\" failed, err: %v", err)
	}

	// start management port controller
	err = nc.mgmtPortController.Start(nc.stopChan)
	if err != nil {
		return fmt.Errorf("failed to start management port controller: %w", err)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		// Start the health checking server used by egressip, if EgressIPNodeHealthCheckPort is specified
		if err := nc.startEgressIPHealthCheckingServer(nc.mgmtPortController); err != nil {
			return err
		}
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// If interconnect is disabled OR interconnect is running in single-zone-mode,
		// the ovnkube-master is responsible for patching ICNI managed namespaces with
		// "k8s.ovn.org/external-gw-pod-ips". In that case, we need ovnkube-node to flush
		// conntrack on every node. In multi-zone-interconnect case, we will handle the flushing
		// directly on the ovnkube-controller code to avoid an extra namespace annotation
		if !config.OVNKubernetesFeature.EnableInterconnect || nc.sbZone == types.OvnDefaultZone {
			err := nc.WatchNamespaces()
			if err != nil {
				return fmt.Errorf("failed to watch namespaces: %w", err)
			}
			// every minute cleanup stale conntrack entries if any
			go wait.Until(func() {
				nc.checkAndDeleteStaleConntrackEntries()
			}, time.Minute*1, nc.stopChan)
		}
		err = nc.WatchEndpointSlices()
		if err != nil {
			return fmt.Errorf("failed to watch endpointSlices: %w", err)
		}
		err = nc.WatchNodes()
		if err != nil {
			return fmt.Errorf("failed to watch nodes: %w", err)
		}
	}

	if nc.healthzServer != nil {
		nc.healthzServer.Start(nc.stopChan, nc.wg)
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		if _, err := nc.watchPodsDPU(); err != nil {
			return err
		}
	} else {
		// start the cni server
		if nc.cniServer != nil {
			if err := nc.cniServer.Start(cni.ServerRunDir); err != nil {
				return err
			}
		}

		// Write CNI config file if it doesn't already exist
		if err := config.WriteCNIConfig(); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		wf := nc.watchFactory.(*factory.WatchFactory)
		c, err := egressservice.NewController(nc.stopChan, nodetypes.OvnKubeNodeSNATMark, nc.name,
			wf.EgressServiceInformer(), wf.ServiceInformer(), wf.EndpointSliceInformer())
		if err != nil {
			return err
		}
		if err = c.Run(nc.wg, 1); err != nil {
			return err
		}
	}
	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		if err = nc.apbExternalRouteNodeController.Run(nc.wg, 1); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressIP && !util.PlatformTypeIsEgressIPCloudProvider() {
		c, err := egressip.NewController(nc.Kube, nc.watchFactory.EgressIPInformer(), nc.watchFactory.NodeInformer(),
			nc.watchFactory.NamespaceInformer(), nc.watchFactory.PodCoreInformer(), nc.networkManager.GetActiveNetworkForNamespace,
			nc.routeManager, config.IPv4Mode, config.IPv6Mode, nc.name, nc.linkManager)
		if err != nil {
			return fmt.Errorf("failed to create egress IP controller: %v", err)
		}
		if err = c.Run(nc.stopChan, nc.wg, 1); err != nil {
			return fmt.Errorf("failed to run egress IP controller: %v", err)
		}
	} else {
		klog.Infof("Egress IP for secondary host network is disabled")
	}

	nc.linkManager.Run(nc.stopChan, nc.wg)

	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		ovspinning.Run(nc.stopChan)
	}()

	klog.Infof("Default node network controller initialized and ready.")
	return nil
}

// Stop gracefully stops the controller
// deleteLogicalEntities will never be true for default network
func (nc *DefaultNodeNetworkController) Stop() {
	close(nc.stopChan)
	nc.wg.Wait()
}

func (nc *DefaultNodeNetworkController) startEgressIPHealthCheckingServer(mgmtPort managementport.Interface) error {
	healthCheckPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
	if healthCheckPort == 0 {
		klog.Infof("Egress IP health check server skipped: no port specified")
		return nil
	}

	ifName := mgmtPort.GetInterfaceName()
	mgmtAddresses := mgmtPort.GetAddresses()
	if len(mgmtAddresses) == 0 {
		return fmt.Errorf("unable to start Egress IP health checking server on interface %s: no mgmt ip", ifName)
	}

	mgmtAddress := mgmtAddresses[0]
	if err := ip.SettleAddresses(ifName, 10); err != nil {
		return fmt.Errorf("failed to start Egress IP health checking server due to unsettled IPv6: %w on interface %s", err, ifName)
	}

	healthServer, err := healthcheck.NewEgressIPHealthServer(mgmtAddress.IP, healthCheckPort)
	if err != nil {
		return fmt.Errorf("unable to allocate health checking server: %v", err)
	}

	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		healthServer.Run(nc.stopChan)
	}()
	return nil
}

func (nc *DefaultNodeNetworkController) reconcileConntrackUponEndpointSliceEvents(oldEndpointSlice, newEndpointSlice *discovery.EndpointSlice) error {
	var errors []error
	if oldEndpointSlice == nil {
		// nothing to do upon an add event
		return nil
	}
	namespacedName, err := util.ServiceNamespacedNameFromEndpointSlice(oldEndpointSlice)
	if err != nil {
		return fmt.Errorf("cannot reconcile conntrack: %v", err)
	}
	svc, err := nc.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("error while retrieving service for endpointslice %s/%s when reconciling conntrack: %v",
			newEndpointSlice.Namespace, newEndpointSlice.Name, err)
	}
	for _, oldPort := range oldEndpointSlice.Ports {
		if *oldPort.Protocol != corev1.ProtocolUDP { // flush conntrack only for UDP
			continue
		}
		for _, oldEndpoint := range oldEndpointSlice.Endpoints {
			for _, oldIP := range oldEndpoint.Addresses {
				oldIPStr := utilnet.ParseIPSloppy(oldIP).String()
				// upon an update event, remove conntrack entries for IP addresses that are no longer
				// in the endpointslice, skip otherwise
				if newEndpointSlice != nil && util.DoesEndpointSliceContainEligibleEndpoint(newEndpointSlice, oldIPStr, *oldPort.Port, *oldPort.Protocol, svc) {
					continue
				}
				// upon update and delete events, flush conntrack only for UDP
				if err := util.DeleteConntrackServicePort(oldIPStr, *oldPort.Port, *oldPort.Protocol,
					netlink.ConntrackReplyAnyIP, nil); err != nil {
					klog.Errorf("Failed to delete conntrack entry for %s: %v", oldIPStr, err)
				}
			}
		}
	}
	return utilerrors.Join(errors...)

}
func (nc *DefaultNodeNetworkController) WatchEndpointSlices() error {
	if util.IsNetworkSegmentationSupportEnabled() {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		req, err := labels.NewRequirement(discovery.LabelServiceName, selection.Exists, nil)
		if err != nil {
			return err
		}
		_, err = nc.retryEndpointSlices.WatchResourceFiltered("", labels.NewSelector().Add(*req))
		return err
	}
	_, err := nc.retryEndpointSlices.WatchResource()
	return err
}

func exGatewayPodsAnnotationsChanged(oldNs, newNs *corev1.Namespace) bool {
	// In reality we only care about exgw pod deletions, however since the list of IPs is not expected to change
	// that often, let's check for *any* changes to these annotations compared to their previous state and trigger
	// the logic for checking if we need to delete any conntrack entries
	return (oldNs.Annotations[util.ExternalGatewayPodIPsAnnotation] != newNs.Annotations[util.ExternalGatewayPodIPsAnnotation]) ||
		(oldNs.Annotations[util.RoutingExternalGWsAnnotation] != newNs.Annotations[util.RoutingExternalGWsAnnotation])
}

func (nc *DefaultNodeNetworkController) checkAndDeleteStaleConntrackEntries() {
	namespaces, err := nc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Unable to get pods from informer: %v", err)
	}
	for _, namespace := range namespaces {
		_, foundRoutingExternalGWsAnnotation := namespace.Annotations[util.RoutingExternalGWsAnnotation]
		_, foundExternalGatewayPodIPsAnnotation := namespace.Annotations[util.ExternalGatewayPodIPsAnnotation]
		if foundRoutingExternalGWsAnnotation || foundExternalGatewayPodIPsAnnotation {
			pods, err := nc.watchFactory.GetPods(namespace.Name)
			if err != nil {
				klog.Warningf("Unable to get pods from informer for namespace %s: %v", namespace.Name, err)
			}
			if len(pods) > 0 || err != nil {
				// we only need to proceed if there is at least one pod in this namespace on this node
				// OR if we couldn't fetch the pods for some reason at this juncture
				_ = nc.syncConntrackForExternalGateways(namespace)
			}
		}
	}
}

func (nc *DefaultNodeNetworkController) syncConntrackForExternalGateways(newNs *corev1.Namespace) error {
	gatewayIPs, err := nc.apbExternalRouteNodeController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(newNs.Name)
	if err != nil {
		return fmt.Errorf("unable to retrieve gateway IPs for Admin Policy Based External Route objects: %w", err)
	}
	// loop through all the IPs on the annotations; ARP for their MACs and form an allowlist
	gatewayIPs = gatewayIPs.Insert(strings.Split(newNs.Annotations[util.ExternalGatewayPodIPsAnnotation], ",")...)
	gatewayIPs = gatewayIPs.Insert(strings.Split(newNs.Annotations[util.RoutingExternalGWsAnnotation], ",")...)

	return util.SyncConntrackForExternalGateways(gatewayIPs, nil, func() ([]*corev1.Pod, error) {
		return nc.watchFactory.GetPods(newNs.Name)
	})
}

func (nc *DefaultNodeNetworkController) WatchNamespaces() error {
	_, err := nc.retryNamespaces.WatchResource()
	return err
}

func (nc *DefaultNodeNetworkController) WatchNodes() error {
	_, err := nc.retryNodes.WatchResource()
	return err
}

// addOrUpdateNode handles creating flows or nftables rules for each node to handle PMTUD
func (nc *DefaultNodeNetworkController) addOrUpdateNode(node *corev1.Node) error {
	var nftElems []*knftables.Element
	var addrs []string

	// Use GetNodeAddresses to get all node IPs (including current node for openflow)
	ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
	if err != nil {
		return fmt.Errorf("failed to get node addresses for node %q: %w", node.Name, err)
	}

	// Process IPv4 addresses
	for _, nodeIP := range ipsv4 {
		addrs = append(addrs, nodeIP.String())
		klog.Infof("Adding remote node %q, IP: %s to PMTUD blocking rules", node.Name, nodeIP)
		// Only add to nftables if this is remote node
		if node.Name != nc.name {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		}
	}

	// Process IPv6 addresses
	for _, nodeIP := range ipsv6 {
		addrs = append(addrs, nodeIP.String())
		klog.Infof("Adding remote node %q, IP: %s to PMTUD blocking rules", node.Name, nodeIP)
		// Only add to nftables if this is remote node
		if node.Name != nc.name {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}

	gw := nc.Gateway.(*gateway)
	gw.openflowManager.updateBridgePMTUDFlowCache(getPMTUDKey(node.Name), addrs)

	if len(nftElems) > 0 {
		if err := nodenft.UpdateNFTElements(nftElems); err != nil {
			return fmt.Errorf("unable to update NFT elements for node %q, error: %w", node.Name, err)
		}
	}

	return nil
}

func removePMTUDNodeNFTRules(nodeIPs []net.IP) error {
	var nftElems []*knftables.Element
	for _, nodeIP := range nodeIPs {
		// Remove IPs from NFT sets
		if utilnet.IsIPv4(nodeIP) {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		} else {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}
	if len(nftElems) > 0 {
		if err := nodenft.DeleteNFTElements(nftElems); err != nil {
			return err
		}
	}
	return nil
}

func (nc *DefaultNodeNetworkController) deleteNode(node *corev1.Node) {
	gw := nc.Gateway.(*gateway)
	gw.openflowManager.deleteFlowsByKey(getPMTUDKey(node.Name))

	// Use GetNodeAddresses to get node IPs
	ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
	if err != nil {
		klog.Errorf("Failed to get node addresses for node %q: %v", node.Name, err)
		return
	}

	ipsToRemove := make([]net.IP, 0, len(ipsv4)+len(ipsv6))
	ipsToRemove = append(ipsToRemove, ipsv4...)
	ipsToRemove = append(ipsToRemove, ipsv6...)

	klog.Infof("Deleting NFT elements for node: %s", node.Name)
	if err := removePMTUDNodeNFTRules(ipsToRemove); err != nil {
		klog.Errorf("Failed to delete nftables rules for PMTUD blocking for node %q: %v", node.Name, err)
	}
}

func (nc *DefaultNodeNetworkController) syncNodes(objs []interface{}) error {
	var keepNFTSetElemsV4, keepNFTSetElemsV6 []*knftables.Element
	var errors []error
	klog.Infof("Starting node controller node sync")
	start := time.Now()
	for _, obj := range objs {
		node, ok := obj.(*corev1.Node)
		if !ok {
			klog.Errorf("Spurious object in syncNodes: %v", obj)
			continue
		}
		if node.Name == nc.name {
			continue
		}

		// Use GetNodeAddresses to get node IPs
		ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
		if err != nil {
			klog.Errorf("Failed to get node addresses for node %q: %v", node.Name, err)
			continue
		}

		// Process IPv4 addresses
		for _, nodeIP := range ipsv4 {
			keepNFTSetElemsV4 = append(keepNFTSetElemsV4, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		}

		// Process IPv6 addresses
		for _, nodeIP := range ipsv6 {
			keepNFTSetElemsV6 = append(keepNFTSetElemsV6, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}
	if err := recreateNFTSet(types.NFTRemoteNodeIPsv4, keepNFTSetElemsV4); err != nil {
		errors = append(errors, err)
	}
	if err := recreateNFTSet(types.NFTRemoteNodeIPsv6, keepNFTSetElemsV6); err != nil {
		errors = append(errors, err)
	}

	klog.Infof("Node controller node sync done. Time taken: %s", time.Since(start))
	return utilerrors.Join(errors...)
}

// validateVTEPInterfaceMTU checks if the MTU of the interface that has ovn-encap-ip is big
// enough to carry the `config.Default.MTU` and the Geneve header. If the MTU is not big
// enough, it will return an error
func (nc *DefaultNodeNetworkController) validateVTEPInterfaceMTU() error {
	// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma
	ovnEncapIps := strings.Split(config.Default.EffectiveEncapIP, ",")
	for _, ip := range ovnEncapIps {
		ovnEncapIP := net.ParseIP(strings.TrimSpace(ip))
		if ovnEncapIP == nil {
			return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIP, config.Default.EffectiveEncapIP)
		}
		interfaceName, mtu, err := util.GetIFNameAndMTUForAddress(ovnEncapIP)
		if err != nil {
			return fmt.Errorf("could not get MTU for the interface with address %s: %w", ovnEncapIP, err)
		}

		// calc required MTU
		var requiredMTU int
		if config.Gateway.SingleNode {
			requiredMTU = config.Default.MTU
		} else {
			if config.IPv4Mode && !config.IPv6Mode {
				// we run in single-stack IPv4 only
				requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv4
			} else {
				// we run in single-stack IPv6 or dual-stack mode
				requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv6
			}
		}

		if mtu < requiredMTU {
			return fmt.Errorf("MTU (%d) of network interface %s is too small for specified overlay MTU (%d)",
				mtu, interfaceName, requiredMTU)
		}
		klog.V(2).Infof("MTU (%d) of network interface %s is big enough to deal with Geneve header overhead (sum %d). ",
			mtu, interfaceName, requiredMTU)
	}
	return nil
}

func getPMTUDKey(nodeName string) string {
	return fmt.Sprintf("%s_pmtud", nodeName)
}

// DummyNextHopIPs returns the fake next hops used for service traffic routing.
// It is used in:
// - br-ex, where we don't really care about the next hop GW in use as traffic is always routed to OVN
// - OVN, only when there is no default GW as it wouldn't matter since there is no external traffic
func DummyNextHopIPs() []net.IP {
	var nextHops []net.IP
	if config.IPv4Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP)
	}
	if config.IPv6Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP)
	}
	return nextHops
}

// DummyMasqueradeIPs returns the fake host masquerade IPs used for service traffic routing.
// It is used in: br-ex, where we SNAT the traffic destined towards a service IP
func DummyMasqueradeIPs() []net.IP {
	var nextHops []net.IP
	if config.IPv4Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP)
	}
	if config.IPv6Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)
	}
	return nextHops
}

// configureGlobalForwarding configures the global forwarding settings.
// It sets the FORWARD policy to DROP/ACCEPT based on the config.Gateway.DisableForwarding value for all enabled IP families.
// For IPv6 it additionally always enables the global forwarding.
func configureGlobalForwarding() error {
	// Global forwarding works differently for IPv6:
	//   conf/all/forwarding - BOOLEAN
	//    Enable global IPv6 forwarding between all interfaces.
	//	  IPv4 and IPv6 work differently here; e.g. netfilter must be used
	//	  to control which interfaces may forward packets and which not.
	// https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt
	//
	// It is not possible to configure the IPv6 forwarding per interface by
	// setting the net.ipv6.conf.<ifname>.forwarding sysctl. Instead,
	// the opposite approach is required where the global forwarding
	// is enabled and an iptables rule is added to restrict it by default.
	if config.IPv6Mode {
		if err := ip.EnableIP6Forward(); err != nil {
			return fmt.Errorf("could not set the correct global forwarding value for ipv6:  %w", err)
		}

	}

	for _, proto := range clusterIPTablesProtocols() {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return fmt.Errorf("failed to get the iptables helper: %w", err)
		}

		target := "ACCEPT"
		if config.Gateway.DisableForwarding {
			target = "DROP"

		}
		if err := ipt.ChangePolicy("filter", "FORWARD", target); err != nil {
			return fmt.Errorf("failed to change the forward policy to %q: %w", target, err)
		}
	}
	return nil
}
