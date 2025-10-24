package e2e

import (
	"context"
	"fmt"
	"k8s.io/kubernetes/test/e2e/framework/kubectl"
	"os"
	"strings"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = Describe("Network Segmentation: Egress Firewall", feature.NetworkSegmentation, func() {

	f := wrappedTestFramework("udn-egress-firewall")
	f.SkipNamespaceCreation = true
	egressFirewallYamlFile := "egress-fw-udn.yml"

	waitForEFApplied := func(namespace string) {
		Eventually(func() bool {
			output, err := kubectl.RunKubectl(namespace, "get", "egressfirewall", "default")
			if err != nil {
				framework.Failf("could not get the egressfirewall default in namespace: %s", namespace)
			}
			return strings.Contains(output, "EgressFirewall Rules applied")
		}, 10*time.Second).Should(BeTrue(), fmt.Sprintf("expected egress firewall in namespace %s to be successfully applied", namespace))
	}

	applyEF := func(egressFirewallConfig, namespace string) {
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			"--namespace=" + namespace,
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		kubectl.RunKubectlOrDie(namespace, applyArgs...)
		waitForEFApplied(namespace)
	}

	Context("on a user defined primary network", func() {
		const (
			nadName                      = "tenant-red"
			userDefinedNetworkIPv4Subnet = "172.31.0.0/16" // last subnet in private range 172.16.0.0/12 (rfc1918)
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
		)

		var (
			nadClient nadclient.K8sCniCncfIoV1Interface
		)

		BeforeEach(func() {
			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			f.Namespace = namespace
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable(
			// The test creates an egress firewall and ensures the pod within a UDN is blocked
			"egress firewall should be enforced",
			func(
				netConfigParams networkAttachmentConfigParams,
			) {
				By("Selecting 1 schedulable nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(nodes.Items)).To(BeNumerically(">", 0))

				By("Selecting nodes for pods and service")
				clientPodNodeName := nodes.Items[0].Name

				By("Creating the attachment configuration")
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Creating a UDN client pod on node (%s)", clientPodNodeName))
				srcPodName := "e2e-egress-fw-udn-src-pod"
				// create the pod that will be used as the source for the connectivity test
				createSrcPod(srcPodName, clientPodNodeName, retryInterval, retryTimeout, f)
				dnsName := "www.google.com"

				By(fmt.Sprintf("Verifying connectivity to internet site %s is permitted", dnsName))
				url := fmt.Sprintf("https://%s", dnsName)
				_, err = kubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "curl", "-g", "--max-time", "5", url)
				framework.ExpectNoError(err, "failed to curl DNS name %s", dnsName)

				By(fmt.Sprintf("Creating an Egress Firewall"))
				denyAllCIDR := "0.0.0.0/0"
				if IsIPv6Cluster(f.ClientSet) {
					denyAllCIDR = "::/0"
				}
				egressFirewallConfig := fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				By(fmt.Sprintf("Verifying connectivity to internet site %s is blocked", dnsName))
				_, err = kubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "curl", "-g", "--max-time", "5", url)
				Expect(err).To(HaveOccurred())

				By("Removing egress firewall, pod can reach internet again")
				kubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
				Eventually(func() bool {
					_, err := kubectl.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default")
					if err != nil {
						return true
					}
					return false
				}).Should(BeTrue())
				_, err = kubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "curl", "-g", "--max-time", "5", url)
				framework.ExpectNoError(err, "failed to curl DNS name %s", dnsName)
			},

			Entry(
				"L3 primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
			),
			Entry(
				"L2 primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
			),
		)

	})

})
