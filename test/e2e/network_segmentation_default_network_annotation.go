package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
)

var _ = Describe("Network Segmentation: Default network multus annotation", func() {
	var (
		f = wrappedTestFramework("default-network-annotation")
	)
	f.SkipNamespaceCreation = true

	type testCase struct {
		ips []string
		mac string
	}
	DescribeTable("when added with static IP and MAC to a pod belonging to primary UDN", func(tc testCase) {
		if !isPreConfiguredUdnAddressesEnabled() {
			Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
		}
		tc.ips = filterCIDRs(f.ClientSet, tc.ips...)
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
		f.Namespace = namespace

		// Create the UDN client using the framework's config
		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

		// Define the UserDefinedNetwork object
		udn := &udnv1.UserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "l2network",
				Namespace: f.Namespace.Name,
			},
			Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
						udnv1.CIDR("103.0.0.0/16"),
						udnv1.CIDR("2014:100:200::0/60"),
					}),
				},
			},
		}

		// Create the resource in the generated namespace
		By("Create a UserDefinedNetwork with Layer2 topology and wait for availability")
		udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(context.TODO(), udn, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should create UserDefinedNetwork")
		Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udn.Namespace, udn.Name), 5*time.Second, time.Second).Should(Succeed())

		// Create the Pod in the generated namespace
		By("Create a Pod with the default network annotation and wait for readiness")
		ips, err := json.Marshal(tc.ips)
		Expect(err).NotTo(HaveOccurred(), "Should marshal IPs for annotation")

		// Define the Pod object with the specified annotation
		By("Creating the pod with the default network annotation and wait for readiness")
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, "static-ip-mac-pod", nil, nil, nil)
		pod.Annotations = map[string]string{
			"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, tc.mac, string(ips)),
		}
		pod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
		pod = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod)

		netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
			return status.Default
		})
		Expect(err).NotTo(HaveOccurred(), "Should get network status from pod")
		Expect(netStatus).To(HaveLen(1), "Should have one network status for the default network")
		var exposedIPs []string

		// Remove the CIDR from the IPs to expose only the IPs
		for _, ip := range tc.ips {
			exposedIPs = append(exposedIPs, strings.Split(ip, "/")[0])
		}
		Expect(netStatus[0].IPs).To(ConsistOf(exposedIPs), "Should have the IPs specified in the default network annotation")
		Expect(strings.ToLower(netStatus[0].Mac)).To(Equal(strings.ToLower(tc.mac)), "Should have the MAC specified in the default network annotation")

	},

		Entry("should create the pod with the specified static IP and MAC address", testCase{
			ips: []string{"103.0.0.3/16", "2014:100:200::3/60"},
			mac: "02:A1:B2:C3:D4:E5",
		}),
	)

	Context("ValidatingAdmissionPolicy protection", func() {
		It("should prevent adding, modifying and removing the default-network annotation on existing pods", func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

			// Create a UserDefinedNetwork for the test
			udn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: f.Namespace.Name,
				},
				Spec: udnv1.UserDefinedNetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role: udnv1.NetworkRolePrimary,
						Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
							"103.0.0.0/16",
							"2014:100:200::0/60",
						}),
					},
				},
			}

			By("Creating a UserDefinedNetwork")
			udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(context.TODO(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create UserDefinedNetwork")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udn.Namespace, udn.Name), 5*time.Second, time.Second).Should(Succeed())

			By("Creating a pod without the default-network annotation")
			podWithoutAnnotation := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-without-annotation", nil, nil, nil)
			podWithoutAnnotation.Spec.Containers[0].Command = []string{"sleep", "infinity"}
			podWithoutAnnotation = e2epod.NewPodClient(f).CreateSync(context.TODO(), podWithoutAnnotation)

			By("Creating a pod with the default-network annotation")

			nse := []nadapi.NetworkSelectionElement{{
				Name:       "default",
				Namespace:  "ovn-kubernetes",
				IPRequest:  []string{"103.0.0.3/16", "2014:100:200::3/60"},
				MacRequest: "02:A1:B2:C3:D4:E5",
			}}
			marshalledNSE, err := json.Marshal(nse)
			Expect(err).NotTo(HaveOccurred(), "Should marshal network selection element")

			podWithAnnotation := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-with-annotation", nil, nil, nil)
			podWithAnnotation.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": string(marshalledNSE),
			}
			podWithAnnotation.Spec.Containers[0].Command = []string{"sleep", "infinity"}
			podWithAnnotation = e2epod.NewPodClient(f).CreateSync(context.TODO(), podWithAnnotation)

			By("Attempting to add the default-network annotation to the pod without annotation")
			podWithoutAnnotation.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": string(marshalledNSE),
			}

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), podWithoutAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to add default-network annotation to existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))

			By("Attempting to modify the default-network annotation from the pod with annotation")
			updatedPodWithAnnotation := podWithAnnotation.DeepCopy()
			updatedPodWithAnnotation.Annotations["v1.multus-cni.io/default-network"] = `[{}]`

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), updatedPodWithAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to modify default-network annotation from existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))

			By("Attempting to remove the default-network annotation from the pod with annotation")
			delete(podWithAnnotation.Annotations, "v1.multus-cni.io/default-network")

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), podWithAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to remove default-network annotation from existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))
		})
	})
})
