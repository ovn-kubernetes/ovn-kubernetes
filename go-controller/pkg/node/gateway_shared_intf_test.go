//go:build linux
// +build linux

package node

import (
	"fmt"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Note: Local mocks are used instead of FakeNetworkManager to test specific error conditions
// (NotFound, InvalidPrimaryNetworkError) from GetActiveNetworkForNamespace. FakeNetworkManager
// doesn't support error injection. And the tests here are not dependent on the methods that
// FakeNetworkManager implements. If more node tests need this, we will enhance FakeNetworkManager.

// mockNetworkManagerWithNamespaceNotFoundError simulates namespace deletion race condition
type mockNetworkManagerWithNamespaceNotFoundError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithNamespaceNotFoundError) GetPrimaryNADForNamespace(_ string) (string, error) {
	// Simulate namespace deletion: no primary NAD by definition.
	return "", nil
}

func (m *mockNetworkManagerWithNamespaceNotFoundError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	notFoundErr := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, namespace)
	return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, notFoundErr)
}

// mockNetworkManagerWithInvalidPrimaryNetworkError simulates UDN deletion scenario
type mockNetworkManagerWithInvalidPrimaryNetworkError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkError) GetPrimaryNADForNamespace(_ string) (string, error) {
	// just a trigger to ensure GetActiveNetworkForNamespace gets called
	return types.DefaultNetworkName, nil
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

// mockNetworkManagerWithError tests that non-graceful errors are properly propagated
type mockNetworkManagerWithError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithError) GetPrimaryNADForNamespace(_ string) (string, error) {
	// just a trigger to ensure GetActiveNetworkForNamespace gets called
	return types.DefaultNetworkName, nil
}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("network lookup failed for namespace %q", namespace)
}

// mockNetworkManagerWithUnprocessedActiveNetworkError simulates a namespace that
// requires a UDN but the primary NAD has not been processed yet.
type mockNetworkManagerWithUnprocessedActiveNetworkError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithUnprocessedActiveNetworkError) GetPrimaryNADForNamespace(namespace string) (string, error) {
	return "", util.NewUnprocessedActiveNetworkError(namespace, "")
}

// mockNetworkManagerWithInactiveNode simulates a UDN where the node is inactive for the network.
type mockNetworkManagerWithInactiveNode struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInactiveNode) GetPrimaryNADForNamespace(_ string) (string, error) {
	return "test-namespace/test-nad", nil
}

func (m *mockNetworkManagerWithInactiveNode) GetNetworkNameForNADKey(_ string) string {
	return "test-udn"
}

func (m *mockNetworkManagerWithInactiveNode) NodeHasNetwork(_, _ string) bool {
	return false
}

func (m *mockNetworkManagerWithInactiveNode) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("unexpected GetActiveNetworkForNamespace call for %q", namespace)
}

// mockNetworkManagerWithActiveUDN simulates a UDN active on this node.
type mockNetworkManagerWithActiveUDN struct {
	networkmanager.Interface
	netInfo util.NetInfo
}

func (m *mockNetworkManagerWithActiveUDN) GetPrimaryNADForNamespace(_ string) (string, error) {
	return "test-namespace/test-nad", nil
}

func (m *mockNetworkManagerWithActiveUDN) GetNetworkNameForNADKey(_ string) string {
	return m.netInfo.GetNetworkName()
}

func (m *mockNetworkManagerWithActiveUDN) NodeHasNetwork(_, _ string) bool {
	return true
}

func (m *mockNetworkManagerWithActiveUDN) GetActiveNetworkForNamespace(_ string) (util.NetInfo, error) {
	return m.netInfo, nil
}

// verifyIPTablesRule checks if an iptables rule exists and asserts the expected state
func verifyIPTablesRule(ipt util.IPTablesHelper, serviceIP string, servicePort, nodePort int32, shouldExist bool, message string) {
	exists, err := ipt.Exists("nat", "OVN-KUBE-NODEPORT",
		"-p", "TCP", "-m", "addrtype", "--dst-type", "LOCAL",
		"--dport", fmt.Sprintf("%d", nodePort), "-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", serviceIP, servicePort))
	Expect(err).NotTo(HaveOccurred())
	if shouldExist {
		Expect(exists).To(BeTrue(), message)
	} else {
		Expect(exists).To(BeFalse(), message)
	}
}

// setupServiceAndEndpointSliceWithRules creates a service and endpoint slice, adds them to npw,
// and verifies iptables rules are created. Returns the created endpoint slice.
func setupServiceAndEndpointSliceWithRules(npw *nodePortWatcher, ipt util.IPTablesHelper, svcName, namespace, serviceIP, endpointIP string, servicePort, nodePort int32, annotations map[string]string) *discovery.EndpointSlice {
	// Create service
	service := newService(svcName, namespace, serviceIP,
		[]corev1.ServicePort{{
			Name:       "http",
			Protocol:   corev1.ProtocolTCP,
			Port:       servicePort,
			TargetPort: intstr.FromInt(int(servicePort) + 8000), // e.g., 80 -> 8080
			NodePort:   nodePort,
		}},
		corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

	// Create endpoint slice with endpoints
	epPortName := "http"
	epPortValue := servicePort + 8000 // Match targetPort
	epPortProtocol := corev1.ProtocolTCP
	epSlice := newEndpointSlice(
		svcName,
		namespace,
		[]discovery.Endpoint{
			{
				Addresses: []string{endpointIP},
			},
		},
		[]discovery.EndpointPort{
			{
				Name:     &epPortName,
				Protocol: &epPortProtocol,
				Port:     &epPortValue,
			},
		},
	)

	// Apply annotations if provided
	if len(annotations) > 0 {
		if epSlice.Annotations == nil {
			epSlice.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			epSlice.Annotations[k] = v
		}
	}

	// Add service and endpoint slice
	err := npw.AddService(service)
	Expect(err).NotTo(HaveOccurred())

	err = npw.AddEndpointSlice(epSlice)
	Expect(err).NotTo(HaveOccurred())

	// Verify iptables rules were created
	verifyIPTablesRule(ipt, serviceIP, servicePort, nodePort, true, "iptables rule should exist before deletion")

	return epSlice
}

var _ = Describe("DeleteEndpointSlice", func() {
	var (
		fakeClient *util.OVNNodeClientset
		watcher    *factory.WatchFactory
		npw        *nodePortWatcher
		iptV4      util.IPTablesHelper
		iptV6      util.IPTablesHelper
	)

	const (
		nodeName      = "test-node"
		testNamespace = "test-namespace"
		testService   = "test-service"
	)

	BeforeEach(func() {
		var err error
		// Restore global default values before each test
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.Gateway.Mode = config.GatewayModeLocal
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeClient = &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(),
		}
		fakeClient.AdminPolicyRouteClient = adminpolicybasedrouteclient.NewSimpleClientset()
		fakeClient.NetworkAttchDefClient = nadfake.NewSimpleClientset()
		fakeClient.UserDefinedNetworkClient = udnfakeclient.NewSimpleClientset()

		watcher, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = watcher.Start()
		Expect(err).NotTo(HaveOccurred())

		// Initialize nodePortWatcher with default network manager
		iptV4, iptV6 = util.SetFakeIPTablesHelpers()
		npw = initFakeNodePortWatcher(iptV4, iptV6)
		npw.watchFactory = watcher
		npw.networkManager = networkmanager.Default().Interface()

		// Initialize nodeIPManager (required for GetLocalEligibleEndpointAddresses)
		k := &kube.Kube{KClient: fakeClient.KubeClient}
		npw.nodeIPManager = newAddressManagerInternal(nodeName, k, nil, watcher, nil, false)
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	Context("when UDN is deleted before processing endpoint slice", func() {
		It("should execute delServiceRules and gracefully skip addServiceRules", func() {
			// Setup service and endpoint slice with iptables rules
			// Add UDN annotation to simulate a mirrored UDN EndpointSlice
			epSlice := setupServiceAndEndpointSliceWithRules(npw, iptV4, testService, testNamespace, "10.96.0.2", "10.244.0.2", 80, 30081,
				map[string]string{types.UserDefinedNetworkEndpointSliceAnnotation: "test-udn"})

			// Replace network manager with one that returns InvalidPrimaryNetworkError
			// This simulates UDN deletion scenario
			npw.networkManager = &mockNetworkManagerWithInvalidPrimaryNetworkError{}

			// Call DeleteEndpointSlice - should not return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should gracefully handle UDN deletion (no error)
			Expect(err).NotTo(HaveOccurred())

			// iptables rules should be deleted even when UDN is deleted
			verifyIPTablesRule(iptV4, "10.96.0.2", 80, 30081, false, "iptables rule should be deleted even when UDN is deleted")
		})
	})

	Context("when network lookup returns other errors", func() {
		It("should execute delServiceRules but return error from network lookup", func() {
			// Setup service and endpoint slice with iptables rules
			epSlice := setupServiceAndEndpointSliceWithRules(npw, iptV4, testService, testNamespace, "10.96.0.3", "10.244.0.3", 80, 30082, nil)

			// Replace network manager with one that returns a generic error
			npw.networkManager = &mockNetworkManagerWithError{}

			// Call DeleteEndpointSlice - should return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return error for other types of failures
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error getting active network"))
			Expect(err.Error()).To(ContainSubstring(testNamespace))
			Expect(err.Error()).To(ContainSubstring(testService))

			// iptables rules should still be deleted even when error is returned
			verifyIPTablesRule(iptV4, "10.96.0.3", 80, 30082, false, "iptables rule should be deleted even when error occurs")
		})
	})

	Context("when service does not exist in cache", func() {
		It("should return nil without error", func() {
			// Create endpoint slice (but no service in cache)
			epSlice := newEndpointSlice(testService, testNamespace, nil, nil)

			// Call DeleteEndpointSlice when service not in cache
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return nil (no-op when not in cache)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("when namespace is deleted before processing endpoint slice", func() {
		It("should clean up old rules even when namespace is gone", func() {
			// Setup service and endpoint slice with iptables rules
			epSlice := setupServiceAndEndpointSliceWithRules(npw, iptV4, testService, testNamespace, "10.96.0.10", "10.244.0.5", 80, 30090, nil)

			// Simulate namespace not found error
			npw.networkManager = &mockNetworkManagerWithNamespaceNotFoundError{}
			err := npw.DeleteEndpointSlice(epSlice)
			// Verify no error (graceful handling)
			Expect(err).NotTo(HaveOccurred())

			// iptables rules should be deleted even though namespace lookup failed
			verifyIPTablesRule(iptV4, "10.96.0.10", 80, 30090, false, "iptables rule should be deleted even when namespace lookup fails")
		})
	})
})

var _ = Describe("SyncServices", func() {
	var (
		fakeClient *util.OVNNodeClientset
		watcher    *factory.WatchFactory
		npw        *nodePortWatcher
		iptV4      util.IPTablesHelper
		iptV6      util.IPTablesHelper
	)

	const (
		nodeName      = "test-node"
		testNamespace = "test-namespace"
		testService   = "test-service"
	)

	BeforeEach(func() {
		var err error
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.Gateway.Mode = config.GatewayModeLocal
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeClient = &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(),
		}
		fakeClient.AdminPolicyRouteClient = adminpolicybasedrouteclient.NewSimpleClientset()
		fakeClient.NetworkAttchDefClient = nadfake.NewSimpleClientset()
		fakeClient.UserDefinedNetworkClient = udnfakeclient.NewSimpleClientset()

		watcher, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = watcher.Start()
		Expect(err).NotTo(HaveOccurred())

		iptV4, iptV6 = util.SetFakeIPTablesHelpers()
		npw = initFakeNodePortWatcher(iptV4, iptV6)
		npw.watchFactory = watcher
		npw.networkManager = networkmanager.Default().Interface()

		k := &kube.Kube{KClient: fakeClient.KubeClient}
		npw.nodeIPManager = newAddressManagerInternal(nodeName, k, nil, watcher, nil, false)
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	Context("when namespace requires UDN but NAD is unprocessed", func() {
		It("should skip service sync without failing startup", func() {
			service := newService(testService, testNamespace, "10.96.0.20",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30091,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.networkManager = &mockNetworkManagerWithUnprocessedActiveNetworkError{}

			err := npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyIPTablesRule(iptV4, "10.96.0.20", 80, 30091, false,
				"iptables rule should not be created when UDN is unprocessed")
		})
	})

	Context("when UDN is inactive on this node", func() {
		It("should skip service sync without installing rules", func() {
			service := newService(testService, testNamespace, "10.96.0.30",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30092,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.networkManager = &mockNetworkManagerWithInactiveNode{}

			err := npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyIPTablesRule(iptV4, "10.96.0.30", 80, 30092, false,
				"iptables rule should not be created when UDN is inactive on this node")
		})
	})

	Context("when UDN is active on this node", func() {
		It("should install nodeport rules", func() {
			// Avoid openflow dependency in this test.
			config.Gateway.AllowNoUplink = true
			npw.ofportPhys = ""

			service := newService(testService, testNamespace, "10.96.0.40",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30093,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			nad := ovntest.GenerateNAD("test-udn", "test-nad", testNamespace, types.Layer3Topology, "10.1.0.0/16", types.NetworkRolePrimary)
			netInfo, err := util.ParseNADInfo(nad)
			Expect(err).NotTo(HaveOccurred())
			npw.networkManager = &mockNetworkManagerWithActiveUDN{netInfo: netInfo}

			nodeName := npw.nodeIPManager.nodeName
			epPortName := "http"
			epPortValue := int32(8080)
			epPortProtocol := corev1.ProtocolTCP
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "ab23",
					Namespace: testNamespace,
					Labels: map[string]string{
						types.LabelUserDefinedServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: netInfo.GetNetworkName(),
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{{
					Addresses: []string{"10.244.0.9"},
					NodeName:  &nodeName,
				}},
				Ports: []discovery.EndpointPort{{
					Name:     &epPortName,
					Protocol: &epPortProtocol,
					Port:     &epPortValue,
				}},
			}
			Expect(watcher.EndpointSliceInformer().GetStore().Add(epSlice)).To(Succeed())

			err = npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyIPTablesRule(iptV4, "10.96.0.40", 80, 30093, true,
				"iptables rule should be created when UDN is active on this node")
		})
	})
})
