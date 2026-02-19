package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	rav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	crdtypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/diagnostics"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/kubevirt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	butaneconfig "github.com/coreos/butane/config"
	butanecommon "github.com/coreos/butane/config/common"

	ipamclaimsv1alpha1 "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	kubevirtv1 "kubevirt.io/api/core/v1"
	kvmigrationsv1alpha1 "kubevirt.io/api/migrations/v1alpha1"
)

func newControllerRuntimeClient() (crclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := kubevirtv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := kvmigrationsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := ipamclaimsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := nadv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := udnv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := rav1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return crclient.New(config, crclient.Options{
		Scheme: scheme,
	})
}

var _ = Describe("Kubevirt Virtual Machines", feature.VirtualMachineSupport, func() {
	var (
		fr                  = wrappedTestFramework("kv-live-migration")
		d                   = diagnostics.New(fr)
		crClient            crclient.Client
		virtClient          *kubevirt.Client
		namespace           string
		iperf3DefaultPort   = int32(5201)
		tcpServerPort       = int32(9900)
		wg                  sync.WaitGroup
		selectedNodes       = []corev1.Node{}
		httpServerTestPods  = []*corev1.Pod{}
		iperfServerTestPods = []*corev1.Pod{}
		clientSet           kubernetes.Interface
		nadClient           nadclient.K8sCniCncfIoV1Interface
		providerCtx         infraapi.Context
		// Systemd resolvd prevent resolving kube api service by fqdn, so
		// we replace it here with NetworkManager

		isDualStack = func() bool {
			GinkgoHelper()
			nodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeList.Items).NotTo(BeEmpty())
			hasIPv4Address, hasIPv6Address := false, false
			for _, addr := range nodeList.Items[0].Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					if utilnet.IsIPv4String(addr.Address) {
						hasIPv4Address = true
					}
					if utilnet.IsIPv6String(addr.Address) {
						hasIPv6Address = true
					}
				}
			}
			return hasIPv4Address && hasIPv6Address
		}
	)

	// disable automatic namespace creation, we need to add the required UDN label
	fr.SkipNamespaceCreation = true

	type liveMigrationTestData struct {
		mode                kubevirtv1.MigrationMode
		numberOfVMs         int
		shouldExpectFailure bool
	}

	type execFnType = func(cmd string) (string, error)

	var (
		sendEcho = func(conn *net.TCPConn) error {
			strEcho := "Halo"

			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed configuring connection deadline: %w", err)
			}
			_, err := conn.Write([]byte(strEcho))
			if err != nil {
				return fmt.Errorf("failed Write to server: %w", err)
			}

			reply := make([]byte, 1024)

			_, err = conn.Read(reply)
			if err != nil {
				return fmt.Errorf("failed Read to server: %w", err)
			}

			if strings.Compare(string(reply), strEcho) == 0 {
				return fmt.Errorf("unexpected reply '%s'", string(reply))
			}
			return nil
		}

		sendEchos = func(conns []*net.TCPConn) error {
			for _, conn := range conns {
				if err := sendEcho(conn); err != nil {
					return err
				}
			}
			return nil
		}

		dial = func(addr string) (*net.TCPConn, error) {
			tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				return nil, fmt.Errorf("failed ResolveTCPAddr: %w", err)
			}
			backoff := wait.Backoff{
				Steps:    4,
				Duration: 10 * time.Millisecond,
				Factor:   5.0,
				Jitter:   0.1,
			}
			allErrors := func(error) bool { return true }
			var conn *net.TCPConn
			if err := retry.OnError(backoff, allErrors, func() error {
				conn, err = net.DialTCP("tcp", nil, tcpAddr)
				if err != nil {
					return fmt.Errorf("failed DialTCP: %w", err)
				}
				return nil
			}); err != nil {
				return nil, err
			}
			if err := conn.SetKeepAlive(true); err != nil {
				return nil, err
			}
			return conn, nil
		}

		dialServiceNodePort = func(client kubernetes.Interface, svc *corev1.Service) ([]*net.TCPConn, error) {
			worker, err := e2enode.GetRandomReadySchedulableNode(context.TODO(), client)
			if err != nil {
				return nil, fmt.Errorf("failed to find ready and schedulable node: %v", err)
			}
			if err != nil {
				return nil, err
			}
			endpoints := []*net.TCPConn{}
			nodePort := fmt.Sprintf("%d", svc.Spec.Ports[0].NodePort)
			port := fmt.Sprintf("%d", svc.Spec.Ports[0].Port)

			d.TCPDumpDaemonSet([]string{"any", deploymentconfig.Get().PrimaryInterfaceName(), deploymentconfig.Get().ExternalBridgeName()}, fmt.Sprintf("port %s or port %s", port, nodePort))
			for _, address := range worker.Status.Addresses {
				if address.Type != corev1.NodeHostName {
					addr := net.JoinHostPort(address.Address, nodePort)
					conn, err := dial(addr)
					if err != nil {
						return endpoints, err
					}
					endpoints = append(endpoints, conn)
				}
			}
			return endpoints, nil
		}

		reconnect = func(conns []*net.TCPConn) error {
			for i, conn := range conns {
				conn.Close()
				conn, err := dial(conn.RemoteAddr().String())
				if err != nil {
					return err
				}
				conns[i] = conn
			}
			return nil
		}

		composeService = func(name, vmName string, port int32) *corev1.Service {
			ipFamilyPolicy := corev1.IPFamilyPolicyPreferDualStack
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: name + vmName,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port: port,
					}},
					Selector: map[string]string{
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					Type:           corev1.ServiceTypeNodePort,
					IPFamilyPolicy: &ipFamilyPolicy,
				},
			}
		}

		by = func(vmName string, step string) string {
			fullStep := fmt.Sprintf("%s: %s", vmName, step)
			By(fullStep)
			return fullStep
		}

		/*
			createDenyAllPolicy = func(vmName string) (*knet.NetworkPolicy, error) {
				policy := &knet.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name: "deny-all-" + vmName,
					},
					Spec: knet.NetworkPolicySpec{
						PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
							kubevirtv1.VirtualMachineNameLabel: vmName,
						}},
						PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress, knet.PolicyTypeIngress},
						Ingress:     []knet.NetworkPolicyIngressRule{},
						Egress:      []knet.NetworkPolicyEgressRule{},
					},
				}
				return fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
			}
		*/

		checkEastWestTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, podIPsByName map[string][]string, stage string) {
			GinkgoHelper()
			Expect(podIPsByName).NotTo(BeEmpty())
			polling := 15 * time.Second
			timeout := time.Minute
			for podName, podIPs := range podIPsByName {
				for _, podIP := range podIPs {
					output := ""
					Eventually(func() error {
						var err error
						output, err = virtClient.RunCommand(vmi, fmt.Sprintf("curl http://%s", net.JoinHostPort(podIP, "8000")), polling)
						return err
					}).
						WithPolling(polling).
						WithTimeout(timeout).
						Should(Succeed(), func() string { return stage + ": " + podName + ": " + output })
				}
			}
		}

		startEastWestIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, serverPodIPsByName map[string][]string, stage string) error {
			GinkgoHelper()
			Expect(serverPodIPsByName).NotTo(BeEmpty())
			polling := 15 * time.Second
			for podName, serverPodIPs := range serverPodIPsByName {
				for _, serverPodIP := range serverPodIPs {
					output, err := virtClient.RunCommand(vmi, fmt.Sprintf("iperf3 -t 0 -c %[2]s --logfile /tmp/%[1]s_%[2]s_iperf3.log &", podName, serverPodIP), polling)
					if err != nil {
						return fmt.Errorf("%s: %w", output, err)
					}
				}
			}
			return nil
		}

		checkIperfTraffic = func(iperfLogFile string, execFn func(cmd string) (string, error), stage string) {
			GinkgoHelper()
			// Check the last line eventually show traffic flowing
			Eventually(func() (string, error) {
				iperfLog, err := execFn("cat " + iperfLogFile)
				if err != nil {
					return "", err
				}
				// Fail fast
				Expect(iperfLog).NotTo(ContainSubstring("iperf3: error"), stage+": "+iperfLogFile)
				// Remove last carriage return to properly split by new line.
				iperfLog = strings.TrimSuffix(iperfLog, "\n")
				iperfLogLines := strings.Split(iperfLog, "\n")
				if len(iperfLogLines) == 0 {
					return "", nil
				}
				lastIperfLogLine := iperfLogLines[len(iperfLogLines)-1]
				return lastIperfLogLine, nil
			}).
				WithPolling(50*time.Millisecond).
				WithTimeout(2*time.Second).
				Should(
					SatisfyAll(
						ContainSubstring(" sec "),
						Not(ContainSubstring("0.00 Bytes  0.00 bits/sec")),
					),
					stage+": failed checking iperf3 traffic at file "+iperfLogFile,
				)
		}

		checkEastWestIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, podIPsByName map[string][]string, stage string) {
			GinkgoHelper()
			for podName, podIPs := range podIPsByName {
				for _, podIP := range podIPs {
					iperfLogFile := fmt.Sprintf("/tmp/%s_%s_iperf3.log", podName, podIP)
					execFn := func(cmd string) (string, error) {
						return virtClient.RunCommand(vmi, cmd, 2*time.Second)
					}
					checkIperfTraffic(iperfLogFile, execFn, stage)
				}
			}
		}
		startNorthSouthIperfTraffic = func(execFn execFnType, addresses []string, port int32, logPrefix, stage string) error {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, address := range addresses {
				iperfLogFile := fmt.Sprintf("/tmp/%s_test_%s_%d_iperf3.log", logPrefix, address, port)
				By(fmt.Sprintf("remove iperf3 log for %s: %s", address, stage))
				output, err := execFn(fmt.Sprintf("rm -f %s", iperfLogFile))
				if err != nil {
					return fmt.Errorf("failed removing iperf3 log file %s: %w", output, err)
				}

				By(fmt.Sprintf("check iperf3 connectivity for %s: %s", address, stage))
				output, err = execFn(fmt.Sprintf("iperf3 -c %s -p %d", address, port))
				if err != nil {
					return fmt.Errorf("failed checking iperf3 connectivity %s: %w", output, err)
				}

				By(fmt.Sprintf("start from %s: %s", address, stage))
				output, err = execFn(fmt.Sprintf("nohup iperf3 -t 0 -c %[1]s -p %[2]d --logfile %[3]s &", address, port, iperfLogFile))
				if err != nil {
					return fmt.Errorf("failed at starting iperf3 in background %s: %w", output, err)
				}
			}
			return nil
		}

		startNorthSouthIngressIperfTraffic = func(container infraapi.ExternalContainer, addresses []string, port int32, stage string) error {
			GinkgoHelper()
			execFn := func(cmd string) (string, error) {
				return infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", cmd})
			}
			return startNorthSouthIperfTraffic(execFn, addresses, port, "ingress", stage)
		}

		startNorthSouthEgressIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, port int32, stage string) error {
			GinkgoHelper()
			execFn := func(cmd string) (string, error) {
				return virtClient.RunCommand(vmi, cmd, 5*time.Second)
			}
			return startNorthSouthIperfTraffic(execFn, addresses, port, "egress", stage)
		}

		checkNorthSouthIngressIperfTraffic = func(container infraapi.ExternalContainer, addresses []string, port int32, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				iperfLogFile := fmt.Sprintf("/tmp/ingress_test_%s_%d_iperf3.log", ip, port)
				execFn := func(cmd string) (string, error) {
					return infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", cmd})
				}
				checkIperfTraffic(iperfLogFile, execFn, stage)
			}
		}

		checkNorthSouthEgressIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, port int32, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				if ip == "" {
					continue
				}
				for _, ip := range addresses {
					iperfLogFile := fmt.Sprintf("/tmp/egress_test_%s_%d_iperf3.log", ip, port)
					execFn := func(cmd string) (string, error) {
						return virtClient.RunCommand(vmi, cmd, 5*time.Second)
					}
					checkIperfTraffic(iperfLogFile, execFn, stage)
				}
			}
		}

		checkNorthSouthEgressICMPTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				if ip == "" {
					continue
				}
				cmd := fmt.Sprintf("ping -c 3 -W 2 %s", ip)
				stdout, err := virtClient.RunCommand(vmi, cmd, 5*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(stdout).To(ContainSubstring(" 0% packet loss"))
			}
		}

		httpServerTestPodsDefaultNetworkIPs = func() map[string][]string {
			ips := map[string][]string{}
			for _, pod := range httpServerTestPods {
				for _, podIP := range pod.Status.PodIPs {
					ips[pod.Name] = append(ips[pod.Name], podIP.IP)
				}
			}
			return ips
		}

		podsMultusNetworkIPs = func(pods []*corev1.Pod, networkStatusPredicate func(nadapi.NetworkStatus) bool) map[string][]string {
			GinkgoHelper()
			ips := map[string][]string{}
			for _, pod := range pods {
				var networkStatuses []nadapi.NetworkStatus
				Eventually(func() ([]nadapi.NetworkStatus, error) {
					var err error
					networkStatuses, err = podNetworkStatus(pod, networkStatusPredicate)
					return networkStatuses, err
				}).
					WithTimeout(15 * time.Second).
					WithPolling(200 * time.Millisecond).
					Should(HaveLen(1))
				for _, ip := range networkStatuses[0].IPs {
					ips[pod.Name] = append(ips[pod.Name], ip)
				}
			}
			return ips
		}

		checkConnectivity = func(vmName string, endpoints []*net.TCPConn, stage string) {
			GinkgoHelper()
			by(vmName, "Check connectivity "+stage)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred())
			polling := 6 * time.Second
			timeout := 2 * time.Minute
			step := by(vmName, stage+": Check tcp connection is not broken")
			Eventually(func() error {
				err = sendEchos(endpoints)
				if err != nil {
					by(vmName, fmt.Sprintf("%s: Check tcp connection failed: %s", stage, err))
					_ = reconnect(endpoints)
				}
				return err
			}).
				WithPolling(polling).
				WithTimeout(timeout).
				Should(Succeed(), step)

			stage = by(vmName, stage+": Check e/w tcp traffic")
			checkEastWestTraffic(vmi, httpServerTestPodsDefaultNetworkIPs(), stage)

			step = by(vmName, stage+": Check n/s tcp traffic")
			output := ""
			Eventually(func() error {
				output, err = virtClient.RunCommand(vmi, "curl -kL https://kubernetes.default.svc.cluster.local", polling)
				return err
			}).
				WithPolling(polling).
				WithTimeout(timeout).
				Should(Succeed(), func() string { return step + ": " + output })
		}

		checkConnectivityAndNetworkPolicies = func(vmName string, endpoints []*net.TCPConn, stage string) {
			GinkgoHelper()
			checkConnectivity(vmName, endpoints, stage)
			By("Skip network policy, test should be fixed after OVN bump broke them")
			/*
				step := by(vmName, stage+": Create deny all network policy")
				policy, err := createDenyAllPolicy(vmName)
				Expect(err).NotTo(HaveOccurred(), step)

				step = by(vmName, stage+": Check connectivity block after create deny all network policy")
				Eventually(func() error { return sendEchos(endpoints) }).
					WithPolling(time.Second).
					WithTimeout(5*time.Second).
					ShouldNot(Succeed(), step)

				Expect(fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Delete(context.TODO(), policy.Name, metav1.DeleteOptions{})).To(Succeed())

				// After apply a deny all policy, the keep-alive packets will be block and
				// the tcp connection may break, to overcome that the test reconnects
				// after deleting the deny all policy to ensure a healthy tcp connection
				Expect(reconnect(endpoints)).To(Succeed(), step)

				step = by(vmName, stage+": Check connectivity is restored after delete deny all network policy")
				Expect(sendEchos(endpoints)).To(Succeed(), step)
			*/
		}

		composeAgnhostPod = func(name, namespace, nodeName string, args ...string) *corev1.Pod {
			agnHostPod := e2epod.NewAgnhostPod(namespace, name, nil, nil, nil, args...)
			agnHostPod.Spec.NodeName = nodeName
			return agnHostPod
		}

		liveMigrateVirtualMachine = func(vmName string) {
			GinkgoHelper()
			vmimCreationRetries := 0
			Eventually(func() error {
				if vmimCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vmim %s creation", vmName))
				}
				vmim := &kubevirtv1.VirtualMachineInstanceMigration{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:    namespace,
						GenerateName: vmName,
					},
					Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
						VMIName: vmName,
					},
				}
				err := crClient.Create(context.Background(), vmim)
				vmimCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		checkLiveMigrationSucceeded = func(vmName string, migrationMode kubevirtv1.MigrationMode) {
			GinkgoHelper()
			By("checking the VM live-migrated correctly")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi")
			currentNode := vmi.Status.NodeName

			Eventually(func() *kubevirtv1.VirtualMachineInstanceMigrationState {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState
			}).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(BeNil(), "should have a MigrationState")
			Eventually(func() string {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState.TargetNode
			}).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(Equal(currentNode), "should refresh MigrationState")
			Eventually(func() bool {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState.Completed
			}).WithPolling(time.Second).WithTimeout(20*time.Minute).Should(BeTrue(), "should complete migration")
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())
			Expect(vmi.Status.MigrationState.SourcePod).NotTo(BeEmpty())
			Eventually(func() corev1.PodPhase {
				sourcePod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      vmi.Status.MigrationState.SourcePod,
					},
				}
				err = crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(sourcePod), sourcePod)
				Expect(err).NotTo(HaveOccurred())
				return sourcePod.Status.Phase
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Equal(corev1.PodSucceeded), "should move source pod to Completed")
			err = crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi after migration")
			Expect(vmi.Status.MigrationState.Failed).To(BeFalse(), func() string {
				vmiJSON, err := json.Marshal(vmi)
				if err != nil {
					return fmt.Sprintf("failed marshaling migrated VM: %v", vmiJSON)
				}
				return fmt.Sprintf("should live migrate successfully: %s", string(vmiJSON))
			})
			Expect(vmi.Status.MigrationState.Mode).To(Equal(migrationMode), "should be the expected migration mode %s", migrationMode)
		}

		liveMigrateSucceed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			liveMigrateVirtualMachine(vmi.Name)
			checkLiveMigrationSucceeded(vmi.Name, kubevirtv1.MigrationPreCopy)
		}

		vmiMigrations = func(client crclient.Client) ([]kubevirtv1.VirtualMachineInstanceMigration, error) {
			unstructuredVMIMigrations := &unstructured.UnstructuredList{}
			unstructuredVMIMigrations.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   kubevirtv1.GroupVersion.Group,
				Kind:    "VirtualMachineInstanceMigrationList",
				Version: kubevirtv1.GroupVersion.Version,
			})

			if err := client.List(context.Background(), unstructuredVMIMigrations); err != nil {
				return nil, err
			}
			if len(unstructuredVMIMigrations.Items) == 0 {
				return nil, fmt.Errorf("empty migration list")
			}

			var migrations []kubevirtv1.VirtualMachineInstanceMigration
			for i := range unstructuredVMIMigrations.Items {
				var vmiMigration kubevirtv1.VirtualMachineInstanceMigration
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
					unstructuredVMIMigrations.Items[i].Object,
					&vmiMigration,
				); err != nil {
					return nil, err
				}
				migrations = append(migrations, vmiMigration)
			}

			return migrations, nil
		}

		checkLiveMigrationFailed = func(vmName string) {
			GinkgoHelper()
			By("checking the VM live-migrated failed to migrate")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi")

			Eventually(func() (kubevirtv1.VirtualMachineInstanceMigrationPhase, error) {
				migrations, err := vmiMigrations(crClient)
				if err != nil {
					return kubevirtv1.MigrationPhaseUnset, err
				}
				if len(migrations) > 1 {
					return kubevirtv1.MigrationPhaseUnset, fmt.Errorf("expected one migration, got %d", len(migrations))
				}
				return migrations[0].Status.Phase, nil
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(
				Equal(kubevirtv1.MigrationFailed),
			)
		}

		liveMigrateFailed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			forceLiveMigrationFailureAnnotationName := kubevirtv1.FuncTestForceLauncherMigrationFailureAnnotation
			By(fmt.Sprintf("Forcing live migration failure by annotating VM with %s", forceLiveMigrationFailureAnnotationName))
			vmiKey := types.NamespacedName{Namespace: namespace, Name: vmi.Name}
			Eventually(func() error {
				err := crClient.Get(context.TODO(), vmiKey, vmi)
				if err == nil {
					vmi.ObjectMeta.Annotations[forceLiveMigrationFailureAnnotationName] = "true"
					err = crClient.Update(context.TODO(), vmi)
				}
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())

			liveMigrateVirtualMachine(vmi.Name)
			checkLiveMigrationFailed(vmi.Name)
		}

		ipv4 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv4.Address
		}

		ipv6 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv6.Address
		}

		findNonLoopbackInterface = func(interfaces []kubevirt.Interface) *kubevirt.Interface {
			for _, iface := range interfaces {
				if iface.Name != "lo" {
					return &iface
				}
			}
			return nil
		}

		addressByFamily = func(familyFn func(iface kubevirt.Interface) []kubevirt.Address, vmi *kubevirtv1.VirtualMachineInstance) func() ([]kubevirt.Address, error) {
			return func() ([]kubevirt.Address, error) {
				networkState, err := kubevirt.RetrieveNetworkState(virtClient, vmi)
				if err != nil {
					return nil, err
				}
				iface := findNonLoopbackInterface(networkState.Interfaces)
				if iface == nil {
					return nil, fmt.Errorf("missing non loopback interface")
				}
				return familyFn(*iface), nil
			}
		}

		addressesFromStatus = func(vmi *kubevirtv1.VirtualMachineInstance) func() ([]string, error) {
			return func() ([]string, error) {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				if err != nil {
					return nil, err
				}
				var addresses []string
				for _, iface := range vmi.Status.Interfaces {
					for _, ip := range iface.IPs {
						if netip.MustParseAddr(ip).IsLinkLocalUnicast() {
							continue
						}
						addresses = append(addresses, ip)
					}
				}
				return addresses, nil
			}
		}

		createVirtualMachine = func(vm *kubevirtv1.VirtualMachine) {
			GinkgoHelper()
			By(fmt.Sprintf("Create virtual machine %s", vm.Name))
			vmCreationRetries := 0
			Eventually(func() error {
				if vmCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vm %s creation", vm.Name))
				}
				err := crClient.Create(context.Background(), vm)
				vmCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		createVirtualMachineInstance = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			By(fmt.Sprintf("Create virtual machine instance %s", vmi.Name))
			vmiCreationRetries := 0
			Eventually(func() error {
				if vmiCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vmi %s creation", vmi.Name))
				}
				err := crClient.Create(context.Background(), vmi)
				vmiCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		waitVirtualMachineInstanceReadinessWith = func(vmi *kubevirtv1.VirtualMachineInstance, conditionStatus corev1.ConditionStatus) {
			GinkgoHelper()
			By(fmt.Sprintf("Waiting for readiness=%q at virtual machine %s", conditionStatus, vmi.Name))
			Eventually(func() []kubevirtv1.VirtualMachineInstanceCondition {
				err := crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).To(SatisfyAny(
					WithTransform(apierrors.IsNotFound, BeTrue()),
					Succeed(),
				))
				return vmi.Status.Conditions
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(
				ContainElement(SatisfyAll(
					HaveField("Type", kubevirtv1.VirtualMachineInstanceReady),
					HaveField("Status", conditionStatus),
				)))
		}

		waitVirtualMachineInstanceReadiness = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			waitVirtualMachineInstanceReadinessWith(vmi, corev1.ConditionTrue)
		}

		waitVirtualMachineInstanceFailed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			waitVirtualMachineInstanceReadinessWith(vmi, corev1.ConditionFalse)
		}

		waitVirtualMachineAddresses = func(vmi *kubevirtv1.VirtualMachineInstance) []kubevirt.Address {
			GinkgoHelper()
			step := by(vmi.Name, "Wait for virtual machine to receive IPv4 address from DHCP")
			Eventually(addressByFamily(ipv4, vmi)).
				WithPolling(time.Second).
				WithTimeout(5*time.Minute).
				Should(HaveLen(1), step)
			addresses, err := addressByFamily(ipv4, vmi)()
			Expect(err).NotTo(HaveOccurred())
			if isDualStack() {
				output, err := virtClient.RunCommand(vmi, `echo '{"interfaces":[{"name":"enp1s0","type":"ethernet","state":"up","ipv4":{"enabled":true,"dhcp":true},"ipv6":{"enabled":true,"dhcp":true,"autoconf":false}}],"routes":{"config":[{"destination":"::/0","next-hop-interface":"enp1s0","next-hop-address":"fe80::1"}]}}' |nmstatectl apply`, 5*time.Second)
				Expect(err).NotTo(HaveOccurred(), output)
				step = by(vmi.Name, "Wait for virtual machine to receive IPv6 address from DHCP")
				Eventually(addressByFamily(ipv6, vmi)).
					WithPolling(time.Second).
					WithTimeout(5*time.Minute).
					Should(HaveLen(2), func() string {
						output, _ := virtClient.RunCommand(vmi, "journalctl -u nmstate", 2*time.Second)
						return step + " -> journal nmstate: " + output
					})
				ipv6Addresses, err := addressByFamily(ipv6, vmi)()
				Expect(err).NotTo(HaveOccurred())
				addresses = append(addresses, ipv6Addresses...)
			}
			return addresses
		}

		waitForVMPodErrorEvent = func(vmName string, expectedErrorSubstring string) {
			GinkgoHelper()
			Eventually(func() []corev1.Event {
				podList, err := fr.ClientSet.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: fmt.Sprintf("%s=%s", kubevirtv1.VirtualMachineNameLabel, vmName),
				})
				if err != nil || len(podList.Items) == 0 {
					return nil
				}

				events, err := fr.ClientSet.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{
					FieldSelector: fmt.Sprintf("involvedObject.name=%s", podList.Items[0].Name),
				})
				if err != nil {
					return nil
				}

				return events.Items
			}).
				WithTimeout(60*time.Second).
				WithPolling(2*time.Second).
				Should(ContainElement(SatisfyAll(
					HaveField("Type", Equal("Warning")),
					HaveField("Message", ContainSubstring(expectedErrorSubstring)),
				)), fmt.Sprintf("VM %s should fail with error: %s", vmName, expectedErrorSubstring))
		}

		virtualMachineAddressesFromStatus = func(vmi *kubevirtv1.VirtualMachineInstance, expectedNumberOfAddresses int) []string {
			GinkgoHelper()
			step := by(vmi.Name, "Wait for virtual machine to report addresses")
			Eventually(addressesFromStatus(vmi)).
				WithPolling(time.Second).
				WithTimeout(10*time.Second).
				Should(HaveLen(expectedNumberOfAddresses), step)

			addresses, err := addressesFromStatus(vmi)()
			Expect(err).NotTo(HaveOccurred())
			return addresses
		}

		generateVMI = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, cloudInitVolumeSource kubevirtv1.VolumeSource, image string) *kubevirtv1.VirtualMachineInstance {
			return &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:    namespace,
					GenerateName: "worker-",
					Annotations:  annotations,
					Labels:       labels,
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					NodeSelector: nodeSelector,
					Domain: kubevirtv1.DomainSpec{
						Resources: kubevirtv1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("1024Mi"),
							},
						},
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{
								{
									DiskDevice: kubevirtv1.DiskDevice{
										Disk: &kubevirtv1.DiskTarget{
											Bus: kubevirtv1.DiskBusVirtio,
										},
									},
									Name: "containerdisk",
								},
								{
									DiskDevice: kubevirtv1.DiskDevice{
										Disk: &kubevirtv1.DiskTarget{
											Bus: kubevirtv1.DiskBusVirtio,
										},
									},
									Name: "cloudinitdisk",
								},
							},
							Interfaces: []kubevirtv1.Interface{
								{
									Name: "net1",
									InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
										Bridge: &kubevirtv1.InterfaceBridge{},
									},
								},
							},
							Rng: &kubevirtv1.Rng{},
						},
					},
					Networks: []kubevirtv1.Network{
						{
							Name:          "net1",
							NetworkSource: networkSource,
						},
					},
					TerminationGracePeriodSeconds: ptr.To(int64(5)),
					Volumes: []kubevirtv1.Volume{
						{
							Name: "containerdisk",
							VolumeSource: kubevirtv1.VolumeSource{
								ContainerDisk: &kubevirtv1.ContainerDiskSource{
									Image: image,
								},
							},
						},
						{
							Name:         "cloudinitdisk",
							VolumeSource: cloudInitVolumeSource,
						},
					},
				},
			}
		}

		generateVM = func(vmi *kubevirtv1.VirtualMachineInstance) *kubevirtv1.VirtualMachine {
			return &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:    namespace,
					GenerateName: vmi.GenerateName,
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: ptr.To(kubevirtv1.RunStrategyAlways),
					Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: vmi.Annotations,
							Labels:      vmi.Labels,
						},
						Spec: vmi.Spec,
					},
				},
			}
		}

		fcosVMI = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, butane string) (*kubevirtv1.VirtualMachineInstance, error) {
			workingDirectory, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			ignition, _, err := butaneconfig.TranslateBytes([]byte(butane), butanecommon.TranslateBytesOptions{
				TranslateOptions: butanecommon.TranslateOptions{
					FilesDir: workingDirectory,
				},
			})
			cloudInitVolumeSource := kubevirtv1.VolumeSource{
				CloudInitConfigDrive: &kubevirtv1.CloudInitConfigDriveSource{
					UserData: string(ignition),
				},
			}
			return generateVMI(labels, annotations, nodeSelector, networkSource, cloudInitVolumeSource, kubevirt.FedoraCoreOSContainerDiskImage), nil
		}

		fcosVM = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, butane string) (*kubevirtv1.VirtualMachine, error) {
			vmi, err := fcosVMI(labels, annotations, nodeSelector, networkSource, butane)
			if err != nil {
				return nil, err
			}
			return generateVM(vmi), nil
		}

		fedoraWithTestToolingVMI = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachineInstance {
			cloudInitVolumeSource := kubevirtv1.VolumeSource{
				CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{
					UserData:    userData,
					NetworkData: networkData,
				},
			}
			return generateVMI(labels, annotations, nodeSelector, networkSource, cloudInitVolumeSource, kubevirt.FedoraWithTestToolingContainerDiskImage)
		}

		fedoraWithTestToolingVM = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachine {
			return generateVM(fedoraWithTestToolingVMI(labels, annotations, nodeSelector, networkSource, userData, networkData))
		}

		createVMWithStaticIP = func(vmName string, staticIPs []string) *kubevirtv1.VirtualMachine {
			GinkgoHelper()
			annotations, err := kubevirt.GenerateAddressesAnnotations("net1", staticIPs)
			Expect(err).NotTo(HaveOccurred())

			vm := fedoraWithTestToolingVM(
				nil,         // labels
				annotations, // annotations with static IP
				nil,         // nodeSelector
				kubevirtv1.NetworkSource{
					Pod: &kubevirtv1.PodNetwork{},
				},
				`#cloud-config
password: fedora
chpasswd: { expire: False }
`,
				`version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`,
			)
			vm.Name = vmName
			vm.Namespace = namespace
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			return vm
		}

		composeDefaultNetworkLiveMigratableVM = func(labels map[string]string, butane string) (*kubevirtv1.VirtualMachine, error) {
			annotations := map[string]string{
				"kubevirt.io/allow-pod-bridge-network-live-migration": "",
			}
			nodeSelector := map[string]string{
				namespace: "true",
			}
			networkSource := kubevirtv1.NetworkSource{
				Pod: &kubevirtv1.PodNetwork{},
			}
			return fcosVM(labels, annotations, nodeSelector, networkSource, butane)
		}

		composeDefaultNetworkLiveMigratableVMs = func(numberOfVMs int, labels map[string]string) ([]*kubevirtv1.VirtualMachine, error) {
			butane := fmt.Sprintf(`
variant: fcos
version: 1.4.0
storage:
  files:
    - path: /root/test/server.go
      contents:
        local: kubevirt/echoserver/main.go
systemd:
  units:
    - name: systemd-resolved.service
      mask: true
    - name: replace-resolved.service
      enabled: true
      contents: |
        [Unit]
        Description=Replace systemd resolvd with NetworkManager
        Wants=network-online.target
        After=network-online.target
        [Service]
        ExecStart=rm -f /etc/resolv.conf
        ExecStart=systemctl restart NetworkManager
        Type=oneshot
        [Install]
        WantedBy=multi-user.target
    - name: echoserver.service
      enabled: true
      contents: |
        [Unit]
        Description=Golang echo server
        Wants=replace-resolved.service
        After=replace-resolved.service
        [Service]
        ExecStart=podman run --name tcpserver --tls-verify=false --privileged --net=host -v /root/test:/test:z registry.access.redhat.com/ubi9/go-toolset:1.20 go run /test/server.go %d
        [Install]
        WantedBy=multi-user.target
passwd:
  users:
  - name: core
    password_hash: $y$j9T$b7RFf2LW7MUOiF4RyLHKA0$T.Ap/uzmg8zrTcUNXyXvBvT26UgkC6zZUVg3UKXeEp5
`, tcpServerPort)

			vms := []*kubevirtv1.VirtualMachine{}
			for i := 1; i <= numberOfVMs; i++ {
				vm, err := composeDefaultNetworkLiveMigratableVM(labels, butane)
				if err != nil {
					return nil, err
				}
				vms = append(vms, vm)
			}
			return vms, nil
		}
		liveMigrateAndCheck = func(vmName string, migrationMode kubevirtv1.MigrationMode, endpoints []*net.TCPConn, step string) {
			liveMigrateVirtualMachine(vmName)
			checkLiveMigrationSucceeded(vmName, migrationMode)
			checkConnectivityAndNetworkPolicies(vmName, endpoints, step)
		}

		runLiveMigrationTest = func(td liveMigrationTestData, vm *kubevirtv1.VirtualMachine) {
			GinkgoHelper()
			defer GinkgoRecover()
			defer wg.Done()
			step := by(vm.Name, "Login to virtual machine")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred())
			Expect(virtClient.LoginToFedora(vmi, "core", "fedora")).To(Succeed(), step)

			waitVirtualMachineAddresses(vmi)

			step = by(vm.Name, "Expose tcpServer as a service")
			svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("tcpserver", vm.Name, tcpServerPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), step)
			defer func() {
				output, err := virtClient.RunCommand(vmi, "podman logs tcpserver", 10*time.Second)
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("%s tcpserver logs: %s", vmi.Name, output)
			}()

			By("Wait some time for service to settle")
			endpoints := []*net.TCPConn{}
			Eventually(func() error {
				endpoints, err = dialServiceNodePort(clientSet, svc)
				return err
			}).WithPolling(3*time.Second).WithTimeout(60*time.Second).Should(Succeed(), "Should dial service port once service settled")

			checkConnectivityAndNetworkPolicies(vm.Name, endpoints, "before live migration")
			// Do just one migration that will fail
			if td.shouldExpectFailure {
				by(vm.Name, "Live migrate virtual machine to check failed migration")
				liveMigrateVirtualMachine(vm.Name)
				checkLiveMigrationFailed(vm.Name)
				checkConnectivityAndNetworkPolicies(vm.Name, endpoints, "after live migrate to check failed migration")
			} else {
				originalNode := vmi.Status.NodeName
				by(vm.Name, "Live migrate for the first time")
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migrate for the first time")

				by(vm.Name, "Live migrate for the second time to a node not owning the subnet")
				// Remove the node selector label from original node to force
				// live migration to a different one.
				e2enode.RemoveLabelOffNode(fr.ClientSet, originalNode, namespace)
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migration for the second time to node not owning subnet")

				by(vm.Name, "Live migrate for the third time to the node owning the subnet")
				// Patch back the original node with the label and remove it
				// from the rest of nodes to force live migration target to it.
				e2enode.AddOrUpdateLabelOnNode(fr.ClientSet, originalNode, namespace, "true")
				for _, selectedNode := range selectedNodes {
					if selectedNode.Name != originalNode {
						e2enode.RemoveLabelOffNode(fr.ClientSet, selectedNode.Name, namespace)
					}
				}
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migration to node owning the subnet")
			}
		}

		checkPodHasIPAtStatus = func(g Gomega, pod *corev1.Pod) {
			g.Expect(pod.Status.PodIP).NotTo(BeEmpty(), "pod %s has no valid IP address yet", pod.Name)
		}

		sanitizeNodeName = func(nodeName string) string {
			return strings.ReplaceAll(nodeName, ".", "-")
		}

		createHTTPServerPods = func(annotations map[string]string) []*corev1.Pod {
			var pods []*corev1.Pod
			for _, selectedNode := range selectedNodes {
				pod := composeAgnhostPod(
					"testpod-"+sanitizeNodeName(selectedNode.Name),
					namespace,
					selectedNode.Name,
					"netexec", "--http-port", "8000")
				pod.Annotations = annotations
				pods = append(pods, e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod))
			}
			return pods
		}

		iperfServerScript = `
#!/bin/bash -xe
iface=$(ifconfig  |grep flags |grep -v "eth0\|lo" | sed "s/: .*//")
iface=${iface:-eth0}

ipv4=$(ifconfig $iface | grep "inet "|awk '{print $2}'| sed "s#/.*##")
if [ "$ipv4" != "" ]; then
	iperf3 -s -D --bind $ipv4 --logfile /tmp/test_${ipv4}_iperf3.log
	sleep 1
	if grep "iperf3: error" /tmp/test_${ipv4}_iperf3.log; then
		cat /tmp/test_${ipv4}_iperf3.log
		exit 1
	fi
fi

cnt=0
while [ "$ipv6" == "" -a $cnt -lt 10 ]; do
	ipv6=$(ifconfig $iface | grep inet6 |grep -v fe80 |awk '{print $2}'| sed "s#/.*##")
	sleep 1
	cnt=$((cnt+1))
done
if [ "$ipv6" != "" ]; then
	iperf3 -s -D --bind $ipv6 --logfile /tmp/test_${ipv6}_iperf3.log
	sleep 1
	if grep "iperf3: error" /tmp/test_${ipv6}_iperf3.log; then
		cat /tmp/test_${ipv6}_iperf3.log 1>&2
		exit 1
	fi
fi
`
		nextIPs = func(idx int, subnets []string) ([]string, error) {
			var ips []string
			for _, subnet := range subnets {
				ip, ipNet, err := net.ParseCIDR(subnet)
				if err != nil {
					return nil, err
				}
				for range idx {
					ip = iputils.NextIP(ip)
				}
				ipNet.IP = ip
				ips = append(ips, ipNet.String())
			}
			return ips, nil
		}

		createIperfServerPods = func(nodes []corev1.Node, udnName string, role udnv1.NetworkRole, staticSubnets []string) ([]*corev1.Pod, error) {
			var pods []*corev1.Pod
			for i, node := range nodes {
				var nse *nadapi.NetworkSelectionElement
				if role != udnv1.NetworkRolePrimary {
					staticIPs, err := nextIPs(i, staticSubnets)
					if err != nil {
						return nil, err
					}
					nse = &nadapi.NetworkSelectionElement{
						Name:      udnName,
						IPRequest: staticIPs,
					}
				}
				pod, err := createPod(fr, "testpod-"+sanitizeNodeName(node.Name), node.Name, namespace, []string{"bash", "-c"}, map[string]string{}, func(pod *corev1.Pod) {
					if nse != nil {
						pod.Annotations = networkSelectionElements(*nse)
					}
					pod.Spec.Containers[0].Image = images.IPerf3()
					pod.Spec.Containers[0].Args = []string{iperfServerScript + "\n sleep infinity"}
				})
				if err != nil {
					return nil, err
				}
				pods = append(pods, pod)
			}
			return pods, nil
		}

		waitForPodsCondition = func(pods []*corev1.Pod, conditionFn func(g Gomega, pod *corev1.Pod)) {
			for _, pod := range pods {
				Eventually(func(g Gomega) {
					var err error
					pod, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					conditionFn(g, pod)
				}).
					WithTimeout(time.Minute).
					WithPolling(time.Second).
					Should(Succeed())
			}
		}

		updatePods = func(pods []*corev1.Pod) []*corev1.Pod {
			for i, pod := range pods {
				var err error
				pod, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				pods[i] = pod
			}
			return pods
		}

		prepareHTTPServerPods = func(annotations map[string]string, conditionFn func(g Gomega, pod *corev1.Pod)) {
			By("Preparing HTTP server pods")
			httpServerTestPods = createHTTPServerPods(annotations)
			waitForPodsCondition(httpServerTestPods, conditionFn)
			httpServerTestPods = updatePods(httpServerTestPods)
		}

		removeImagesInNode = func(node, imageURL string) error {
			By("Removing unused images in node " + node)
			output, err := infraprovider.Get().ExecK8NodeCommand(node, []string{
				"crictl", "images", "-o", "json",
			})
			if err != nil {
				return err
			}

			// Remove tag if exists.
			taglessImageURL := strings.Split(imageURL, ":")[0]
			imageID, err := images.ImageIDByImageURL(taglessImageURL, output)
			if err != nil {
				return err
			}
			if imageID != "" {
				_, err = infraprovider.Get().ExecK8NodeCommand(node, []string{
					"crictl", "rmi", imageID,
				})
				if err != nil {
					return err
				}
				_, err = infraprovider.Get().ExecK8NodeCommand(node, []string{
					"crictl", "rmi", "--prune",
				})
				if err != nil {
					return err
				}
			}
			return nil
		}

		removeImagesInNodes = func(imageURL string) error {
			nodesList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for nodeIdx := range nodesList.Items {
				err = removeImagesInNode(nodesList.Items[nodeIdx].Name, imageURL)
				if err != nil {
					return err
				}
			}
			return nil
		}

		createCUDN = func(cudn *udnv1.ClusterUserDefinedNetwork) {
			GinkgoHelper()
			By("Creating ClusterUserDefinedNetwork")
			Expect(crClient.Create(context.Background(), cudn)).To(Succeed())
			DeferCleanup(func() {
				if e2eframework.TestContext.DeleteNamespace && (e2eframework.TestContext.DeleteNamespaceOnFailure || !CurrentSpecReport().Failed()) {
					crClient.Delete(context.Background(), cudn)
				}
			})
			Eventually(clusterUserDefinedNetworkReadyFunc(fr.DynamicClient, cudn.Name), 5*time.Second, time.Second).Should(Succeed())
		}

		createRA = func(ra *rav1.RouteAdvertisements) {
			GinkgoHelper()
			By("Creating RouteAdvertisements")
			Expect(crClient.Create(context.Background(), ra)).To(Succeed())
			DeferCleanup(func() {
				if e2eframework.TestContext.DeleteNamespace && (e2eframework.TestContext.DeleteNamespaceOnFailure || !CurrentSpecReport().Failed()) {
					crClient.Delete(context.Background(), ra)
				}
			})

			By("ensure route advertisement matching CUDN was created successfully")
			Eventually(func(g Gomega) string {
				Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(ra), ra)).To(Succeed())
				return ra.Status.Status
			}, 30*time.Second, time.Second).Should(Equal("Accepted"))
		}

		getCUDNSubnets = func(cudn *udnv1.ClusterUserDefinedNetwork) []string {
			nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.TODO(), cudn.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			var result map[string]interface{}
			err = json.Unmarshal([]byte(nad.Spec.Config), &result)
			Expect(err).NotTo(HaveOccurred())
			return strings.Split(result["subnets"].(string), ",")
		}
	)
	BeforeEach(func() {
		// So we can use it at AfterEach, since fr.ClientSet is nil there
		clientSet = fr.ClientSet
		providerCtx = infraprovider.Get().NewTestContext()

		var err error
		crClient, err = newControllerRuntimeClient()
		Expect(err).NotTo(HaveOccurred())

		virtClient, err = kubevirt.NewClient("/tmp")
		Expect(err).NotTo(HaveOccurred())

		nadClient, err = nadclient.NewForConfig(fr.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with default pod network", Ordered, func() {
		BeforeEach(func() {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework": fr.BaseName,
			})
			fr.Namespace = ns
			namespace = fr.Namespace.Name
			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			nodesByOVNZone := map[string][]corev1.Node{}
			for _, workerNode := range workerNodeList.Items {
				ovnZone, ok := workerNode.Labels["k8s.ovn.org/zone-name"]
				if !ok {
					ovnZone = "global"
				}
				_, ok = nodesByOVNZone[ovnZone]
				if !ok {
					nodesByOVNZone[ovnZone] = []corev1.Node{}
				}
				nodesByOVNZone[ovnZone] = append(nodesByOVNZone[ovnZone], workerNode)
			}

			selectedNodes = []corev1.Node{}
			// If there is one global zone select the first three for the
			// migration
			if len(nodesByOVNZone) == 1 {
				selectedNodes = []corev1.Node{
					workerNodeList.Items[0],
					workerNodeList.Items[1],
					workerNodeList.Items[2],
				}
				// Otherwise select a pair of nodes from different OVN zones
			} else {
				for _, nodes := range nodesByOVNZone {
					selectedNodes = append(selectedNodes, nodes[0])
					if len(selectedNodes) == 3 {
						break // we want just three of them
					}
				}
			}

			Expect(selectedNodes).To(HaveLen(3), "at least three nodes in different zones are needed for interconnect scenarios")

			// Label the selected nodes with the generated namespaces, so we can
			// configure VM nodeSelector with it and live migration will take only
			// them into consideration
			for _, node := range selectedNodes {
				e2enode.AddOrUpdateLabelOnNode(fr.ClientSet, node.Name, namespace, "true")
			}

			prepareHTTPServerPods(map[string]string{}, checkPodHasIPAtStatus)
		})

		AfterEach(func() {
			for _, node := range selectedNodes {
				e2enode.RemoveLabelOffNode(fr.ClientSet, node.Name, namespace)
			}
		})

		AfterAll(func() {
			Expect(removeImagesInNodes(kubevirt.FedoraCoreOSContainerDiskImage)).To(Succeed())
		})

		DescribeTable("when live migration", func(td liveMigrationTestData) {
			if td.mode == kubevirtv1.MigrationPostCopy && os.Getenv("GITHUB_ACTIONS") == "true" {
				Skip("Post copy live migration not working at github actions")
			}
			if td.mode == kubevirtv1.MigrationPostCopy && os.Getenv("KUBEVIRT_SKIP_MIGRATE_POST_COPY") == "true" {
				Skip("Post copy live migration explicitly skipped")
			}
			var err error

			Expect(err).NotTo(HaveOccurred())

			d.ConntrackDumpingDaemonSet()
			d.OVSFlowsDumpingDaemonSet(deploymentconfig.Get().ExternalBridgeName())
			d.IPTablesDumpingDaemonSet()

			bandwidthPerMigration := resource.MustParse("40Mi")
			forcePostCopyMigrationPolicy := &kvmigrationsv1alpha1.MigrationPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "force-post-copy",
				},
				Spec: kvmigrationsv1alpha1.MigrationPolicySpec{
					AllowPostCopy:           ptr.To(true),
					CompletionTimeoutPerGiB: ptr.To(int64(1)),
					BandwidthPerMigration:   &bandwidthPerMigration,
					Selectors: &kvmigrationsv1alpha1.Selectors{
						VirtualMachineInstanceSelector: kvmigrationsv1alpha1.LabelSelector{
							"test-live-migration": "post-copy",
						},
					},
				},
			}
			if td.mode == kubevirtv1.MigrationPostCopy {
				err = crClient.Create(context.TODO(), forcePostCopyMigrationPolicy)
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					Expect(crClient.Delete(context.TODO(), forcePostCopyMigrationPolicy)).To(Succeed())
				}()
			}

			vmLabels := map[string]string{}
			if td.mode == kubevirtv1.MigrationPostCopy {
				vmLabels = forcePostCopyMigrationPolicy.Spec.Selectors.VirtualMachineInstanceSelector
			}
			vms, err := composeDefaultNetworkLiveMigratableVMs(td.numberOfVMs, vmLabels)
			Expect(err).NotTo(HaveOccurred())

			for _, vm := range vms {
				By(fmt.Sprintf("Create virtual machine %s", vm.Name))
				vmCreationRetries := 0
				Eventually(func() error {
					if vmCreationRetries > 0 {
						// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
						// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
						By(fmt.Sprintf("Retrying vm %s creation", vm.Name))
					}
					err = crClient.Create(context.Background(), vm)
					vmCreationRetries++
					return err
				}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
			}

			for _, vm := range vms {
				By(fmt.Sprintf("Create virtual machine %s", vm.Name))
				if td.shouldExpectFailure {
					By("annotating the VMI with `fail fast`")
					vmKey := types.NamespacedName{Namespace: namespace, Name: vm.Name}
					var vmi kubevirtv1.VirtualMachineInstance

					Eventually(func() error {
						err = crClient.Get(context.TODO(), vmKey, &vmi)
						if err == nil {
							vmi.ObjectMeta.Annotations[kubevirtv1.FuncTestLauncherFailFastAnnotation] = "true"
							err = crClient.Update(context.TODO(), &vmi)
						}
						return err
					}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
				}
			}

			for _, vm := range vms {
				By(fmt.Sprintf("Waiting for readiness at virtual machine %s", vm.Name))
				Eventually(func() bool {
					err = crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm), vm)
					Expect(err).NotTo(HaveOccurred())
					return vm.Status.Ready
				}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(BeTrue())
			}
			wg.Add(int(td.numberOfVMs))
			for _, vm := range vms {
				go runLiveMigrationTest(td, vm)
			}
			wg.Wait()
		},
			Entry("with pre-copy succeeds, should keep connectivity", liveMigrationTestData{
				mode:        kubevirtv1.MigrationPreCopy,
				numberOfVMs: 1,
			}),
			Entry("with post-copy succeeds, should keep connectivity", liveMigrationTestData{
				mode:        kubevirtv1.MigrationPostCopy,
				numberOfVMs: 1,
			}),
			Entry("with pre-copy fails, should keep connectivity", liveMigrationTestData{
				mode:                kubevirtv1.MigrationPreCopy,
				numberOfVMs:         1,
				shouldExpectFailure: true,
			}),
		)
	})
	Context("with user defined networks and persistent ips configured", Ordered, func() {
		AfterAll(func() {
			Expect(removeImagesInNodes(kubevirt.FedoraContainerDiskImage)).To(Succeed())
		})
		type testCommand struct {
			description string
			cmd         func()
		}
		type resourceCommand struct {
			description string
			cmd         func() string
		}
		var (
			cudn       *udnv1.ClusterUserDefinedNetwork
			vm         *kubevirtv1.VirtualMachine
			vmi        *kubevirtv1.VirtualMachineInstance
			cidrIPv4   = "172.31.0.0/24" // subnet in private range 172.16.0.0/12 (rfc1918)
			cidrIPv6   = "2010:100:200::0/60"
			staticIPv4 = "172.31.0.101"
			staticIPv6 = "2010:100:200::101"
			staticMAC  = "02:00:00:00:00:01"
			restart    = testCommand{
				description: "restart",
				cmd: func() {
					By("Restarting vm")
					output, err := virtClient.RestartVirtualMachine(vmi)
					Expect(err).NotTo(HaveOccurred(), output)

					By("Wait some time to vmi conditions to catch up after restart")
					time.Sleep(3 * time.Second)

					waitVirtualMachineInstanceReadiness(vmi)
				},
			}
			liveMigrate = testCommand{
				description: "live migration",
				cmd: func() {
					liveMigrateSucceed(vmi)
				},
			}
			liveMigrateFailed = testCommand{
				description: "live migration failed",
				cmd: func() {
					liveMigrateFailed(vmi)
				},
			}
			// For secondary network interfaces:
			// - DHCPv6 cannot be activated because KubeVirt does not support it.
			// - In Fedora 39, the "may-fail" option is configured by cloud-init in
			//   NetworkManager. This causes the entire interface to remain inactive
			//   if no IPv6 address is assigned.
			networkDataIPv4 = `version: 2
ethernets:
  eth0:
    dhcp4: true
`

			networkDataDualStack = `version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`
			userData = `
#cloud-config
password: fedora
chpasswd: { expire: False }
`

			userDataWithIperfServer = userData + fmt.Sprintf(`
write_files:
  - path: /tmp/iperf-server.sh
    encoding: b64
    content: %s
    permissions: '0755'
`, base64.StdEncoding.EncodeToString([]byte(iperfServerScript)))

			virtualMachine = resourceCommand{
				description: "VirtualMachine",
				cmd: func() string {
					vm = fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
						Multus: &kubevirtv1.MultusNetwork{
							NetworkName: cudn.Name,
						},
					}, userData, networkDataIPv4)
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineWithUDN = resourceCommand{
				description: "VirtualMachine with interface binding for UDN",
				cmd: func() string {
					vm = fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineWithUDNAndStaticIPsAndMAC = resourceCommand{
				description: "VirtualMachine with interface binding for UDN and statics IPs and MAC",
				cmd: func() string {
					GinkgoHelper()
					if !isPreConfiguredUdnAddressesEnabled() {
						Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
					}

					annotations, err := kubevirt.GenerateAddressesAnnotations("net1", filterIPs(fr.ClientSet, staticIPv4, staticIPv6))
					Expect(err).NotTo(HaveOccurred())

					vm = fedoraWithTestToolingVM(nil /*labels*/, annotations, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = staticMAC
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineInstance = resourceCommand{
				description: "VirtualMachineInstance",
				cmd: func() string {
					vmi = fedoraWithTestToolingVMI(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
						Multus: &kubevirtv1.MultusNetwork{
							NetworkName: cudn.Name,
						},
					}, userData, networkDataIPv4)
					createVirtualMachineInstance(vmi)
					return vmi.Name
				},
			}

			virtualMachineInstanceWithUDN = resourceCommand{
				description: "VirtualMachineInstance with interface binding for UDN",
				cmd: func() string {
					vmi = fedoraWithTestToolingVMI(nil /*labels*/, nil /*annotations*/, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vmi.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vmi.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					createVirtualMachineInstance(vmi)
					return vmi.Name
				},
			}

			filterOutIPv6 = func(ips map[string][]string) map[string][]string {
				filteredOutIPs := map[string][]string{}
				for podName, podIPs := range ips {
					for _, podIP := range podIPs {
						if !utilnet.IsIPv6String(podIP) {
							_, ok := filteredOutIPs[podName]
							if !ok {
								filteredOutIPs[podName] = []string{}
							}
							filteredOutIPs[podName] = append(filteredOutIPs[podName], podIP)
						}
					}
				}
				return filteredOutIPs
			}
		)
		type testData struct {
			description string
			resource    resourceCommand
			test        testCommand
			topology    udnv1.NetworkTopology
			role        udnv1.NetworkRole
			ingress     string
			ipRequests  []string
			macRequest  string
		}
		var (
			containerNetwork = func(td testData) (infraapi.Network, error) {
				if td.ingress == "routed" {
					return infraprovider.Get().GetNetwork("bgpnet")
				}
				return infraprovider.Get().PrimaryNetwork()
			}
			exposeVMIperfServer = func(td testData, vmi *kubevirtv1.VirtualMachineInstance, vmiAddresses []string) ([]string, int32) {
				GinkgoHelper()
				if td.ingress == "routed" {
					return vmiAddresses, iperf3DefaultPort
				}
				step := by(vmi.Name, "Expose VM iperf server as a service")
				svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("iperf3-vm-server", vmi.Name, iperf3DefaultPort), metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(svc.Spec.Ports[0].NodePort).NotTo(Equal(0), step)
				serverPort := svc.Spec.Ports[0].NodePort
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), fr.ClientSet, 1)
				Expect(err).NotTo(HaveOccurred())
				serverIPs := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
				return serverIPs, serverPort
			}
		)
		DescribeTable("should keep ip", func(td testData) {
			if td.role == "" {
				td.role = udnv1.NetworkRoleSecondary
			}
			if td.role == udnv1.NetworkRolePrimary && !isInterconnectEnabled() {
				const upstreamIssue = "https://github.com/ovn-org/ovn-kubernetes/issues/4528"
				e2eskipper.Skipf(
					"The egress check of tests are known to fail on non-IC deployments. Upstream issue: %s", upstreamIssue,
				)
			}

			l := map[string]string{
				"e2e-framework": fr.BaseName,
			}
			if td.role == udnv1.NetworkRolePrimary {
				l[RequiredUDNNamespaceLabel] = ""
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			networkName := ""
			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, networkName = kubevirt.GenerateCUDN(namespace, "net1", td.topology, td.role, dualCIDRs)

			if td.topology == udnv1.NetworkTopologyLocalnet {
				By("setting up the localnet underlay")
				Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())
			}
			createCUDN(cudn)

			if td.ingress == "routed" {
				createRA(&rav1.RouteAdvertisements{
					ObjectMeta: metav1.ObjectMeta{
						Name: cudn.Name,
					},
					Spec: rav1.RouteAdvertisementsSpec{
						Advertisements: []rav1.AdvertisementType{rav1.PodNetwork},
						NetworkSelectors: crdtypes.NetworkSelectors{{
							NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"name": cudn.Name},
								},
							},
						}},
					},
				})
			}

			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			selectedNodes = workerNodeList.Items
			Expect(selectedNodes).NotTo(BeEmpty())

			iperfServerTestPods, err = createIperfServerPods(selectedNodes, cudn.Name, td.role, []string{})
			Expect(err).NotTo(HaveOccurred())

			var externalContainer infraapi.ExternalContainer
			if td.role == udnv1.NetworkRolePrimary {
				providerNetwork, err := containerNetwork(td)
				Expect(err).ShouldNot(HaveOccurred(), "primary network must be available to attach containers")
				externalContainerPort := infraprovider.Get().GetExternalContainerPort()
				externalContainerName := namespace + "-iperf"
				externalContainerSpec := infraapi.ExternalContainer{
					Name:    externalContainerName,
					Image:   images.IPerf3(),
					Network: providerNetwork,
					CmdArgs: []string{"sleep infinity"},
					ExtPort: externalContainerPort,
				}
				externalContainer, err = providerCtx.CreateExternalContainer(externalContainerSpec)
				Expect(err).ShouldNot(HaveOccurred(), "creation of external container is test dependency")
			}

			var externalContainerIPs []string
			if externalContainer.IsIPv4() {
				externalContainerIPs = append(externalContainerIPs, externalContainer.IPv4)
			}
			if externalContainer.IsIPv6() {
				externalContainerIPs = append(externalContainerIPs, externalContainer.IPv6)
			}

			if td.ingress == "routed" {
				// pre=created test dependency and therefore we dont delete
				frrExternalContainer := infraapi.ExternalContainer{Name: "frr"}
				frrNetwork, err := containerNetwork(td)
				Expect(err).NotTo(HaveOccurred())
				frrExternalContainerInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(frrExternalContainer, frrNetwork)
				Expect(err).NotTo(HaveOccurred(), "must fetch FRR container network interface attached to secondary network")

				output, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"bash", "-c", fmt.Sprintf(`
set -xe
dnf install -y iproute
ip route add %[1]s via %[2]s
ip route add %[3]s via %[4]s
`, cidrIPv4, frrExternalContainerInterface.GetIPv4(), cidrIPv6, frrExternalContainerInterface.GetIPv6())})
				Expect(err).NotTo(HaveOccurred(), output)
			}

			vmiName := td.resource.cmd()
			vmi = &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmiName,
				},
			}

			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step := by(vmi.Name, "Login to virtual machine for the first time")
			Eventually(func() error {
				return virtClient.LoginToFedora(vmi, "fedora", "fedora")
			}).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(Succeed(), step)

			// expect 2 addresses on dual-stack deployments; 1 on single-stack
			step = by(vmi.Name, "Wait for addresses at the virtual machine")
			expectedNumberOfAddresses := len(dualCIDRs)
			expectedAddreses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)
			if _, hasIPRequests := vmi.Annotations[kubevirt.AddressesAnnotation]; hasIPRequests {
				Expect(expectedAddreses).To(ConsistOf(filterIPs(fr.ClientSet, staticIPv4, staticIPv6)), "expected addresses should be consistent with the static IPs")
			}
			if vmi.Spec.Domain.Devices.Interfaces[0].MacAddress != "" {
				Expect(vmi.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal(vmi.Status.Interfaces[0].MAC), "expected mac address should be consistent with the static MAC")
			}
			expectedAddresesAtGuest := expectedAddreses
			testPodsIPs := podsMultusNetworkIPs(iperfServerTestPods, podNetworkStatusByNetConfigPredicate(namespace, cudn.Name, strings.ToLower(string(td.role))))

			serverIPs, serverPort := exposeVMIperfServer(td, vmi, expectedAddreses)

			// IPv6 is not support for secondaries with IPAM so guest will
			// have only ipv4.
			if td.role != udnv1.NetworkRolePrimary {
				expectedAddresesAtGuest, err = util.MatchAllIPStringFamily(false /*ipv4*/, expectedAddreses)
				Expect(err).NotTo(HaveOccurred())
				testPodsIPs = filterOutIPv6(testPodsIPs)
			}
			Expect(testPodsIPs).NotTo(BeEmpty())

			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(expectedAddresesAtGuest), step)

			step = by(vmi.Name, fmt.Sprintf("Check east/west traffic before %s %s", td.resource.description, td.test.description))
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			if td.role == udnv1.NetworkRolePrimary {
				if isIPv6Supported(fr.ClientSet) && isInterconnectEnabled() {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv6 gateway before %s %s", td.resource.description, td.test.description))

					expectedIPv6GatewayPath, err := kubevirt.GenerateGatewayIPv6RouterLLA(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred())
					Eventually(kubevirt.RetrieveIPv6Gateways).
						WithArguments(virtClient, vmi).
						WithTimeout(5*time.Second).
						WithPolling(time.Second).
						Should(Equal([]string{expectedIPv6GatewayPath}), "should filter remote ipv6 gateway nexthop")
				}
				step = by(vmi.Name, fmt.Sprintf("Check north/south traffic before %s %s", td.resource.description, td.test.description))
				output, err := virtClient.RunCommand(vmi, "/tmp/iperf-server.sh", time.Minute)
				Expect(err).NotTo(HaveOccurred(), step+": "+output)
				Expect(startNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)).To(Succeed())
				checkNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)
				checkNorthSouthEgressICMPTraffic(vmi, externalContainerIPs, step)
				if td.ingress == "routed" {
					_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"bash", "-c", iperfServerScript})
					Expect(err).NotTo(HaveOccurred(), step)
					Expect(startNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)).To(Succeed())
					By("Check egress src ip is not node IP on 'routed' ingress mode")
					for _, vmAddress := range expectedAddreses {
						output, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
							"bash", "-c", fmt.Sprintf("grep 'connected to %s' /tmp/test_*", vmAddress),
						})
						Expect(err).NotTo(HaveOccurred(), step+": "+output)
					}
					checkNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)
				}
			}

			by(vmi.Name, fmt.Sprintf("Running %s for %s", td.test.description, td.resource.description))
			td.test.cmd()

			step = by(vmi.Name, fmt.Sprintf("Login to virtual machine after %s %s", td.resource.description, td.test.description))
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			obtainedAddresses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)

			Expect(obtainedAddresses).To(Equal(expectedAddreses))
			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(expectedAddresesAtGuest), step)

			step = by(vmi.Name, fmt.Sprintf("Check east/west traffic after %s %s", td.resource.description, td.test.description))
			if td.test.description == restart.description {
				// At restart we need re-connect
				Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
				if td.role == udnv1.NetworkRolePrimary {
					output, err := virtClient.RunCommand(vmi, "/tmp/iperf-server.sh &", time.Minute)
					Expect(err).NotTo(HaveOccurred(), step+": "+output)
					Expect(startNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)).To(Succeed())
				}
			}
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)
			if td.role == udnv1.NetworkRolePrimary {
				step = by(vmi.Name, fmt.Sprintf("Check north/south traffic after %s %s", td.resource.description, td.test.description))
				checkNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)
				checkNorthSouthEgressICMPTraffic(vmi, externalContainerIPs, step)
				if td.ingress == "routed" {
					checkNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)
				}
			}

			if td.role == udnv1.NetworkRolePrimary && td.test.description == liveMigrate.description && isInterconnectEnabled() {
				if isIPv4Supported(fr.ClientSet) {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv4 gateway cached mac after %s %s", td.resource.description, td.test.description))
					Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

					expectedGatewayMAC, err := kubevirt.GenerateGatewayMAC(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred(), step)

					Expect(err).NotTo(HaveOccurred(), step)
					Eventually(kubevirt.RetrieveCachedGatewayMAC).
						WithArguments(virtClient, vmi, "enp1s0", cidrIPv4).
						WithTimeout(10*time.Second).
						WithPolling(time.Second).
						Should(Equal(expectedGatewayMAC), step)
				}
				if isIPv6Supported(fr.ClientSet) {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv6 gateway after %s %s", td.resource.description, td.test.description))

					targetNodeIPv6GatewayPath, err := kubevirt.GenerateGatewayIPv6RouterLLA(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred())
					Eventually(kubevirt.RetrieveIPv6Gateways).
						WithArguments(virtClient, vmi).
						WithTimeout(5*time.Second).
						WithPolling(time.Second).
						Should(Equal([]string{targetNodeIPv6GatewayPath}), "should reconcile ipv6 gateway nexthop after live migration")
				}
			}
		},
			func(td testData) string {
				role := udnv1.NetworkRoleSecondary
				if td.role != "" {
					role = td.role
				}
				ingress := "snat"
				if td.ingress != "" {
					ingress = td.ingress
				}
				return fmt.Sprintf("after %s of %s with %s/%s with %s ingress", td.test.description, td.resource.description, role, td.topology, ingress)
			},
			Entry(nil, testData{
				resource: virtualMachine,
				test:     restart,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstanceWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstanceWithUDN,
				test:     liveMigrateFailed,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrateFailed,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
		)
	})
	Context("with kubevirt VM using layer2 UDPN", Ordered, func() {
		var (
			podName                 = "virt-launcher-vm1"
			cidrIPv4                = "172.31.0.0/24"
			cidrIPv6                = "2010:100:200::/60"
			primaryUDNNetworkStatus nadapi.NetworkStatus
			virtLauncherCommand     = func(command string) (string, error) {
				stdout, stderr, err := ExecShellInPodWithFullOutput(fr, namespace, podName, command)
				if err != nil {
					return "", fmt.Errorf("%s: %s: %w", stdout, stderr, err)
				}
				return stdout, nil
			}
			primaryUDNValueFor = func(ty, field string) ([]string, error) {
				output, err := virtLauncherCommand(fmt.Sprintf(`nmcli -e no -g %s %s show ovn-udn1`, field, ty))
				if err != nil {
					return nil, err
				}
				return strings.Split(output, " | "), nil
			}
			primaryUDNValueForConnection = func(field string) ([]string, error) {
				return primaryUDNValueFor("connection", field)
			}
			primaryUDNValueForDevice = func(field string) ([]string, error) {
				return primaryUDNValueFor("device", field)
			}
		)
		AfterAll(func() {
			Expect(removeImagesInNodes(kubevirt.FakeLauncherImage)).To(Succeed())
		})
		BeforeEach(func() {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			fr.Namespace = ns
			namespace = fr.Namespace.Name
			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			cudn.Spec.Network.Layer2.MTU = 1300
			createCUDN(cudn)

			By("Create virt-launcher pod")
			kubevirtPod := kubevirt.GenerateFakeVirtLauncherPod(namespace, "vm1")
			Expect(crClient.Create(context.Background(), kubevirtPod)).To(Succeed())

			By("Wait for virt-launcher pod to be ready and primary UDN network status to pop up")
			waitForPodsCondition([]*corev1.Pod{kubevirtPod}, func(g Gomega, pod *corev1.Pod) {
				ok, err := testutils.PodRunningReady(pod)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ok).To(BeTrue())

				primaryUDNNetworkStatuses, err := podNetworkStatus(pod, func(networkStatus nadapi.NetworkStatus) bool {
					return networkStatus.Default
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(primaryUDNNetworkStatuses).To(HaveLen(1))
				primaryUDNNetworkStatus = primaryUDNNetworkStatuses[0]
			})

			By("Wait NetworkManager readiness")
			Eventually(func() error {
				_, err := virtLauncherCommand("systemctl is-active NetworkManager")
				return err
			}).
				WithTimeout(5 * time.Second).
				WithPolling(time.Second).
				Should(Succeed())

			By("Reconfigure primary UDN interface to use dhcp/nd for ipv4 and ipv6")
			_, err = virtLauncherCommand(kubevirt.GenerateAddressDiscoveryConfigurationCommand("ovn-udn1"))
			Expect(err).NotTo(HaveOccurred())
		})
		It("should configure IPv4 and IPv6 using DHCP and NDP", func() {
			dnsService, err := fr.ClientSet.CoreV1().Services(config.Kubernetes.DNSServiceNamespace).
				Get(context.Background(), config.Kubernetes.DNSServiceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			if isIPv4Supported(fr.ClientSet) {
				expectedIP, err := matchIPv4StringFamily(primaryUDNNetworkStatus.IPs)
				Expect(err).NotTo(HaveOccurred())

				expectedDNS, err := matchIPv4StringFamily(dnsService.Spec.ClusterIPs)
				Expect(err).NotTo(HaveOccurred())

				_, cidr, err := net.ParseCIDR(cidrIPv4)
				Expect(err).NotTo(HaveOccurred())
				expectedGateway := util.GetNodeGatewayIfAddr(cidr).IP.String()

				Eventually(primaryUDNValueForConnection).
					WithArguments("DHCP4.OPTION").
					WithTimeout(10 * time.Second).
					WithPolling(time.Second).
					Should(ContainElements(
						"host_name = vm1",
						fmt.Sprintf("ip_address = %s", expectedIP),
						fmt.Sprintf("domain_name_servers = %s", expectedDNS),
						fmt.Sprintf("routers = %s", expectedGateway),
						fmt.Sprintf("interface_mtu = 1300"),
					))
				Expect(primaryUDNValueForConnection("IP4.ADDRESS")).To(ConsistOf(expectedIP + "/24"))
				Expect(primaryUDNValueForConnection("IP4.GATEWAY")).To(ConsistOf(expectedGateway))
				Expect(primaryUDNValueForConnection("IP4.DNS")).To(ConsistOf(expectedDNS))
				Expect(primaryUDNValueForDevice("GENERAL.MTU")).To(ConsistOf("1300"))
			}

			if isIPv6Supported(fr.ClientSet) {
				expectedIP, err := matchIPv6StringFamily(primaryUDNNetworkStatus.IPs)
				Expect(err).NotTo(HaveOccurred())
				Eventually(primaryUDNValueFor).
					WithArguments("connection", "DHCP6.OPTION").
					WithTimeout(10 * time.Second).
					WithPolling(time.Second).
					Should(ContainElements(
						"fqdn_fqdn = vm1",
						fmt.Sprintf("ip6_address = %s", expectedIP),
					))
				Expect(primaryUDNValueForConnection("IP6.ADDRESS")).To(SatisfyAll(HaveLen(2), ContainElements(expectedIP+"/128")))
				Expect(primaryUDNValueForConnection("IP6.GATEWAY")).To(ConsistOf(WithTransform(func(ipv6 string) bool {
					return netip.MustParseAddr(ipv6).IsLinkLocalUnicast()
				}, BeTrue())))
				Expect(primaryUDNValueForConnection("IP6.ROUTE")).To(ContainElement(ContainSubstring(fmt.Sprintf("dst = %s", cidrIPv6))))
				Expect(primaryUDNValueForDevice("GENERAL.MTU")).To(ConsistOf("1300"))
			}
		})
	})
	Context("with user defined networks with ipamless localnet topology", Ordered, func() {
		BeforeEach(func() {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework": fr.BaseName,
			})
			Expect(err).ToNot(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
		})
		AfterAll(func() {
			Expect(removeImagesInNodes(kubevirt.FedoraContainerDiskImage)).To(Succeed())
		})
		var (
			ipv4CIDR             = "172.31.0.0/24"
			ipv6CIDR             = "2010:100:200::0/60"
			vmiIPv4              = "172.31.0.100/24"
			vmiIPv6              = "2010:100:200::100/60"
			vmiMAC               = "0A:58:0A:80:00:64"
			staticIPsNetworkData = func(ips []string) (string, error) {
				type Ethernet struct {
					DHCP4     *bool    `json:"dhcp4,omitempty"`
					DHCP6     *bool    `json:"dhcp6,omitempty"`
					Addresses []string `json:"addresses,omitempty"`
				}
				networkData, err := yaml.Marshal(&struct {
					Version   int                 `json:"version,omitempty"`
					Ethernets map[string]Ethernet `json:"ethernets,omitempty"`
				}{
					Version: 2,
					Ethernets: map[string]Ethernet{
						"eth0": {
							DHCP4:     ptr.To(false),
							DHCP6:     ptr.To(false),
							Addresses: ips,
						},
					},
				})
				if err != nil {
					return "", err
				}
				return string(networkData), nil
			}

			userData = `#cloud-config
password: fedora
chpasswd: { expire: False }
`
		)
		DescribeTable("should maintain tcp connection with minimal downtime", func(td func(vmi *kubevirtv1.VirtualMachineInstance)) {
			By("setting up the localnet underlay")
			cudn, networkName := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLocalnet, udnv1.NetworkRoleSecondary, udnv1.DualStackCIDRs{})
			createCUDN(cudn)

			Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())

			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			selectedNodes = workerNodeList.Items
			Expect(selectedNodes).NotTo(BeEmpty())

			iperfServerTestPods, err = createIperfServerPods(selectedNodes, cudn.Name, cudn.Spec.Network.Localnet.Role, filterCIDRs(fr.ClientSet, ipv4CIDR, ipv6CIDR))
			Expect(err).NotTo(HaveOccurred())

			filteredCIDRs := filterCIDRs(fr.ClientSet, vmiIPv4, vmiIPv6)
			networkData, err := staticIPsNetworkData(filteredCIDRs)
			Expect(err).NotTo(HaveOccurred())

			vm := fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
				Multus: &kubevirtv1.MultusNetwork{
					NetworkName: cudn.Name,
				},
			}, userData, networkData)
			// Harcode mac address so it's the same after live migration
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = vmiMAC
			createVirtualMachine(vm)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step := by(vmi.Name, "Login to virtual machine for the first time")
			Eventually(func() error {
				return virtClient.LoginToFedora(vmi, "fedora", "fedora")
			}).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(Succeed(), step)

			step = by(vmi.Name, "Wait for cloud init to finish at first boot")
			output, err := virtClient.RunCommand(vmi, "cloud-init status --wait", time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			testPodsIPs := podsMultusNetworkIPs(iperfServerTestPods, podNetworkStatusByNetConfigPredicate(namespace, cudn.Name, strings.ToLower(string(cudn.Spec.Network.Localnet.Role))))
			Expect(testPodsIPs).NotTo(BeEmpty())

			step = by(vmi.Name, "Check east/west traffic before virtual machine instance live migration")
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			by(vmi.Name, "Running live migration for virtual machine instance")
			td(vmi)

			// Update vmi status after live migration
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step = by(vmi.Name, "Login to virtual machine after virtual machine instance live migration")
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			step = by(vmi.Name, "Check east/west traffic after virtual machine instance live migration")
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			By("Stop iperf3 traffic before force killing vm, so iperf3 server do not get stuck")
			output, err = virtClient.RunCommand(vmi, "killall --wait iperf3", 5*time.Second)
			Expect(err).ToNot(HaveOccurred(), output)

			step = by(vmi.Name, fmt.Sprintf("Force kill qemu at node %q where VM is running on", vmi.Status.NodeName))
			Expect(kubevirt.ForceKillVirtLauncherAtNode(infraprovider.Get(), vmi.Status.NodeName, vmi.Namespace, vmi.Name)).To(Succeed(), step)

			step = by(vmi.Name, "Waiting for failed restarted VMI to reach ready state")
			waitVirtualMachineInstanceFailed(vmi)
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed(), step)

			step = by(vmi.Name, "Login to virtual machine after virtual machine instance force killed")
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			step = by(vmi.Name, "Wait for cloud init to finish after vm restart")
			output, err = virtClient.RunCommand(vmi, "cloud-init status --wait", time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			step = by(vmi.Name, "Verify static IP is configured after vm restart")
			filteredIPs := []string{}
			for _, filteredCIDR := range filteredCIDRs {
				filteredIPs = append(filteredIPs, strings.Split(filteredCIDR, "/")[0])
			}
			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(filteredIPs), step)

			step = by(vmi.Name, "Restart iperf traffic after forcing a vm failure")
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			by(vmi.Name, "Running live migration after forcing vm failure")
			td(vmi)

			step = by(vmi.Name, "Check east/west traffic for failed virtual machine after live migration")
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)
		},
			Entry("after succeeded live migration", liveMigrateSucceed),
			Entry("after failed live migration", liveMigrateFailed),
		)
	})

	getIPAMClaimName := func(vmName, netName string) string {
		return fmt.Sprintf("%s.%s", vmName, netName)
	}

	verifyIPAMClaimStatusSuccess := func(ipamClaimName string) {
		Eventually(func(g Gomega) {
			ipamClaim := &ipamclaimsv1alpha1.IPAMClaim{}
			g.Expect(crClient.Get(context.Background(), crclient.ObjectKey{
				Namespace: namespace,
				Name:      ipamClaimName,
			}, ipamClaim)).To(Succeed(), "Should get IPAMClaim")

			g.Expect(ipamClaim.Status.OwnerPod).NotTo(BeNil(), "OwnerPod should be set")
			g.Expect(ipamClaim.Status.OwnerPod.Name).To(HavePrefix("virt-launcher-"), "OwnerPod should be the virt-launcher pod")

			g.Expect(ipamClaim.Status.Conditions).NotTo(BeEmpty(), "Conditions should be set")
			condition := meta.FindStatusCondition(ipamClaim.Status.Conditions, "IPsAllocated")
			g.Expect(condition).NotTo(BeNil(), "IPsAllocated condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "Condition status should be True")
			g.Expect(condition.Reason).To(Equal("SuccessfulAllocation"), "Condition reason should be SuccessfulAllocation")

			g.Expect(ipamClaim.Status.IPs).NotTo(BeEmpty(), "IPs should be set on successful allocation")
		}).
			WithTimeout(30*time.Second).
			WithPolling(2*time.Second).
			Should(Succeed(), fmt.Sprintf("IPAMClaim %s should have expected successful status", ipamClaimName))
	}

	verifyIPAMClaimStatusFailure := func(ipamClaimName string, expectedReason string) {
		Eventually(func(g Gomega) {
			ipamClaim := &ipamclaimsv1alpha1.IPAMClaim{}
			g.Expect(crClient.Get(context.Background(), crclient.ObjectKey{
				Namespace: namespace,
				Name:      ipamClaimName,
			}, ipamClaim)).To(Succeed(), "Should get IPAMClaim")

			g.Expect(ipamClaim.Status.OwnerPod).NotTo(BeNil(), "OwnerPod should be set")
			g.Expect(ipamClaim.Status.OwnerPod.Name).To(HavePrefix("virt-launcher-"), "OwnerPod should be the virt-launcher pod")

			g.Expect(ipamClaim.Status.Conditions).NotTo(BeEmpty(), "Conditions should be set")
			condition := meta.FindStatusCondition(ipamClaim.Status.Conditions, "IPsAllocated")
			g.Expect(condition).NotTo(BeNil(), "IPsAllocated condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse), "Condition status should be False")
			g.Expect(condition.Reason).To(Equal(expectedReason), "Condition reason should match")

			g.Expect(ipamClaim.Status.IPs).To(BeEmpty(), "IPs should not be set on failed allocation")
		}).
			WithTimeout(30*time.Second).
			WithPolling(2*time.Second).
			Should(Succeed(), fmt.Sprintf("IPAMClaim %s should have expected failure status", ipamClaimName))
	}

	Context("duplicate addresses validation", func() {
		const networkName = "net1"
		var (
			cudn          *udnv1.ClusterUserDefinedNetwork
			duplicateIPv4 = "10.128.0.200" // Static IP that will be used by both VMs
			duplicateIPv6 = "2010:100:200::200"
			cidrIPv4      = "10.128.0.0/24"
			cidrIPv6      = "2010:100:200::0/60"
		)

		BeforeEach(func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ = kubevirt.GenerateCUDN(namespace, networkName, udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(cudn)
		})

		waitForVMReadinessAndVerifyIPs := func(vmName string, expectedIPs []string) {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			expectedNumberOfAddresses := len(filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)}))
			actualAddresses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)
			Expect(actualAddresses).To(ConsistOf(expectedIPs), fmt.Sprintf("VM %s should get the requested static IPs", vmName))
		}

		It("should fail when creating second VM with duplicate static IP", func() {
			staticIPs := filterIPs(fr.ClientSet, duplicateIPv4, duplicateIPv6)

			By("Creating first VM with static IP")
			vm1 := createVMWithStaticIP("test-vm-1", staticIPs)
			createVirtualMachine(vm1)
			waitForVMReadinessAndVerifyIPs(vm1.Name, staticIPs)

			By("Verifying first VM IPAMClaim has successful status")
			verifyIPAMClaimStatusSuccess(getIPAMClaimName(vm1.Name, networkName))

			By("Creating second VM with duplicate static IP - should fail")
			vm2 := createVMWithStaticIP("test-vm-2", staticIPs)
			createVirtualMachine(vm2)

			By("Verifying pod fails with duplicate IP allocation error")
			waitForVMPodErrorEvent(vm2.Name, "provided IP is already allocated")

			By("Verifying second VM IPAMClaim has failure status with IPAddressConflict")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(vm2.Name, networkName), "IPAddressConflict")

			By("Verifying first VM is still running normally")
			waitForVMReadinessAndVerifyIPs(vm1.Name, staticIPs)
		})

		newVMIWithPrimaryIfaceMAC := func(mac string) *kubevirtv1.VirtualMachineInstance {
			vm := fedoraWithTestToolingVMI(nil, nil, nil, kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}, "#", "")
			vm.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			vm.Spec.Domain.Devices.Interfaces[0].MacAddress = mac
			return vm
		}

		It("should fail when creating second VM with duplicate user requested MAC", func() {
			const testMAC = "02:a1:b2:c3:d4:e5"
			vmi1 := newVMIWithPrimaryIfaceMAC(testMAC)
			vm1 := generateVM(vmi1)
			createVirtualMachine(vm1)

			By("Asserting VM with static MAC is running as expected")
			Eventually(func(g Gomega) []kubevirtv1.VirtualMachineInstanceCondition {
				g.Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm1), vmi1)).To(Succeed())
				return vmi1.Status.Conditions
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(ContainElement(SatisfyAll(
				HaveField("Type", kubevirtv1.VirtualMachineInstanceAgentConnected),
				HaveField("Status", corev1.ConditionTrue),
			)))
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm1), vmi1)).To(Succeed())
			Expect(vmi1.Status.Interfaces[0].MAC).To(Equal(testMAC), "vmi status should report the requested mac")

			By("Verifying first VM IPAMClaim has successful status")
			verifyIPAMClaimStatusSuccess(getIPAMClaimName(vm1.Name, networkName))

			By("Create second VM requesting the same MAC address")
			vmi2 := newVMIWithPrimaryIfaceMAC(testMAC)
			vm2 := generateVM(vmi2)
			createVirtualMachine(vm2)

			By("Asserting second VM pod has attached event reflecting MAC conflict error")
			vm2Selector := fmt.Sprintf("%s=%s", kubevirtv1.VirtualMachineNameLabel, vm2.Name)
			Eventually(func(g Gomega) []corev1.Event {
				podList, err := fr.ClientSet.CoreV1().Pods(vm2.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: vm2Selector})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(podList.Items).ToNot(BeEmpty())
				events, err := fr.ClientSet.CoreV1().Events(vm2.Namespace).SearchWithContext(context.Background(), scheme.Scheme, &podList.Items[0])
				g.Expect(err).ToNot(HaveOccurred())
				return events.Items
			}).WithTimeout(time.Minute * 1).WithPolling(time.Second * 3).Should(ContainElement(SatisfyAll(
				HaveField("Type", "Warning"),
				HaveField("Reason", "ErrorAllocatingPod"),
				HaveField("Message", ContainSubstring("MAC address already in use")),
			)))

			By("Verifying second VM IPAMClaim has failure status with MACAddressConflict")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(vm2.Name, networkName), "MACAddressConflict")

			By("Assert second VM not running")
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm2), vmi2)).To(Succeed())
			Expect(vmi2.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", kubevirtv1.VirtualMachineInstanceReady),
				HaveField("Status", corev1.ConditionFalse),
			)), "second VM should not be ready due to MAC conflict")
		})
	})

	Context("IP family validation for layer2 primary networks", func() {
		BeforeEach(func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
		})

		It("should fail when dual-stack network requests only IPv4", func() {
			cidrIPv4 := "10.130.0.0/24"
			cidrIPv6 := "2010:100:201::0/60"
			staticIPv4 := "10.130.0.101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(cudn)

			vm := createVMWithStaticIP("test-vm-dualstack-ipv4-only", []string{staticIPv4})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when dual-stack network requests only IPv6", func() {
			cidrIPv4 := "10.131.0.0/24"
			cidrIPv6 := "2010:100:202::0/60"
			staticIPv6 := "2010:100:202::101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(cudn)

			vm := createVMWithStaticIP("test-vm-dualstack-ipv6-only", []string{staticIPv6})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when single-stack IPv4 network requests multiple IPv4 IPs", func() {
			cidrIPv4 := "10.132.0.0/24"
			staticIPv4_1 := "10.132.0.101"
			staticIPv4_2 := "10.132.0.102"

			singleStackIPv4CIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4)})

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, singleStackIPv4CIDRs)
			createCUDN(cudn)

			vm := createVMWithStaticIP("test-vm-ipv4-network-two-ipv4", []string{staticIPv4_1, staticIPv4_2})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when single-stack IPv6 network requests multiple IPv6 IPs", func() {
			cidrIPv6 := "2010:100:204::0/60"
			staticIPv6_1 := "2010:100:204::101"
			staticIPv6_2 := "2010:100:204::102"

			singleStackIPv6CIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv6)})
			if len(singleStackIPv6CIDRs) == 0 {
				Skip("Cluster does not support IPv6")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, singleStackIPv6CIDRs)
			createCUDN(cudn)

			vm := createVMWithStaticIP("test-vm-ipv6-network-two-ipv6", []string{staticIPv6_1, staticIPv6_2})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should succeed when dual-stack network requests correct IPs (1 IPv4 + 1 IPv6)", func() {
			cidrIPv4 := "10.134.0.0/24"
			cidrIPv6 := "2010:100:205::0/60"
			staticIPv4 := "10.134.0.101"
			staticIPv6 := "2010:100:205::101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(cudn)

			staticIPs := filterIPs(fr.ClientSet, staticIPv4, staticIPv6)
			vm := createVMWithStaticIP("test-vm-dualstack-correct", staticIPs)
			createVirtualMachine(vm)

			// VM should start successfully
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			// Verify it got the requested IPs
			actualAddresses := virtualMachineAddressesFromStatus(vmi, len(staticIPs))
			Expect(actualAddresses).To(ConsistOf(staticIPs), "VM should get the requested static IPs")
		})
	})

	Context("ipv4 subnet exhaustion", func() {
		const networkName = "net1"
		var (
			cudn     *udnv1.ClusterUserDefinedNetwork
			cidrIPv4 = "10.130.0.0/30" // subnet with no usable IPs
			cidrIPv6 = "2011:100:200::0/120"
		)

		BeforeEach(func() {
			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.Background(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ = kubevirt.GenerateCUDN(namespace, networkName, udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(cudn)
		})

		It("should fail when subnet is exhausted", func() {
			By("Creating VM that should fail due to subnet exhaustion")
			exhaustedVMName := "exhausted-vm"
			vm := fedoraWithTestToolingVM(
				nil, // labels
				nil, // no static IP annotations
				nil, // nodeSelector
				kubevirtv1.NetworkSource{
					Pod: &kubevirtv1.PodNetwork{},
				},
				`#cloud-config
password: fedora
chpasswd: { expire: False }
`,
				`version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`,
			)
			vm.Name = exhaustedVMName
			vm.Namespace = namespace
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			createVirtualMachine(vm)

			By("Verifying pod fails with subnet exhaustion error")
			waitForVMPodErrorEvent(exhaustedVMName, "subnet address pool exhausted")

			By("Verifying VM IPAMClaim has failure status with SubnetExhausted")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(exhaustedVMName, networkName), "SubnetExhausted")
		})
	})
})
