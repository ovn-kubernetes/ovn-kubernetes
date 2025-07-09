package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var _ = Describe("Network Segmentation: Preconfigured Layer2 UDN", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-preconfigured-l2udn")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs        clientset.Interface
		udnClient udnclientset.Interface
	)

	type testConfig struct {
		netConfig          *networkAttachmentConfigParams
		expectedGatewayIPs []string
	}

	BeforeEach(func() {

		cs = f.ClientSet

		var err error
		udnClient, err = udnclientset.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())

		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		f.Namespace = namespace
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("should respect network configuration",
		func(config testConfig) {
			netConfig := config.netConfig

			By("creating the L2 network")

			netConfig.namespace = f.Namespace.Name
			udnManifest := generateUserDefinedNetworkManifest(netConfig, f.ClientSet)
			cleanup, err := createManifest(netConfig.namespace, udnManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanup)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, netConfig.namespace, netConfig.name), 5*time.Second, time.Second).Should(Succeed())

			By("creating a pod on the custom L2 network")
			podConfig := *podConfig("gateway-test-pod")
			podConfig.namespace = f.Namespace.Name
			pod := runUDNPod(cs, f.Namespace.Name, podConfig, nil)

			By("getting the created UDN object to validate against")
			udn, err := udnClient.K8sV1().UserDefinedNetworks(netConfig.namespace).Get(context.Background(), netConfig.name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("validating gateway configuration for pod %s", pod.Name))
			podAnno, err := unmarshalPodAnnotation(pod.Annotations, pod.Namespace+"/"+netConfig.name)
			Expect(err).NotTo(HaveOccurred())

			var podGatewayIPs []string
			for _, gw := range podAnno.Gateways {
				podGatewayIPs = append(podGatewayIPs, gw.String())
			}
			
			expectedGatewayIPs := filterIPs(f.ClientSet, config.expectedGatewayIPs...)
			Expect(podGatewayIPs).To(ContainElements(expectedGatewayIPs), "Gateway IPs should be found in pod routes")

			// Check that pod IP is not in reserved CIDRs (if any are configured)
			if len(udn.Spec.Layer2.ReservedSubnets) > 0 {
				By(fmt.Sprintf("validating pod %s is not in reserved subnet", pod.Name))
				for _, ip := range podAnno.IPs {
					podIP := ip.IP.String()
					// Check against each reserved subnet
					for _, reservedSubnet := range udn.Spec.Layer2.ReservedSubnets {
						_, reservedNet, err := net.ParseCIDR(string(reservedSubnet))
						Expect(err).NotTo(HaveOccurred())
						Expect(reservedNet.Contains(net.ParseIP(podIP))).To(BeFalse(),
							fmt.Sprintf("Pod IP %s should not be in reserved subnet %s", podIP, reservedSubnet))
					}
				}
			}

		},
		Entry("Layer2 basic configuration", testConfig{
			netConfig: &networkAttachmentConfigParams{
				name:     "custom-l2-net",
				topology: "layer2",
				cidr:     joinStrings("10.128.0.0/16", "2014:100:200::0/60"),
				role:     "primary",
			},
			expectedGatewayIPs: []string{"10.128.0.1", "2014:100:200::1"},
		}),
		Entry("Layer2 with custom subnets", testConfig{
			netConfig: &networkAttachmentConfigParams{
				name:                "custom-l2-net",
				topology:            "layer2",
				cidr:                joinStrings("10.128.0.0/16", "2014:100:200::0/60"),
				role:                "primary",
				defaultGatewayIPs:   joinStrings("10.128.0.10", "2014:100:200::100"),
				reservedCIDRs:       joinStrings("10.128.1.0/24", "2014:100:200::/122"),
				infrastructureCIDRs: joinStrings("10.128.0.10/30", "2014:100:200::100/122"),
			},
			expectedGatewayIPs: []string{"10.128.0.10", "2014:100:200::100"},
		}),
		Entry("Layer2 with inverted gateway/management IPs", testConfig{
			netConfig: &networkAttachmentConfigParams{
				name:              "inv-gateway-net",
				topology:          "layer2",
				cidr:              joinStrings("10.128.0.0/16", "2014:100:200::0/60"),
				role:              "primary",
				defaultGatewayIPs: joinStrings("10.128.0.2", "2014:100:200::2"),
			},
			expectedGatewayIPs: []string{"10.128.0.2", "2014:100:200::2"},
		}),
	)
})
