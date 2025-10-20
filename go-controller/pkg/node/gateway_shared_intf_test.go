//go:build linux
// +build linux

package node

import (
	"fmt"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockNetworkManagerWithError is a mock implementation of networkmanager.Interface
// that returns an error from GetActiveNetworkForNamespace
type mockNetworkManagerWithError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("failed to get namespace \"%s\": namespace \"%s\" not found", namespace, namespace)
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
		It("should wrap the error with service context", func() {
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
			// Verify addServiceRules adds proper context with service namespace/name
			Expect(err.Error()).To(ContainSubstring("error getting active network for service test-namespace/test-service-network-error"))
			// Verify the underlying error from GetActiveNetworkForNamespace is preserved
			Expect(err.Error()).To(ContainSubstring("namespace \"test-namespace\" not found"))
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
