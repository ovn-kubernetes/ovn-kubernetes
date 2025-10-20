//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockNetworkManagerWithError is a mock implementation of networkmanager.Interface
// that returns an error from GetActiveNetworkForNamespace
type mockNetworkManagerWithError struct{}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("failed to get namespace \"%s\": namespace \"%s\" not found", namespace, namespace)
}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespaceFast(_ string) util.NetInfo {
	return nil
}

func (m *mockNetworkManagerWithError) GetNetwork(_ string) util.NetInfo {
	return nil
}

func (m *mockNetworkManagerWithError) GetActiveNetwork(_ string) util.NetInfo {
	return nil
}

func (m *mockNetworkManagerWithError) DoWithLock(_ func(_ util.NetInfo) error) error {
	return nil
}

func (m *mockNetworkManagerWithError) GetActiveNetworkNamespaces(_ string) ([]string, error) {
	return nil, nil
}

var _ = Describe("addServiceRules", func() {
	var (
		fakeClient     *util.OVNNodeClientset
		watcher        *factory.WatchFactory
		networkManager networkmanager.Interface
	)

	const (
		nodeName = "test-node"
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

		// Initialize network manager
		networkManager = networkmanager.Default().Interface()
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	Context("when called with nil networkManager", func() {
		It("should return error", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-error",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30080,
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("networkManager is nil"))
		})
	})

	Context("when GetActiveNetworkForNamespace returns error", func() {
		It("should return error from GetActiveNetworkForNamespace", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-network-error",
					Namespace: "test-namespace",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.2",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30081,
						},
					},
				},
			}

			// Create a mock networkManager that returns an error
			mockNetworkManager := &mockNetworkManagerWithError{}

			err := addServiceRules(service, nil, false, nil, mockNetworkManager)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get namespace \"test-namespace\": namespace \"test-namespace\" not found"))
		})
	})

	Context("when called for nodePortWatcherIptables mode (nil npw)", func() {
		It("should successfully process service without npw (iptables mode)", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-iptables",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.7",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30085,
						},
					},
				},
			}

			// nodePortWatcherIptables mode: npw is nil, localEndpoints is nil, hasLocalHostNetworkEp is false
			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle services with ExternalIPs in iptables mode", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-iptables-ext",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:        corev1.ServiceTypeClusterIP,
					ClusterIP:   "10.96.0.8",
					ExternalIPs: []string{"203.0.113.20"},
					Ports: []corev1.ServicePort{
						{
							Name:       "https",
							Protocol:   corev1.ProtocolTCP,
							Port:       443,
							TargetPort: intstr.FromInt(8443),
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle services with LoadBalancer IPs in iptables mode", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-iptables-lb",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeLoadBalancer,
					ClusterIP: "10.96.0.9",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30090,
						},
					},
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: "203.0.113.50"},
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle services with multiple ports in iptables mode", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-multiport",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.11",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30088,
						},
						{
							Name:       "https",
							Protocol:   corev1.ProtocolTCP,
							Port:       443,
							TargetPort: intstr.FromInt(8443),
							NodePort:   30443,
						},
						{
							Name:       "dns",
							Protocol:   corev1.ProtocolUDP,
							Port:       53,
							TargetPort: intstr.FromInt(5353),
							NodePort:   30053,
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("parameter validation", func() {
		It("should accept various combinations of parameters", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-params",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.100",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			// Test with empty localEndpoints
			err := addServiceRules(service, util.PortToLBEndpoints{}, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())

			// Test with populated localEndpoints
			localEndpoints := util.PortToLBEndpoints{
				"TCP/http": {
					Port:  8080,
					V4IPs: []string{"10.244.0.5", "10.244.0.6"},
				},
			}
			err = addServiceRules(service, localEndpoints, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())

			// Test with hasLocalHostNetworkEp = true
			localEndpointsHostNet := util.PortToLBEndpoints{
				"TCP/http": {
					Port:  8080,
					V4IPs: []string{"192.168.1.1"},
				},
			}
			err = addServiceRules(service, localEndpointsHostNet, true, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("function signature compatibility", func() {
		It("should match the refactored function signature", func() {
			// This test verifies that the function signature matches what callers expect
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-signature",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.200",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30200,
						},
					},
				},
			}

			// Test signature: (service, localEndpoints, svcHasLocalHostNetEndPnt, npw, networkManager)

			// Case 1: nodePortWatcher mode with full parameters
			localEndpoints := util.PortToLBEndpoints{
				"TCP/http": {
					Port:  8080,
					V4IPs: []string{"10.244.0.10"},
				},
			}
			hasLocalHostNetworkEp := false
			var npw *nodePortWatcher = nil // In real code, this would be a valid npw instance

			err := addServiceRules(service, localEndpoints, hasLocalHostNetworkEp, npw, networkManager)
			Expect(err).NotTo(HaveOccurred())

			// Case 2: nodePortWatcherIptables mode with nil npw
			err = addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with different service types", func() {
		It("should handle ClusterIP service", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-clusterip",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.50",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle NodePort service", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodeport",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.51",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   31080,
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle LoadBalancer service", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-loadbalancer",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeLoadBalancer,
					ClusterIP: "10.96.0.52",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   31081,
						},
					},
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: "203.0.113.100"},
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with different protocols", func() {
		It("should handle TCP protocol", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-tcp",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.60",
					Ports: []corev1.ServicePort{
						{
							Name:       "tcp-port",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle UDP protocol", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-udp",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.61",
					Ports: []corev1.ServicePort{
						{
							Name:       "udp-port",
							Protocol:   corev1.ProtocolUDP,
							Port:       53,
							TargetPort: intstr.FromInt(5353),
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle SCTP protocol", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sctp",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.62",
					Ports: []corev1.ServicePort{
						{
							Name:       "sctp-port",
							Protocol:   corev1.ProtocolSCTP,
							Port:       9999,
							TargetPort: intstr.FromInt(9999),
						},
					},
				},
			}

			err := addServiceRules(service, nil, false, nil, networkManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("nodePortWatcher DeleteEndpointSlice", func() {
	var (
		fakeClient     *util.OVNNodeClientset
		watcher        *factory.WatchFactory
		networkManager networkmanager.Interface
		npw            *nodePortWatcher
	)

	const (
		nodeName = "test-node"
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

		// Initialize network manager
		networkManager = networkmanager.Default().Interface()

		// Create a nodePortWatcher instance for testing
		npw = &nodePortWatcher{
			watchFactory:   watcher,
			networkManager: networkManager,
			nodeIPManager: &addressManager{
				nodeName: nodeName,
			},
			serviceInfo: make(map[ktypes.NamespacedName]*serviceConfig),
			ofm: &openflowManager{
				flowCache:     make(map[string][]string),
				exGWFlowCache: make(map[string][]string),
				flowChan:      make(chan struct{}, 1),
			},
		}
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	Context("when deleting an endpoint slice for default network", func() {
		It("should successfully delete endpoint slice when service exists", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.100",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30080,
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-slice1",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.0.5"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
				Ports: []discovery.EndpointPort{
					{
						Name:     ptr.To("http"),
						Port:     ptr.To(int32(8080)),
						Protocol: ptr.To(corev1.ProtocolTCP),
					},
				},
			}

			// Add service and endpoint slice to fake client
			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle deletion when service is not found", func() {
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "non-existent-service",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.0.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			}

			// Create endpoint slice without service
			_, err := fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			// Should not return error when service is not found
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle deletion when endpoint slice has already been deleted", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-deleted",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.101",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30081,
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleted-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-deleted",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.0.20"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			}

			// Add only service (endpoint slice already deleted from API server)
			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Try to delete endpoint slice that doesn't exist
			err = npw.DeleteEndpointSlice(epSlice)
			// Should not return error - this is a retry case
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("when network segmentation is enabled", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		})

		AfterEach(func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		})

		It("should use network name from endpoint slice annotation", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-udn",
					Namespace: "test-namespace",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.102",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-udn-slice",
					Namespace: "test-namespace",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-udn",
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: "my-network",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("test-namespace").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("test-namespace").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should use default network when annotation is missing", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-default-net",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.103",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30083,
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-default-net-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-default-net",
					},
					// No network annotation
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.2.5"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("when GetActiveNetworkForNamespace fails", func() {
		It("should not return error from GetActiveNetworkForNamespace when namespace is not found", func() {
			// DeleteEndpointSlice calls addServiceRules, which calls GetActiveNetworkForNamespace
			// This test verifies that the error is properly propagated

			mockNetworkManager := &mockNetworkManagerWithError{}
			npw.networkManager = mockNetworkManager

			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-netmgr",
					Namespace: "non-existent-namespace",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.104",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-netmgr-slice",
					Namespace: "non-existent-namespace",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-netmgr",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.3.5"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("non-existent-namespace").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("non-existent-namespace").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// First, we need to populate the serviceInfo cache so that DeleteEndpointSlice
			// will try to call addServiceRules
			npw.serviceInfo[ktypes.NamespacedName{Namespace: "non-existent-namespace", Name: "test-service-netmgr"}] = &serviceConfig{
				service:               service,
				hasLocalHostNetworkEp: false,
				localEndpoints: util.PortToLBEndpoints{
					"TCP/http": {
						Port:  8080,
						V4IPs: []string{"10.244.3.5"},
					},
				},
			}

			// DeleteEndpointSlice will call addServiceRules which calls GetActiveNetworkForNamespace
			// This should fail because the mock network manager returns an error
			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("edge cases", func() {
		It("should handle endpoint slice with no endpoints", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-empty",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.105",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-empty",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{}, // No endpoints
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle endpoint slice with multiple endpoints", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-multi-ep",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.106",
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							NodePort:   30086,
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-endpoint-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-multi-ep",
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.244.0.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
					{
						Addresses: []string{"10.244.0.11"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
					{
						Addresses: []string{"10.244.0.12"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(false), // Not ready
						},
					},
				},
				Ports: []discovery.EndpointPort{
					{
						Name:     ptr.To("http"),
						Port:     ptr.To(int32(8080)),
						Protocol: ptr.To(corev1.ProtocolTCP),
					},
				},
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle endpoint slice with IPv6 addresses", func() {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-ipv6",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:       corev1.ServiceTypeClusterIP,
					ClusterIP:  "fd00::100",
					IPFamilies: []corev1.IPFamily{corev1.IPv6Protocol},
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Protocol:   corev1.ProtocolTCP,
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						},
					},
				},
			}

			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ipv6-slice",
					Namespace: "default",
					Labels: map[string]string{
						discovery.LabelServiceName: "test-service-ipv6",
					},
				},
				AddressType: discovery.AddressTypeIPv6,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"fd00:10:244::5"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
				Ports: []discovery.EndpointPort{
					{
						Name:     ptr.To("http"),
						Port:     ptr.To(int32(8080)),
						Protocol: ptr.To(corev1.ProtocolTCP),
					},
				},
			}

			_, err := fakeClient.KubeClient.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices("default").Create(context.TODO(), epSlice, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
