//go:build linux
// +build linux

package node

import (
	"fmt"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
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

func (m *mockNetworkManagerWithNamespaceNotFoundError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	notFoundErr := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, namespace)
	return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, notFoundErr)
}

// mockNetworkManagerWithInvalidPrimaryNetworkError simulates UDN deletion scenario
type mockNetworkManagerWithInvalidPrimaryNetworkError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

// mockNetworkManagerWithError tests that non-graceful errors are properly propagated
type mockNetworkManagerWithError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("network lookup failed for namespace %q", namespace)
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
			KubeClient: fake.NewClientset(),
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
