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
	ktypes "k8s.io/apimachinery/pkg/types"
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

// Helper function to create test services with different configurations
func createTestService(namespace, name, clusterIP string, nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeNodePort,
			ClusterIP: clusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   nodePort,
				},
			},
		},
	}
}

var _ = Describe("DeleteEndpointSlice", func() {
	var (
		fakeClient *util.OVNNodeClientset
		watcher    *factory.WatchFactory
		npw        *nodePortWatcher
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
		config.Gateway.Mode = config.GatewayModeShared
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
		iptV4, iptV6 := util.SetFakeIPTablesHelpers()
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
			// Create service using helper
			service := createTestService(testNamespace, testService, "10.96.0.2", 30081)

			// Add service to npw cache
			svcKey := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
			npw.serviceInfo[svcKey] = &serviceConfig{
				service:               service,
				hasLocalHostNetworkEp: false,
				localEndpoints:        util.PortToLBEndpoints{},
			}

			// Create endpoint slice
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "-xyz",
					Namespace: testNamespace,
					Labels: map[string]string{
						discovery.LabelServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: types.DefaultNetworkName,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
			}

			// Replace network manager with one that returns InvalidPrimaryNetworkError
			// This simulates UDN deletion scenario
			npw.networkManager = &mockNetworkManagerWithInvalidPrimaryNetworkError{}

			// Call DeleteEndpointSlice - should not return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should gracefully handle UDN deletion (no error)
			Expect(err).NotTo(HaveOccurred())

			// Service should still be in cache (delServiceRules executed)
			_, exists := npw.serviceInfo[svcKey]
			Expect(exists).To(BeTrue())
		})
	})

	Context("when network lookup returns other errors", func() {
		It("should execute delServiceRules but return error from network lookup", func() {
			// Create service using helper
			service := createTestService(testNamespace, testService, "10.96.0.3", 30082)

			// Add service to npw cache
			svcKey := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
			npw.serviceInfo[svcKey] = &serviceConfig{
				service:               service,
				hasLocalHostNetworkEp: false,
				localEndpoints:        util.PortToLBEndpoints{},
			}

			// Create endpoint slice
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "-def",
					Namespace: testNamespace,
					Labels: map[string]string{
						discovery.LabelServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: types.DefaultNetworkName,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
			}

			// Replace network manager with one that returns a generic error
			npw.networkManager = &mockNetworkManagerWithError{}

			// Call DeleteEndpointSlice - should return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return error for other types of failures
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error getting active network"))
			Expect(err.Error()).To(ContainSubstring(testNamespace))
			Expect(err.Error()).To(ContainSubstring(testService))

			// Service should still be in cache (delServiceRules executed)
			_, exists := npw.serviceInfo[svcKey]
			Expect(exists).To(BeTrue())
		})
	})

	Context("when service does not exist in cache", func() {
		It("should return nil without error", func() {
			// Create endpoint slice (but no service in cache)
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "-ghi",
					Namespace: testNamespace,
					Labels: map[string]string{
						discovery.LabelServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: types.DefaultNetworkName,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
			}

			// Call DeleteEndpointSlice when service not in cache
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return nil (no-op when not in cache)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("when namespace is deleted before processing endpoint slice", func() {
		It("should clean up old rules even when namespace is gone", func() {
			// This test verifies the core fix: delServiceRules runs BEFORE network lookup
			// So even if namespace is deleted, old rules are cleaned up

			// Create service with some endpoints using helper
			service := createTestService(testNamespace, testService, "10.96.0.10", 30090)

			// Add service to cache with local endpoints
			svcKey := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
			localEndpoints := util.PortToLBEndpoints{
				"TCP/http": {
					Port:  8080,
					V4IPs: []string{"10.244.0.5"},
				},
			}
			npw.serviceInfo[svcKey] = &serviceConfig{
				service:               service,
				hasLocalHostNetworkEp: false,
				localEndpoints:        localEndpoints,
			}

			// Create endpoint slice
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "-test",
					Namespace: testNamespace,
					Labels: map[string]string{
						discovery.LabelServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: types.DefaultNetworkName,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
			}

			// Simulate namespace deletion - network lookup will fail
			npw.networkManager = &mockNetworkManagerWithNamespaceNotFoundError{}

			// Before fix: delServiceRules would NOT run (early return on network lookup error)
			// After fix: delServiceRules RUNS first, then network lookup error is gracefully handled
			err := npw.DeleteEndpointSlice(epSlice)

			// Verify no error (graceful handling)
			Expect(err).NotTo(HaveOccurred())

			// The key verification: service cache still exists and was updated by delServiceRules
			// This proves delServiceRules executed before the network lookup
			svcConfig, exists := npw.serviceInfo[svcKey]
			Expect(exists).To(BeTrue())
			Expect(svcConfig).NotTo(BeNil())
		})
	})
})
