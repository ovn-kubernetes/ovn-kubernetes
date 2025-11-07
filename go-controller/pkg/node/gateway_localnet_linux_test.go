package node

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	v4localnetGatewayIP = "10.244.0.1"
	v6localnetGatewayIP = "fd00:96:1::1"
	gwMAC               = "0a:0b:0c:0d:0e:0f"
	linkName            = "breth0"
)

func initFakeNodePortWatcher(iptV4, iptV6 util.IPTablesHelper) *nodePortWatcher {
	initIPTable := map[string]util.FakeTable{
		"nat":    {},
		"filter": {},
		"mangle": {},
	}

	f4 := iptV4.(*util.FakeIPTables)
	err := f4.MatchState(initIPTable, nil)
	Expect(err).NotTo(HaveOccurred())

	f6 := iptV6.(*util.FakeIPTables)
	err = f6.MatchState(initIPTable, nil)
	Expect(err).NotTo(HaveOccurred())

	gwMACParsed, _ := net.ParseMAC(gwMAC)

	defaultBridge := bridgeconfig.TestDefaultBridgeConfig()
	defaultBridge.SetMAC(gwMACParsed)

	fNPW := nodePortWatcher{
		ofportPhys:  "eth0",
		gatewayIPv4: v4localnetGatewayIP,
		gatewayIPv6: v6localnetGatewayIP,
		serviceInfo: make(map[k8stypes.NamespacedName]*serviceConfig),
		ofm: &openflowManager{
			flowCache:     map[string][]string{},
			defaultBridge: defaultBridge,
		},
		networkManager: networkmanager.Default().Interface(),
		gwBridge:       bridgeconfig.TestBridgeConfig(""),
	}
	return &fNPW
}

func startNodePortWatcher(n *nodePortWatcher, fakeClient *util.OVNNodeClientset) error {
	if err := initLocalGatewayIPTables(); err != nil {
		return err
	}

	k := &kube.Kube{KClient: fakeClient.KubeClient}
	n.nodeIPManager = newAddressManagerInternal(fakeNodeName, k, nil, n.watchFactory, nil, false)
	localHostNetEp := "192.168.18.15/32"
	ip, ipnet, _ := net.ParseCIDR(localHostNetEp)
	ipFullNet := net.IPNet{IP: ip, Mask: ipnet.Mask}
	n.nodeIPManager.cidrs.Insert(ipFullNet.String())

	// Add or delete iptables rules from FORWARD chain based on DisableForwarding. This is
	// to imitate addition or deletion of iptales rules done in newNodePortWatcher().
	var subnets []*net.IPNet
	for _, subnet := range config.Default.ClusterSubnets {
		subnets = append(subnets, subnet.CIDR)
	}
	subnets = append(subnets, config.Kubernetes.ServiceCIDRs...)
	if config.Gateway.DisableForwarding {
		if err := initExternalBridgeServiceForwardingRules(subnets); err != nil {
			return fmt.Errorf("failed to add accept rules in forwarding table for bridge %s: err %v", linkName, err)
		}
	} else {
		if err := delExternalBridgeServiceForwardingRules(subnets); err != nil {
			return fmt.Errorf("failed to delete accept rules in forwarding table for bridge %s: err %v", linkName, err)
		}
	}

	// set up a controller to handle events on services to mock the nodeportwatcher bits
	// in gateway.go and trigger code in gateway_shared_intf.go
	_, err := n.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			Expect(n.AddService(svc)).To(Succeed())
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*corev1.Service)
			newSvc := new.(*corev1.Service)
			Expect(n.UpdateService(oldSvc, newSvc)).To(Succeed())
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			Expect(n.DeleteService(svc)).To(Succeed())
		},
	}, n.SyncServices)

	return err
}

func startNodePortWatcherWithRetry(n *nodePortWatcher, fakeClient *util.OVNNodeClientset, stopChan chan struct{}, wg *sync.WaitGroup) (*retry.RetryFramework, error) {
	if err := initLocalGatewayIPTables(); err != nil {
		return nil, err
	}

	k := &kube.Kube{KClient: fakeClient.KubeClient}
	n.nodeIPManager = newAddressManagerInternal(fakeNodeName, k, nil, n.watchFactory, nil, false)
	localHostNetEp := "192.168.18.15/32"
	ip, ipnet, _ := net.ParseCIDR(localHostNetEp)
	ipFullNet := net.IPNet{IP: ip, Mask: ipnet.Mask}
	n.nodeIPManager.cidrs.Insert(ipFullNet.String())

	nodePortWatcherRetry := n.newRetryFrameworkForTests(factory.ServiceForFakeNodePortWatcherType, stopChan, wg)
	if _, err := nodePortWatcherRetry.WatchResource(); err != nil {
		return nil, fmt.Errorf("failed to start watching services with retry framework: %v", err)
	}
	return nodePortWatcherRetry, nil
}

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       k8stypes.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
		Annotations: map[string]string{},
	}
}

func newService(name, namespace, ip string, ports []corev1.ServicePort, serviceType corev1.ServiceType,
	externalIPs []string, serviceStatus corev1.ServiceStatus, isETPLocal, isITPLocal bool) *corev1.Service {
	externalTrafficPolicy := corev1.ServiceExternalTrafficPolicyTypeCluster
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	if isETPLocal {
		externalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
	}
	if isITPLocal {
		internalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
	}
	return &corev1.Service{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: corev1.ServiceSpec{
			ClusterIP:             ip,
			ClusterIPs:            []string{ip},
			Ports:                 ports,
			Type:                  serviceType,
			ExternalIPs:           externalIPs,
			ExternalTrafficPolicy: externalTrafficPolicy,
			InternalTrafficPolicy: &internalTrafficPolicy,
		},
		Status: serviceStatus,
	}
}

func newServiceWithoutNodePortAllocation(name, namespace, ip string, ports []corev1.ServicePort, serviceType corev1.ServiceType,
	externalIPs []string, serviceStatus corev1.ServiceStatus, isETPLocal, isITPLocal bool) *corev1.Service {
	doNotAllocateNodePorts := false
	service := newService(name, namespace, ip, ports, serviceType, externalIPs, serviceStatus, isETPLocal, isITPLocal)
	service.Spec.AllocateLoadBalancerNodePorts = &doNotAllocateNodePorts
	return service
}

func newEndpointSlice(svcName, namespace string, endpoints []discovery.Endpoint, endpointPort []discovery.EndpointPort) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName + "ab23",
			Namespace: namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svcName},
		},
		Ports:       endpointPort,
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

func makeConntrackFilter(ip string, port int, protocol corev1.Protocol) *netlink.ConntrackFilter {
	filter := &netlink.ConntrackFilter{}

	var err error
	if protocol == corev1.ProtocolUDP {
		err = filter.AddProtocol(17)
	} else if protocol == corev1.ProtocolTCP {
		err = filter.AddProtocol(6)
	} else if protocol == corev1.ProtocolSCTP {
		err = filter.AddProtocol(132)
	}
	Expect(err).NotTo(HaveOccurred())

	if port > 0 {
		err = filter.AddPort(netlink.ConntrackOrigDstPort, uint16(port))
		Expect(err).NotTo(HaveOccurred())
	}
	ipAddress := net.ParseIP(ip)
	Expect(ipAddress).NotTo(BeNil())
	err = filter.AddIP(netlink.ConntrackOrigDstIP, ipAddress)
	Expect(err).NotTo(HaveOccurred())

	return filter
}

type ctFilterDesc struct {
	ip   string
	port int
}

func addConntrackMocks(nlMock *mocks.NetLinkOps, filterDescs []ctFilterDesc) {
	ctMocks := make([]ovntest.TestifyMockHelper, 0, len(filterDescs))
	for _, ctf := range filterDescs {
		ctMocks = append(ctMocks, ovntest.TestifyMockHelper{
			OnCallMethodName: "ConntrackDeleteFilters",
			OnCallMethodArgs: []interface{}{
				netlink.ConntrackTableType(netlink.ConntrackTable),
				netlink.InetFamily(netlink.FAMILY_V4),
				makeConntrackFilter(ctf.ip, ctf.port, corev1.ProtocolTCP),
			},
			RetArgList: []interface{}{uint(1), nil},
		})
	}
	ovntest.ProcessMockFnList(&nlMock.Mock, ctMocks)
}

/*
Note: all of the tests described below actually rely on OVNK node controller start up failing. This is
because no node is actually added when the controller is started, so node start up fails querying kapi for its
own node. This is either intentional or accidentally convenient as the node port watcher is then replaced with a fake
one and started again to exercise the tests.
*/
var _ = Describe("Node Operations", func() {
	var (
		app          *cli.App
		fExec        *ovntest.FakeExec
		iptV4, iptV6 util.IPTablesHelper
		nft          *knftables.Fake
		fNPW         *nodePortWatcher
		netlinkMock  *mocks.NetLinkOps

		nInitialFakeCommands int
	)

	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		netlinkMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkMock)

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		fExec = ovntest.NewFakeExec()
		err := util.SetExec(fExec)
		Expect(err).NotTo(HaveOccurred())
		nInitialFakeCommands = 1

		iptV4, iptV6 = util.SetFakeIPTablesHelpers()
		nft = nodenft.SetFakeNFTablesHelper()
		err = nft.ParseDump(getBaseNFTRules(types.K8sMgmtIntfName))
		Expect(err).NotTo(HaveOccurred())

		fNPW = initFakeNodePortWatcher(iptV4, iptV6)
	})

	AfterEach(func() {
		util.SetNetLinkOpMockInst(origNetlinkInst)
	})

	Context("on startup", func() {
		It("removes stale iptables rules while keeping remaining intact", func() {
			app.Action = func(*cli.Context) error {
				// Depending on the order of informer event processing the initial
				// Service might be "added" once or twice.  Take that into account.
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				externalIP := "1.1.1.1"
				externalIPPort := int32(8032)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIP},
					corev1.ServiceStatus{},
					false, false,
				)

				fakeRules := getExternalIPTRules(service.Spec.Ports[0], externalIP, service.Spec.ClusterIP, false, false)
				Expect(insertIptRules(fakeRules)).To(Succeed())
				fakeRules = getExternalIPTRules(
					corev1.ServicePort{
						Port:     27000,
						Protocol: corev1.ProtocolUDP,
						Name:     "This is going to dissapear I hope",
					},
					"10.10.10.10",
					"172.32.0.12",
					false,
					false,
				)
				Expect(insertIptRules(fakeRules)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							"-p UDP -d 10.10.10.10 --dport 27000 -j DNAT --to-destination 172.32.0.12:27000",
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Eventually(func() bool {
					return fExec.CalledMatchesExpectedAtLeastN(minNFakeCommands)
				}, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on add", func() {
		It("inits iptables rules with ExternalIP", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Port:     8032,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIP},
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with NodePort", func() {
			app.Action = func(*cli.Context) error {

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with NodePort where ETP=local, LGW", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with LoadBalancer", func() {
			app.Action = func(*cli.Context) error {
				// Depending on the order of informer event processing the initial
				// Service might be "added" once or twice.  Take that into account.
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				externalIP := "1.1.1.1"
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false,
				)
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Eventually(func() bool {
					return fExec.CalledMatchesExpectedAtLeastN(minNFakeCommands)
				}, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=local, LGW mode", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_8080")
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_8080")
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where AllocateLoadBalancerNodePorts=False, ETP=local, LGW mode", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				svcPortName := "http"
				service := *newServiceWithoutNodePortAllocation("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Name:       svcPortName,
							Protocol:   corev1.ProtocolTCP,
							Port:       int32(80),
							TargetPort: intstr.FromInt(int(int32(8080))),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
					NodeName:  &fakeNodeName,
				}
				otherNodeName := "node2"
				nonLocalEndpoint := discovery.Endpoint{
					Addresses: []string{"10.244.1.3"}, // is not picked since its not local to the node
					NodeName:  &otherNodeName,
				}
				ep2 := discovery.Endpoint{
					Addresses: []string{"10.244.0.4"},
					NodeName:  &fakeNodeName,
				}
				epPortValue := int32(8080)
				epPortProtocol := corev1.ProtocolTCP
				epPort1 := discovery.EndpointPort{
					Name:     &svcPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2, nonLocalEndpoint},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 0.5000000000", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 1.0000000000", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 0.5000000000", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 1.0000000000", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedLBIngressFlows := []string{
					"cookie=0xd8c1fe514f305bc1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x799e0efe5404e9a1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-services-v4 { %s . tcp . %d }\n", ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue()))
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-services-v4 { %s . tcp . %d }\n", ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue()))
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				Expect(fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_80")).To(Equal(expectedLBIngressFlows))
				Expect(fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_80")).To(Equal(expectedLBExternalIPFlows))
				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		})

		It("inits iptables rules and openflows with named port and AllocateLoadBalancerNodePorts=False, ETP=local, LGW mode", func() {
			app.Action = func(*cli.Context) error {
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				config.Gateway.Mode = config.GatewayModeLocal
				svcPortName := "https-port"
				svcPortValue := int32(8080)
				svcProtocol := corev1.ProtocolTCP
				svcTargetPortName := "https-target"
				svcAllocateLoadBalancerNodePorts := false
				svcStatusIP := "192.168.0.10"
				svcStatusIPMode := corev1.LoadBalancerIPModeVIP

				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP

				nodeName := "node"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Name:       svcPortName,
							Port:       svcPortValue,
							Protocol:   svcProtocol,
							TargetPort: intstr.FromString(svcTargetPortName),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					nil,
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{
									IP:     svcStatusIP,
									IPMode: &svcStatusIPMode,
								},
							},
						},
					},
					true, false,
				)
				service.Spec.AllocateLoadBalancerNodePorts = &svcAllocateLoadBalancerNodePorts
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
					NodeName:  &nodeName,
				}
				ep2 := discovery.Endpoint{
					Addresses: []string{"10.244.0.4"},
					NodeName:  &nodeName,
				}
				epPort1 := discovery.EndpointPort{
					Name:     &svcPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j DNAT --to-destination %s:%v",
								service.Spec.Ports[0].Protocol,
								service.Status.LoadBalancer.Ingress[0].IP,
								service.Spec.Ports[0].Port,
								service.Spec.ClusterIP,
								service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j DNAT --to-destination %s:%d -m statistic --mode random --probability 0.5000000000",
								service.Spec.Ports[0].Protocol,
								service.Status.LoadBalancer.Ingress[0].IP,
								service.Spec.Ports[0].Port,
								endpointSlice.Endpoints[0].Addresses[0],
								*endpointSlice.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %d -j DNAT --to-destination %s:%d -m statistic --mode random --probability 1.0000000000",
								service.Spec.Ports[0].Protocol,
								service.Status.LoadBalancer.Ingress[0].IP,
								service.Spec.Ports[0].Port,
								endpointSlice.Endpoints[1].Addresses[0],
								*endpointSlice.Ports[0].Port),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-services-v4 { %s . tcp . %v }\n"+
					"add element inet ovn-kubernetes mgmtport-no-snat-services-v4 { %s . tcp . %v }\n",
					endpointSlice.Endpoints[1].Addresses[0],
					*endpointSlice.Ports[0].Port,
					endpointSlice.Endpoints[0].Addresses[0],
					*endpointSlice.Ports[0].Port)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=cluster, LGW mode", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false, // ETP=cluster
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_8080")
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_8080")
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=local, SGW mode", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeShared
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()
				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedNodePortFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}
				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, icmp, nw_dst=5.5.5.5, icmp_type=3, icmp_code=4, actions=output:patch-breth0_ov",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, tcp, nw_dst=5.5.5.5, tp_dst=8080, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x10c6b89e483ea111, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, nw_src=5.5.5.5, tp_src=8080, actions=output:eth0",
						gwMAC),
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, icmp, nw_dst=1.1.1.1, icmp_type=3, icmp_code=4, actions=output:patch-breth0_ov",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, tcp, nw_dst=1.1.1.1, tp_dst=8080, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x71765945a31dc2f1, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, nw_src=1.1.1.1, tp_src=8080, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(Equal(expectedNodePortFlows))
				flows = fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_8080")
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_8080")
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with DualStack NodePort", func() {
			app.Action = func(*cli.Context) error {
				nodePort := int32(31111)

				fNPW.gatewayIPv6 = v6localnetGatewayIP

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)
				service.Spec.ClusterIPs = []string{"10.129.0.2", "fd00:10:96::10"}
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIPs[0], service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables4, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedTables6 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination [%s]:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIPs[1], service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules for ExternalIP with DualStack", func() {
			app.Action = func(*cli.Context) error {

				// Depending on the order of informer event processing the initial
				// Service might be "added" once or twice.  Take that into account.
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				externalIPv4 := "10.10.10.1"
				externalIPv6 := "fd00:96:1::1"
				clusterIPv4 := "10.129.0.2"
				clusterIPv6 := "fd00:10:96::10"
				fNPW.gatewayIPv6 = v6localnetGatewayIP

				service := *newService("service1", "namespace1", clusterIPv4,
					[]corev1.ServicePort{
						{
							Port:     8032,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIPv4, externalIPv6},
					corev1.ServiceStatus{},
					false, false,
				)
				service.Spec.ClusterIPs = []string{clusterIPv4, clusterIPv6}

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Eventually(func() bool {
					return fExec.CalledMatchesExpectedAtLeastN(minNFakeCommands)
				}, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIPv4, service.Spec.Ports[0].Port, clusterIPv4, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-ETP":      []string{},
						"OVN-KUBE-ITP":      []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables4, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedTables6 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination [%s]:%v", service.Spec.Ports[0].Protocol, externalIPv6, service.Spec.Ports[0].Port, clusterIPv6, service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on delete", func() {
		It("deletes iptables rules with ExternalIP", func() {
			app.Action = func(*cli.Context) error {
				// Depending on the order of informer event processing the initial
				// Service might be "added" once or twice.  Take that into account.
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				externalIP := "1.1.1.1"
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Port:     8032,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIP},
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()
				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"1.1.1.1", 8032}, {"10.129.0.2", 8032}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())
				Eventually(func() bool {
					return fExec.CalledMatchesExpectedAtLeastN(minNFakeCommands)
				}, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				Eventually(func() error {
					f6 := iptV6.(*util.FakeIPTables)
					return f6.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes iptables rules for NodePort", func() {
			app.Action = func(*cli.Context) error {
				nodePort := int32(31111)

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 0}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())
				Eventually(fExec.CalledMatchesExpected, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}

				Eventually(func() error {
					f6 := iptV6.(*util.FakeIPTables)
					return f6.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on update", func() {
		It("deletes conntrack entries for UDP ports when target port changes", func() {
			app.Action = func(*cli.Context) error {
				nodePort := int32(31111)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort:   nodePort,
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(53),
							TargetPort: intstr.FromInt(5353),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update service with new targetPort
				updatedService := service.DeepCopy()
				updatedService.Spec.Ports[0].TargetPort = intstr.FromInt(5454)

				// Atomic variable to track the number of conntrack delete filters calls
				var conntrackDeleteFiltersCount atomic.Int32

				// Mock conntrack deletion expectations
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter(service.Spec.ClusterIP, int(service.Spec.Ports[0].Port), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter("192.168.18.15", int(nodePort), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion was called
				Eventually(func() bool {
					return conntrackDeleteFiltersCount.Load() == 2
				}, "2s").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes conntrack entries for UDP ports when port changes", func() {
			app.Action = func(*cli.Context) error {
				nodePort := int32(31111)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort:   nodePort,
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(53),
							TargetPort: intstr.FromInt(5353),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update service with new port
				updatedService := service.DeepCopy()
				updatedService.Spec.Ports[0].Port = 54

				// Atomic variable to track the number of conntrack delete filters calls
				var conntrackDeleteFiltersCount atomic.Int32

				// Mock conntrack deletion expectations
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter(service.Spec.ClusterIP, int(service.Spec.Ports[0].Port), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter("192.168.18.15", int(nodePort), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion was called
				Eventually(func() bool {
					return conntrackDeleteFiltersCount.Load() == 2
				}, "2s").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not delete conntrack entries when TCP service target port changes", func() {
			app.Action = func(*cli.Context) error {
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Protocol:   corev1.ProtocolTCP,
							Port:       int32(80),
							TargetPort: intstr.FromInt(8080),
						},
					},
					corev1.ServiceTypeClusterIP,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update TCP service with new targetPort
				updatedService := service.DeepCopy()
				updatedService.Spec.Ports[0].TargetPort = intstr.FromInt(9090)

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion was not called
				Consistently(func() bool {
					netlinkMock.AssertNotCalled(GinkgoT(), "ConntrackDeleteFilters")
					return true
				}, "1s", "100ms").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes conntrack entries for UDP when ExternalIP changes", func() {
			app.Action = func(*cli.Context) error {
				externalIP1 := "1.1.1.1"
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(53),
							TargetPort: intstr.FromInt(5353),
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIP1},
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update service with new ExternalIP
				updatedService := service.DeepCopy()
				updatedService.Spec.ExternalIPs = []string{"2.2.2.2"}

				// Atomic variable to track the number of conntrack delete filters calls
				var conntrackDeleteFiltersCount atomic.Int32

				// Mock conntrack deletion for old ExternalIP
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter(externalIP1, int(service.Spec.Ports[0].Port), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion was called
				Eventually(func() bool {
					return conntrackDeleteFiltersCount.Load() == 1
				}, "2s").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes conntrack entries for UDP when LoadBalancer IP changes", func() {
			app.Action = func(*cli.Context) error {
				lbIP1 := "1.1.1.1"
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(53),
							TargetPort: intstr.FromInt(5353),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: lbIP1,
							}},
						},
					},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update service with new LoadBalancer IP
				updatedService := service.DeepCopy()
				updatedService.Status.LoadBalancer.Ingress[0].IP = "2.2.2.2"

				// Atomic variable to track the number of conntrack delete filters calls
				var conntrackDeleteFiltersCount atomic.Int32

				// Mock conntrack deletion for old LoadBalancer IP
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter(lbIP1, int(service.Spec.Ports[0].Port), corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion was called
				Eventually(func() bool {
					return conntrackDeleteFiltersCount.Load() == 1
				}, "2s").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes conntrack entries only for changed UDP ports in multi-port service", func() {
			app.Action = func(*cli.Context) error {
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(53),
							TargetPort: intstr.FromInt(5353),
						},
						{
							Protocol:   corev1.ProtocolUDP,
							Port:       int32(80),
							TargetPort: intstr.FromInt(8080),
						},
					},
					corev1.ServiceTypeClusterIP,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				// Update service - change only port 80's targetPort
				updatedService := service.DeepCopy()
				updatedService.Spec.Ports[1].TargetPort = intstr.FromInt(9090)

				// Atomic variable to track the number of conntrack delete filters calls
				var conntrackDeleteFiltersCount atomic.Int32

				// Mock conntrack deletion ONLY for port 80
				netlinkMock.
					On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(netlink.FAMILY_V4),
						makeConntrackFilter(service.Spec.ClusterIP, 80, corev1.ProtocolUDP)).
					Return(uint(1), nil).
					Run(func(_ mock.Arguments) {
						conntrackDeleteFiltersCount.Add(1)
					}).
					Once()

				// Update the service
				_, err = fakeClient.KubeClient.CoreV1().Services(service.Namespace).Update(
					context.Background(), updatedService, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				// Verify conntrack deletion called once for port 80 only
				Eventually(func() bool {
					return conntrackDeleteFiltersCount.Load() == 1
				}, "2s").Should(BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on add and delete", func() {
		It("manages iptables rules with ExternalIP", func() {
			app.Action = func(*cli.Context) error {
				// Depending on the order of informer event processing the initial
				// Service might be "added" once or twice.  Take that into account.
				minNFakeCommands := nInitialFakeCommands + 1
				fExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				}, minNFakeCommands)

				externalIP := "10.10.10.1"
				externalIPPort := int32(8034)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{externalIP},
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Eventually(func() bool {
					return fExec.CalledMatchesExpectedAtLeastN(minNFakeCommands)
				}, "2s").Should(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v",
								service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port,
								service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-ETP":      []string{},
						"OVN-KUBE-ITP":      []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}).Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}).Should(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.10.10.1", 8034}, {"10.129.0.2", 8034}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("check openflows for LoadBalancer and external ip are correctly added and removed where ETP=local, LGW mode", func() {
			app.Action = func(*cli.Context) error {
				externalIP := "1.1.1.1"
				externalIP2 := "1.1.1.2"
				config.Gateway.Mode = config.GatewayModeLocal
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeLoadBalancer,
					[]string{externalIP, externalIP2},
					corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows1 := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows2 := []string{
					"cookie=0x77df6d2c74c0a658, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.2, actions=output:LOCAL",
				}

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}).Should(BeNil())
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_8080")
				}).Should(Equal(expectedLBIngressFlows))
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_8080")
				}).Should(Equal(expectedLBExternalIPFlows1))
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.2_8080")
				}).Should(Equal(expectedLBExternalIPFlows2))

				addConntrackMocks(netlinkMock, []ctFilterDesc{
					{"1.1.1.1", 8080},
					{"1.1.1.2", 8080},
					{"5.5.5.5", 8080},
					{"192.168.18.15", 31111},
					{"10.129.0.2", 8080},
				})

				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("Ingress_namespace1_service1_5.5.5.5_8080")
				}, "2s").Should(BeNil())
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.1_8080")
				}, "2s").Should(BeNil())
				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("External_namespace1_service1_1.1.1.2_8080")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules with ExternalIP through retry logic", func() {
			app.Action = func(*cli.Context) error {
				var nodePortWatcherRetry *retry.RetryFramework
				var err error
				badExternalIP := "10.10.10.aa"
				goodExternalIP := "10.10.10.1"
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

				externalIPPort := int32(8034)
				service_ns := "namespace1"
				service_name := "service1"
				service := *newService(service_name, service_ns, "10.129.0.2",
					[]corev1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: corev1.ProtocolTCP,
						},
					},
					corev1.ServiceTypeClusterIP,
					[]string{badExternalIP}, // first use an incorrect IP
					corev1.ServiceStatus{},
					false, false,
				)

				wg := &sync.WaitGroup{}
				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset().GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wg.Wait()
					wf.Shutdown()
				}()

				By("starting node port watcher retry framework")
				fNPW.watchFactory = wf
				nodePortWatcherRetry, err = startNodePortWatcherWithRetry(
					fNPW, fakeClient, stopChan, wg)
				Expect(err).NotTo(HaveOccurred())
				Expect(nodePortWatcherRetry).NotTo(BeNil())

				Expect(fExec.CalledMatchesExpected()).To(BeFalse(), fExec.ErrorDesc) // no command is executed

				By("add service with incorrect external IP")
				_, err = fakeClient.KubeClient.CoreV1().Services(service_ns).Create(
					context.TODO(), &service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// expected ip tables with no external IP set
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				By("verify that a new retry entry for this service exists")
				key, err := retry.GetResourceKey(&service)
				Expect(err).NotTo(HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, nodePortWatcherRetry)
				// check iptables
				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				// HACK: Fix the service by setting a correct external IP address in newObj field
				// of the retry entry
				newObj := retry.GetNewObjFieldFromRetryObj(key, nodePortWatcherRetry)
				Expect(newObj).ToNot(BeNil())
				svc := newObj.(*corev1.Service)
				svc.Spec.ExternalIPs = []string{goodExternalIP}
				ok := retry.SetNewObjFieldInRetryObj(key, nodePortWatcherRetry, svc)
				Expect(ok).To(BeTrue())

				By("trigger immediate retry")
				retry.SetRetryObjWithNoBackoff(key, nodePortWatcherRetry)
				nodePortWatcherRetry.RequestRetryObjs()
				retry.CheckRetryObjectEventually(key, false, nodePortWatcherRetry) // entry should be gone

				// now expect ip tables to show the external IP
				ovn_kube_external_ip_field := []string{
					fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v",
						service.Spec.Ports[0].Protocol, goodExternalIP, service.Spec.Ports[0].Port,
						service.Spec.ClusterIP, service.Spec.Ports[0].Port)}
				expectedTables["nat"]["OVN-KUBE-EXTERNALIP"] = ovn_kube_external_ip_field
				Eventually(func(g Gomega) {
					f4 := iptV4.(*util.FakeIPTables)
					err = f4.MatchState(expectedTables, nil)
					g.Expect(err).NotTo(HaveOccurred())
				}).Should(Succeed())

				// TODO Make delete operation fail, check retry entry, run a successful delete
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules for NodePort", func() {
			app.Action = func(*cli.Context) error {
				nodePort := int32(38034)

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					false, false,
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, nodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}).Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}).Should(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 38034}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ETP=local, LGW", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				wg := &sync.WaitGroup{}
				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wg.Wait()
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				Expect(nodenft.MatchNFTRules(expectedNFT, nft.Dump())).To(Succeed())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(BeNil())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT = getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ETP=local, SGW", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				epPortName := "https"
				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				wg := &sync.WaitGroup{}
				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wg.Wait()
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					// default
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				Expect(nodenft.MatchNFTRules(expectedNFT, nft.Dump())).To(Succeed())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 = iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT = getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by local-host-networked pods where ETP=local, LGW", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				outport := int32(443)
				epPortName := "https"
				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Name:       epPortName,
							NodePort:   int32(31111),
							Protocol:   corev1.ProtocolTCP,
							Port:       int32(8080),
							TargetPort: intstr.FromInt(int(outport)),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"}, // host-networked endpoint local to this node
					NodeName:  &fakeNodeName,
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				// to ensure the endpoint is local-host-networked
				res := fNPW.nodeIPManager.cidrs.Has(fmt.Sprintf("%s/32", ep1.Addresses[0]))
				Expect(res).To(BeTrue())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=ct(commit,zone=64003,nat(dst=10.244.0.1:443),table=6)",
					"cookie=0xe745ecf105, priority=110, table=6, actions=output:LOCAL",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=LOCAL, tcp, tp_src=443, actions=ct(zone=64003 nat,table=7)",
					"cookie=0xe745ecf105, priority=110, table=7, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				Expect(nodenft.MatchNFTRules(expectedNFT, nft.Dump())).To(Succeed())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
						"OVN-KUBE-ETP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 = iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT = getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ITP=local and ETP=local", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				epPortName := "https"
				epPortValue := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: corev1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, true,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &epPortValue,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-ETP":        []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j MARK --set-xmark %s", service.Spec.Ports[0].Protocol, service.Spec.ClusterIP, service.Spec.Ports[0].Port, types.OVNKubeITPMark),
						},
					},
				}
				expectedFlows := []string{
					// default
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				expectedNFT += fmt.Sprintf("add element inet ovn-kubernetes mgmtport-no-snat-nodeports { tcp . %v }\n", service.Spec.Ports[0].NodePort)
				Expect(nodenft.MatchNFTRules(expectedNFT, nft.Dump())).To(Succeed())

				flows := fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-ITP":        []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ETP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 = iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT = getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by local-host-networked pods where ETP=local and ITP=local", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				outport := int32(443)
				epPortProtocol := corev1.ProtocolTCP
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]corev1.ServicePort{
						{
							Name:       epPortName,
							NodePort:   int32(31111),
							Protocol:   corev1.ProtocolTCP,
							Port:       int32(8080),
							TargetPort: intstr.FromInt(int(outport)),
						},
					},
					corev1.ServiceTypeNodePort,
					nil,
					corev1.ServiceStatus{},
					true, true,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"}, // host-networked endpoint local to this node
					NodeName:  &fakeNodeName,
				}
				epPort1 := discovery.EndpointPort{
					Name:     &epPortName,
					Port:     &outport,
					Protocol: &epPortProtocol,
				}
				// endpointSlice.Endpoints is host-networked so this will
				// come under hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1},
				)

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset(&service, &endpointSlice).GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				// to ensure the endpoint is local-host-networked
				res := fNPW.nodeIPManager.cidrs.Has(fmt.Sprintf("%s/32", endpointSlice.Endpoints[0].Addresses[0]))
				Expect(res).To(BeTrue())
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ITP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j REDIRECT --to-port %d", service.Spec.Ports[0].Protocol, service.Spec.ClusterIP, service.Spec.Ports[0].Port, int32(service.Spec.Ports[0].TargetPort.IntValue())),
						},
						"OVN-KUBE-ETP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=ct(commit,zone=64003,nat(dst=10.244.0.1:443),table=6)",
					"cookie=0xe745ecf105, priority=110, table=6, actions=output:LOCAL",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=LOCAL, tcp, tp_src=443, actions=ct(zone=64003 nat,table=7)",
					"cookie=0xe745ecf105, priority=110, table=7, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())

				expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
				Expect(nodenft.MatchNFTRules(expectedNFT, nft.Dump())).To(Succeed())

				Expect(fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				Expect(fakeClient.KubeClient.CoreV1().Services(service.Namespace).Delete(
					context.Background(), service.Name, metav1.DeleteOptions{})).To(Succeed())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
						"OVN-KUBE-ETP": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Eventually(func() error {
					f4 := iptV4.(*util.FakeIPTables)
					return f4.MatchState(expectedTables, nil)
				}, "2s").Should(Succeed())

				Eventually(func() error {
					expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
					return nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				}, "2s").Should(Succeed())

				Eventually(func() []string {
					return fNPW.ofm.getFlowsByKey("NodePort_namespace1_service1_tcp_31111")
				}, "2s").Should(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("disable-forwarding", func() {
		It("adds or removes iptables rules upon change in forwarding mode", func() {
			app.Action = func(*cli.Context) error {
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.1.0.0/16"), HostSubnetLength: 24}}
				config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("172.16.1.0/24")
				config.Gateway.DisableForwarding = true

				stopChan := make(chan struct{})
				fakeClient := util.GetOVNClientset().GetNodeClientset()
				wf, err := factory.NewNodeWatchFactory(fakeClient, "node")
				Expect(err).ToNot(HaveOccurred())
				Expect(wf.Start()).To(Succeed())
				defer func() {
					close(stopChan)
					wf.Shutdown()
				}()

				fNPW.watchFactory = wf
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {
						"FORWARD": []string{
							"-d 169.254.169.1 -j ACCEPT",
							"-s 169.254.169.1 -j ACCEPT",
							"-d 172.16.1.0/24 -j ACCEPT",
							"-s 172.16.1.0/24 -j ACCEPT",
							"-d 10.1.0.0/16 -j ACCEPT",
							"-s 10.1.0.0/16 -j ACCEPT",
						},
					},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				Expect(configureGlobalForwarding()).To(Succeed())
				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, map[util.FakePolicyKey]string{{
					Table: "filter",
					Chain: "FORWARD",
				}: "DROP"})
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				// Enable forwarding and test deletion of iptables rules from FORWARD chain
				config.Gateway.DisableForwarding = false
				fNPW.watchFactory = wf
				Expect(configureGlobalForwarding()).To(Succeed())
				Expect(startNodePortWatcher(fNPW, fakeClient)).To(Succeed())
				expectedTables = map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
					},
					"filter": {
						"FORWARD": []string{},
					},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, map[util.FakePolicyKey]string{{
					Table: "filter",
					Chain: "FORWARD",
				}: "ACCEPT"})
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 = iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

})
