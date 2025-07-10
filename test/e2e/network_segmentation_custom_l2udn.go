package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("Network Segmentation: Custom L2 UDN", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-custom-l2udn")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs        clientset.Interface
		udnClient udnclientset.Interface
	)

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
		func(netConfig *networkAttachmentConfigParams) {
			By("creating the L2 network")

			netConfig.namespace = f.Namespace.Name
			udnManifest := generateUserDefinedNetworkManifest(netConfig)
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
			Expect(udn.Spec.Layer2).NotTo(BeNil())

			By(fmt.Sprintf("validating gateway configuration for pod %s", pod.Name))
			podAnno, err := unmarshalPodAnnotation(pod.Annotations, pod.Namespace+"/"+netConfig.name)
			Expect(err).NotTo(HaveOccurred())

			expectedGatewayIPs := make(map[string]bool)
			if len(udn.Spec.Layer2.DefaultGatewayIPs) > 0 {
				for _, gwIP := range udn.Spec.Layer2.DefaultGatewayIPs {
					expectedGatewayIPs[string(gwIP)] = true
				}
			} else {
				// without custom gateways default values are expected
				for _, subnet := range udn.Spec.Layer2.Subnets {
					expectedGatewayIPs[util.GetNodeGatewayIfAddr(testing.MustParseIPNet(string(subnet))).IP.String()] = true
				}
			}

			expectedGWs := len(udn.Spec.Layer2.Subnets)
			for _, route := range podAnno.Routes {
				if route.NextHop != nil {
					gwIP := route.NextHop.String()
					if expectedGatewayIPs[gwIP] {
						expectedGWs--
						By(fmt.Sprintf("Found custom gateway IP %s in pod routes", gwIP))
						if expectedGWs == 0 {
							break
						}
					}
				}
			}
			Expect(expectedGWs).To(BeZero(), "Custom gateway IPs should be found in pod routes")

			// Check that pod IP is not in reserved CIDRs (if any are configured)
			if len(udn.Spec.Layer2.ReservedSubnets) > 0 {
				By(fmt.Sprintf("validating pod %s is not in reserved subnet", pod.Name))
				for _, ip := range podAnno.IPs {
					podIP := ip.IP.String()
					fmt.Println(podIP)
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
		Entry("Layer2 basic configuration", &networkAttachmentConfigParams{
			name:     "custom-l2-net",
			topology: "layer2",
			cidr:     correctCIDRFamily("10.128.0.0/16", "2014:100:200::0/60"),
			role:     "primary",
		}),
		Entry("Layer2 with custom subnets", &networkAttachmentConfigParams{
			name:              "custom-l2-net",
			topology:          "layer2",
			cidr:              correctCIDRFamily("10.128.0.0/16", "2014:100:200::0/60"),
			role:              "primary",
			defaultGatewayIPs: correctIPFamily("10.128.0.10", "2014:100:200::100"),
			reservedCIDRs:     correctCIDRFamily("10.128.1.0/24", "2014:100:200::/122"),
			infraCIDRs:        correctCIDRFamily("10.128.0.10/30", "2014:100:200::100/122"),
		}),
		Entry("Layer2 with inverted gateway/management IPs", &networkAttachmentConfigParams{
			name:              "inv-gateway-net",
			topology:          "layer2",
			cidr:              correctCIDRFamily("10.128.0.0/16", "2014:100:200::0/60"),
			role:              "primary",
			defaultGatewayIPs: correctIPFamily("10.128.0.2", "2014:100:200::2"),
		}),
	)
})
