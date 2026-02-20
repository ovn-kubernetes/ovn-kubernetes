package networkconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NOTE: This file tests the full controller in an integrated fashion.
// Of course the applyReactor is used to mock the k8s api server.

const ovnNetworkConnectSubnetAnnotation = "k8s.ovn.org/network-connect-subnet"

// =============================================================================
// Shared test helpers for creating test objects
// =============================================================================

// newTestCNC creates a test CNC object with the given name, selectors, and connect subnets.
// If connectSubnets is nil, it defaults to 192.168.0.0/16 with /24 prefix.
func newTestCNC(name string, selectors []apitypes.NetworkSelector, connectSubnets []networkconnectv1.ConnectSubnet) *networkconnectv1.ClusterNetworkConnect {
	if connectSubnets == nil {
		connectSubnets = []networkconnectv1.ConnectSubnet{
			{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
		}
	}
	return &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			NetworkSelectors: selectors,
			ConnectSubnets:   connectSubnets,
			Connectivity: []networkconnectv1.ConnectivityType{
				networkconnectv1.PodNetwork,
			},
		},
	}
}

// newTestUDNNAD creates a test NAD owned by a UserDefinedNetwork.
func newTestUDNNAD(name, namespace, network string, networkID string) *nadv1.NetworkAttachmentDefinition {
	return &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				types.OvnNetworkNameAnnotation: network,
				types.OvnNetworkIDAnnotation:   networkID,
			},
			OwnerReferences: []metav1.OwnerReference{makeUDNOwnerRef(name)},
		},
		Spec: nadv1.NetworkAttachmentDefinitionSpec{
			Config: fmt.Sprintf(
				`{"cniVersion": "0.4.0", "name": "%s", "type": "%s", "topology": "layer3", "netAttachDefName": "%s/%s", "role": "primary", "subnets": "10.0.0.0/16/24"}`,
				network,
				config.CNI.Plugin,
				namespace,
				name,
			),
		},
	}
}

// newTestCUDNNAD creates a test NAD owned by a ClusterUserDefinedNetwork.
func newTestCUDNNAD(name, namespace, network string, labels map[string]string, networkID string) *nadv1.NetworkAttachmentDefinition {
	nad := newTestUDNNAD(name, namespace, network, networkID)
	nad.Labels = labels
	nad.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(network)}
	return nad
}

// newTestNamespace creates a test namespace with the given name and labels.
func newTestNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

var _ = ginkgo.Describe("NetworkConnect ClusterManager Controller Integration Tests", func() {
	var (
		app           *cli.App
		controller    *Controller
		fakeClientset *util.OVNClusterManagerClientset
		fakeNM        *networkmanager.FakeNetworkManager
		wf            *factory.WatchFactory
	)

	// start initializes the controller with pre-populated objects (standard k8s objects only)
	start := func(objects ...runtime.Object) {
		fakeClientset = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClientset)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fakeNM = &networkmanager.FakeNetworkManager{
			PrimaryNetworks: make(map[string]util.NetInfo),
			NADNetworks:     make(map[string]util.NetInfo),
		}

		tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
		controller = NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = controller.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// Local aliases for shared helpers (for cleaner test code)
	testCNC := func(name string, selectors []apitypes.NetworkSelector) *networkconnectv1.ClusterNetworkConnect {
		return newTestCNC(name, selectors, nil)
	}
	testCUDNNAD := newTestCUDNNAD
	testUDNNAD := newTestUDNNAD

	// Helper to get CNC annotations
	getCNCAnnotations := func(cncName string) (map[string]string, error) {
		cnc, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
			context.Background(), cncName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return cnc.Annotations, nil
	}

	// Helper to verify CNC has tunnel ID annotation
	hasTunnelIDAnnotation := func(cncName string) bool {
		annotations, err := getCNCAnnotations(cncName)
		if err != nil {
			return false
		}
		_, exists := annotations[util.OvnConnectRouterTunnelKeyAnnotation]
		return exists
	}

	// Helper to verify CNC has non-empty subnet annotation
	hasNonEmptySubnetAnnotation := func(cncName string) bool {
		annotations, err := getCNCAnnotations(cncName)
		if err != nil {
			return false
		}
		subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]
		if !exists {
			return false
		}
		return subnetAnnotation != "{}"
	}

	// Helper to get subnet annotation network count
	getSubnetAnnotationNetworkCount := func(cncName string) int {
		annotations, err := getCNCAnnotations(cncName)
		if err != nil {
			return -1
		}
		subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]
		if !exists {
			return 0
		}
		if subnetAnnotation == "{}" {
			return 0
		}
		var subnets map[string]util.NetworkConnectSubnetAnnotation
		if err := json.Unmarshal([]byte(subnetAnnotation), &subnets); err != nil {
			return -1
		}
		return len(subnets)
	}

	ginkgo.BeforeEach(func() {
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkConnect = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	ginkgo.AfterEach(func() {
		if controller != nil {
			controller.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
	})

	ginkgo.Context("ClusterNetworkConnect ClusterManager Annotation Tests", func() {

		ginkgo.It("1. CNC created with 0 matching networks only has tunnel ID annotation", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-no-networks"

				start() // No pre-populated objects

				// Create CNC via client
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"nonexistent": "label"},
							},
						},
					},
				})
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have tunnel ID annotation
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Verify no subnet annotation
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("2. CNC created with matching P-CUDNs has both subnet and tunnel ID annotations", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-cudn"
				testLabel := map[string]string{"test-cudn": "true"}
				cudnNetwork := util.GenerateCUDNNetworkName("test-cudn")

				start() // Start with no pre-populated objects

				// Create NAD first
				nad := testCUDNNAD("cudn-red", "red", cudnNetwork, testLabel, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("red").Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have both annotations
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName) && hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Verify subnet annotation has 1 network
				gomega.Expect(getSubnetAnnotationNetworkCount(cncName)).To(gomega.Equal(1))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("3. CNC selects multiple CUDNs matching label selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-multi"
				testLabel := map[string]string{"env": "test"}
				network1 := util.GenerateCUDNNetworkName("blue")
				network2 := util.GenerateCUDNNetworkName("green")

				start()

				// Create NADs
				nad1 := testCUDNNAD("cudn-blue", "blue", network1, testLabel, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("blue").Create(
					context.Background(), nad1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nad2 := testCUDNNAD("cudn-green", "green", network2, testLabel, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("green").Create(
					context.Background(), nad2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// This one should NOT be selected (different label)
				nad3 := testCUDNNAD("cudn-yellow", "yellow", util.GenerateCUDNNetworkName("yellow"), map[string]string{"env": "prod"}, "3")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("yellow").Create(
					context.Background(), nad3, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have both annotations with 2 networks
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(2))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("4. CNC annotations updated when new NAD is created that matches selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-nad-create"
				testLabel := map[string]string{"test-create": "true"}
				network1 := util.GenerateCUDNNetworkName("create1")

				start()

				// Create first NAD
				nad1 := testCUDNNAD("cudn-create1", "create1-ns", network1, testLabel, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("create1-ns").Create(
					context.Background(), nad1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for initial state with 1 network
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(1))

				// Create a second NAD that matches the selector
				network2 := util.GenerateCUDNNetworkName("create2")
				nad2 := testCUDNNAD("cudn-create2", "create2-ns", network2, testLabel, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("create2-ns").Create(
					context.Background(), nad2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to be updated with 2 networks
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(2))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("5. CNC annotations updated when matching NAD is deleted", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-nad-delete"
				testLabel := map[string]string{"test-delete": "true"}
				network1 := util.GenerateCUDNNetworkName("del1")
				network2 := util.GenerateCUDNNetworkName("del2")

				start()

				// Create NADs
				nad1 := testCUDNNAD("cudn-del1", "del1-ns", network1, testLabel, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("del1-ns").Create(
					context.Background(), nad1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nad2 := testCUDNNAD("cudn-del2", "del2-ns", network2, testLabel, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("del2-ns").Create(
					context.Background(), nad2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for initial state with 2 networks
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(2))

				// Delete one NAD
				err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("del2-ns").Delete(
					context.Background(), "cudn-del2", metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to be updated with 1 network
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(1))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("6. CNC annotations updated when NAD label changes to match selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-label-match"
				matchingLabel := map[string]string{"test-label": "true"}
				nonMatchingLabel := map[string]string{"test-label": "false"}
				network := util.GenerateCUDNNetworkName("label-match")

				start()

				// Create CNC first
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: matchingLabel,
							},
						},
					},
				})
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have tunnel ID (no matching NADs yet)
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				// Create NAD with non-matching label
				nad := testCUDNNAD("cudn-label", "label-ns", network, nonMatchingLabel, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("label-ns").Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Still no subnet annotation
				gomega.Consistently(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(1 * time.Second).Should(gomega.BeFalse())

				// Update NAD label to match
				updatedNAD, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("label-ns").Get(
					context.Background(), "cudn-label", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedNAD.Labels = matchingLabel
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("label-ns").Update(
					context.Background(), updatedNAD, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("7. CNC annotations updated when NAD label changes to stop matching selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-label-unmatch"
				matchingLabel := map[string]string{"test-label": "true"}
				network := util.GenerateCUDNNetworkName("label-unmatch")

				start()

				// Create NAD with matching label
				nad := testCUDNNAD("cudn-unlabel", "unlabel-ns", network, matchingLabel, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("unlabel-ns").Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: matchingLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Update NAD label to no longer match
				updatedNAD, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("unlabel-ns").Get(
					context.Background(), "cudn-unlabel", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedNAD.Labels = map[string]string{"test-label": "false"}
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("unlabel-ns").Update(
					context.Background(), updatedNAD, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC subnet annotation to become empty
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))

				// Should still have tunnel ID
				gomega.Expect(hasTunnelIDAnnotation(cncName)).To(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("8. CNC ignores secondary network NADs", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-secondary"
				testLabel := map[string]string{"test-secondary": "true"}
				network := util.GenerateCUDNNetworkName("secondary")

				start()

				// Create a secondary NAD (no "role": "primary" in config)
				secondaryNAD := &nadv1.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cudn-secondary",
						Namespace: "secondary-ns",
						Labels:    testLabel,
						Annotations: map[string]string{
							types.OvnNetworkNameAnnotation: network,
							types.OvnNetworkIDAnnotation:   "1",
						},
						OwnerReferences: []metav1.OwnerReference{
							*metav1.NewControllerRef(
								&metav1.ObjectMeta{Name: network},
								userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"),
							),
						},
					},
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: fmt.Sprintf(
							`{"cniVersion": "0.4.0", "name": "%s", "type": "%s", "topology": "layer3", "netAttachDefName": "secondary-ns/cudn-secondary", "subnets": "10.0.0.0/16/24"}`,
							network,
							config.CNI.Plugin,
						),
					},
				}
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("secondary-ns").Create(
					context.Background(), secondaryNAD, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have tunnel ID
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Should NOT have subnet annotation (secondary NAD should be ignored)
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		// Primary UDN selector tests
		ginkgo.It("9. CNC with Primary UDN selector matches namespace with primary UDN", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-udn"
				nsName := "udn-ns"
				nadName := "primary-udn"
				network := util.GenerateUDNNetworkName(nsName, nadName)

				start()

				// Create namespace with matching label and RequiredUDNNamespaceLabel
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: nsName,
						Labels: map[string]string{
							"udn-enabled":                   "true",
							types.RequiredUDNNamespaceLabel: "",
						},
					},
				}
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create UDN NAD
				nad := testUDNNAD(nadName, nsName, network, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Configure FakeNetworkManager to return this NAD as primary for the namespace
				netInfo, err := util.ParseNADInfo(nad)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(nsName + "/" + nadName)
				fakeNM.PrimaryNetworks[nsName] = mutableNetInfo
				fakeNM.NADNetworks[nsName+"/"+nadName] = netInfo

				// Ensure namespace and NAD are visible in informer caches before CNC creation.
				gomega.Eventually(func() bool {
					_, err := controller.namespaceLister.Get(nsName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())
				gomega.Eventually(func() bool {
					_, err := controller.nadLister.NetworkAttachmentDefinitions(nsName).Get(nadName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())
				fakeNM.NADNetworks[nsName+"/"+nadName] = netInfo
				fakeNM.NADNetworks[nsName+"/"+nadName] = netInfo

				// Create CNC with Primary UDN selector
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"udn-enabled": "true"},
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have both annotations
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName) && hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Verify subnet annotation has 1 network
				gomega.Expect(getSubnetAnnotationNetworkCount(cncName)).To(gomega.Equal(1))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("10. CNC selects multiple Primary UDNs from multiple namespaces", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-multi-udn"

				start()

				// Create first namespace and UDN
				ns1 := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "frontend-a",
						Labels: map[string]string{
							"tier":                          "frontend",
							types.RequiredUDNNamespaceLabel: "",
						},
					},
				}
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				network1 := util.GenerateUDNNetworkName("frontend-a", "primary-udn")
				nad1 := testUDNNAD("primary-udn", "frontend-a", network1, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("frontend-a").Create(
					context.Background(), nad1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo1, err := util.ParseNADInfo(nad1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo1 := util.NewMutableNetInfo(netInfo1)
				mutableNetInfo1.AddNADs("frontend-a/primary-udn")
				fakeNM.PrimaryNetworks["frontend-a"] = mutableNetInfo1
				fakeNM.NADNetworks["frontend-a/primary-udn"] = netInfo1

				// Create second namespace and UDN
				ns2 := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "frontend-b",
						Labels: map[string]string{
							"tier":                          "frontend",
							types.RequiredUDNNamespaceLabel: "",
						},
					},
				}
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				network2 := util.GenerateUDNNetworkName("frontend-b", "primary-udn")
				nad2 := testUDNNAD("primary-udn", "frontend-b", network2, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("frontend-b").Create(
					context.Background(), nad2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo2, err := util.ParseNADInfo(nad2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo2 := util.NewMutableNetInfo(netInfo2)
				mutableNetInfo2.AddNADs("frontend-b/primary-udn")
				fakeNM.PrimaryNetworks["frontend-b"] = mutableNetInfo2
				fakeNM.NADNetworks["frontend-b/primary-udn"] = netInfo2

				// Create a non-matching namespace
				ns3 := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "backend",
						Labels: map[string]string{"tier": "backend"},
					},
				}
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns3, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"},
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have 2 networks in subnet annotation
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(2))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("11. CNC updated when namespace label changes to match Primary UDN selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-ns-label-match"
				nsName := "label-change-ns"
				nadName := "primary-udn"
				network := util.GenerateUDNNetworkName(nsName, nadName)

				start()

				// Create CNC first
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				})
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for tunnel ID (no matching namespaces yet)
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				// Create namespace with non-matching selector label but with RequiredUDNNamespaceLabel
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: nsName,
						Labels: map[string]string{
							"selected":                      "false",
							types.RequiredUDNNamespaceLabel: "",
						},
					},
				}
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create UDN NAD
				nad := testUDNNAD(nadName, nsName, network, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Configure FakeNetworkManager
				netInfo, err := util.ParseNADInfo(nad)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(nsName + "/" + nadName)
				fakeNM.PrimaryNetworks[nsName] = mutableNetInfo
				fakeNM.NADNetworks[nsName+"/"+nadName] = netInfo

				// Ensure namespace and NAD are visible in informer caches before CNC creation.
				gomega.Eventually(func() bool {
					_, err := controller.namespaceLister.Get(nsName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())
				gomega.Eventually(func() bool {
					_, err := controller.nadLister.NetworkAttachmentDefinitions(nsName).Get(nadName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())

				// Still no subnet annotation
				gomega.Consistently(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(1 * time.Second).Should(gomega.BeFalse())

				// Update namespace label to match (keep RequiredUDNNamespaceLabel)
				updatedNS, err := fakeClientset.KubeClient.CoreV1().Namespaces().Get(
					context.Background(), nsName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedNS.Labels = map[string]string{
					"selected":                      "true",
					types.RequiredUDNNamespaceLabel: "",
				}
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Update(
					context.Background(), updatedNS, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("12. CNC updated when namespace label changes to stop matching Primary UDN selector", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-ns-unmatch"
				nsName := "unmatch-ns"
				nadName := "primary-udn"
				network := util.GenerateUDNNetworkName(nsName, nadName)

				start()

				// Create namespace with matching label
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: nsName,
						Labels: map[string]string{
							"selected":                      "true",
							types.RequiredUDNNamespaceLabel: "",
						},
					},
				}
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create UDN NAD
				nad := testUDNNAD(nadName, nsName, network, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Configure FakeNetworkManager
				netInfo, err := util.ParseNADInfo(nad)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(nsName + "/" + nadName)
				fakeNM.PrimaryNetworks[nsName] = mutableNetInfo
				fakeNM.NADNetworks[nsName+"/"+nadName] = netInfo

				// Ensure namespace and NAD are visible in informer caches before CNC creation.
				gomega.Eventually(func() bool {
					_, err := controller.namespaceLister.Get(nsName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())
				gomega.Eventually(func() bool {
					_, err := controller.nadLister.NetworkAttachmentDefinitions(nsName).Get(nadName)
					return err == nil
				}).WithTimeout(2 * time.Second).Should(gomega.BeTrue())

				// Create CNC with Primary UDN selector
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation (namespace matches)
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Update namespace label to stop matching (keep RequiredUDNNamespaceLabel)
				updatedNS, err := fakeClientset.KubeClient.CoreV1().Namespaces().Get(
					context.Background(), nsName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedNS.Labels = map[string]string{
					"selected":                      "false", // no longer matches
					types.RequiredUDNNamespaceLabel: "",
				}
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Update(
					context.Background(), updatedNS, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC subnet annotation to become empty
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))

				// Should still have tunnel ID
				gomega.Expect(hasTunnelIDAnnotation(cncName)).To(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("13. CNC ignores namespace without primary UDN even if label matches", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-no-udn"

				start()

				// Create namespace with matching label but no UDN
				// Note: we don't add RequiredUDNNamespaceLabel since this namespace has no UDN
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "no-udn-ns",
						Labels: map[string]string{"selected": "true"},
					},
				}
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Don't configure FakeNetworkManager - no primary network for this namespace

				// Create CNC
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for tunnel ID
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Should NOT have subnet annotation (namespace has no primary UDN)
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("14. CNC selector update causes networks to start matching", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-selector-match"
				cudnNetwork := util.GenerateCUDNNetworkName("selector-test")

				start()

				// Create NAD with specific label
				nad := testCUDNNAD("cudn-selector", "selector-ns", cudnNetwork, map[string]string{"env": "prod"}, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("selector-ns").Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC with non-matching selector
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "dev"}, // doesn't match "prod"
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for tunnel ID (no matching networks yet)
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
				gomega.Expect(hasNonEmptySubnetAnnotation(cncName)).To(gomega.BeFalse())

				// Update CNC selector to match the NAD
				updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cncName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedCNC.Spec.NetworkSelectors = []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "prod"}, // now matches
							},
						},
					},
				}
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
					context.Background(), updatedCNC, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("15. CNC selector update causes networks to stop matching", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-selector-unmatch"
				cudnNetwork := util.GenerateCUDNNetworkName("selector-unmatch")

				start()

				// Create NAD with specific label
				nad := testCUDNNAD("cudn-unmatch", "unmatch-ns", cudnNetwork, map[string]string{"env": "prod"}, "1")
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("unmatch-ns").Create(
					context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC with matching selector
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "prod"}, // matches
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have subnet annotation
				gomega.Eventually(func() bool {
					return hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Update CNC selector to no longer match
				updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cncName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				updatedCNC.Spec.NetworkSelectors = []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "dev"}, // no longer matches "prod"
							},
						},
					},
				}
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
					context.Background(), updatedCNC, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC subnet annotation to become empty
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))

				// Should still have tunnel ID
				gomega.Expect(hasTunnelIDAnnotation(cncName)).To(gomega.BeTrue())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("16. CNC continues processing healthy networks even when one NAD has parse error", func() {
			app.Action = func(*cli.Context) error {
				cncName := "test-cnc-error-aggregation"
				testLabel := map[string]string{"error-test": "true"}
				healthyNetwork := util.GenerateCUDNNetworkName("healthy")

				start()

				// Create a NAD with malformed config (will cause ParseNADInfo to fail)
				malformedNAD := &nadv1.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "malformed-nad",
						Namespace: "malformed-ns",
						Labels:    testLabel,
						Annotations: map[string]string{
							types.OvnNetworkNameAnnotation: "malformed-network",
							types.OvnNetworkIDAnnotation:   "1",
						},
						OwnerReferences: []metav1.OwnerReference{makeCUDNOwnerRef("malformed-cudn")},
					},
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						// Invalid JSON config - missing required fields, will fail ParseNADInfo
						Config: `{"cniVersion": "0.4.0", "name": "malformed", "type": "invalid-type"}`,
					},
				}
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("malformed-ns").Create(
					context.Background(), malformedNAD, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create a healthy NAD
				healthyNAD := testCUDNNAD("healthy-nad", "healthy-ns", healthyNetwork, testLabel, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("healthy-ns").Create(
					context.Background(), healthyNAD, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create CNC that matches both NADs
				cnc := testCNC(cncName, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: testLabel,
							},
						},
					},
				})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC to have tunnel ID and subnet annotation
				// The healthy NAD should be processed even though the malformed one fails
				gomega.Eventually(func() bool {
					return hasTunnelIDAnnotation(cncName) && hasNonEmptySubnetAnnotation(cncName)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

				// Verify subnet annotation has 1 network (the healthy one)
				// The malformed NAD should have been skipped due to parse error
				gomega.Expect(getSubnetAnnotationNetworkCount(cncName)).To(gomega.Equal(1))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})

var _ = ginkgo.Describe("NetworkConnect ClusterManager Controller InitialSync Tests", func() {
	var (
		app *cli.App
	)

	ginkgo.BeforeEach(func() {
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkConnect = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	ginkgo.Context("Controller restart preserves allocator state", func() {
		ginkgo.It("initialSync correctly restores tunnel IDs and subnet allocations after restart", func() {
			app.Action = func(*cli.Context) error {
				// ============================================================
				// PHASE 1: Set up controller with 2 CNCs selecting multiple networks
				// ============================================================

				// CNC1: 2 CUDNs + 2 PUDNs
				cnc1Name := "test-cnc1"
				cnc1CUDNLabel := map[string]string{"cnc1-cudn": "true"}
				cudn1Network := util.GenerateCUDNNetworkName("cudn1")
				cudn2Network := util.GenerateCUDNNetworkName("cudn2")

				// CNC2: 1 CUDN + 1 PUDN
				cnc2Name := "test-cnc2"
				cnc2CUDNLabel := map[string]string{"cnc2-cudn": "true"}
				cudn3Network := util.GenerateCUDNNetworkName("cudn3")

				// Create clientset and watch factory
				fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
				ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

				wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeNM := &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
					NADNetworks:     make(map[string]util.NetInfo),
				}

				tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
				controller := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

				err = wf.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = controller.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ============================================================
				// Create NADs for CNC1 (2 CUDNs)
				// ============================================================
				nad1 := newTestCUDNNAD("cudn1-nad", "ns-cudn1", cudn1Network, cnc1CUDNLabel, "1")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns-cudn1").Create(
					context.Background(), nad1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nad2 := newTestCUDNNAD("cudn2-nad", "ns-cudn2", cudn2Network, cnc1CUDNLabel, "2")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns-cudn2").Create(
					context.Background(), nad2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ============================================================
				// Create P-UDN namespaces and NADs for CNC1 (2 P-UDNs)
				// ============================================================
				ns1 := newTestNamespace("pudn1-ns", map[string]string{
					"cnc1-pudn":                     "true",
					types.RequiredUDNNamespaceLabel: "",
				})
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				pudn1Network := util.GenerateUDNNetworkName("pudn1-ns", "primary-udn")
				nad3 := newTestUDNNAD("primary-udn", "pudn1-ns", pudn1Network, "3")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("pudn1-ns").Create(
					context.Background(), nad3, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo1, err := util.ParseNADInfo(nad3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo1 := util.NewMutableNetInfo(netInfo1)
				mutableNetInfo1.AddNADs("pudn1-ns/primary-udn")
				fakeNM.PrimaryNetworks["pudn1-ns"] = mutableNetInfo1
				fakeNM.NADNetworks["pudn1-ns/primary-udn"] = netInfo1

				ns2 := newTestNamespace("pudn2-ns", map[string]string{
					"cnc1-pudn":                     "true",
					types.RequiredUDNNamespaceLabel: "",
				})
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				pudn2Network := util.GenerateUDNNetworkName("pudn2-ns", "primary-udn")
				nad4 := newTestUDNNAD("primary-udn", "pudn2-ns", pudn2Network, "4")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("pudn2-ns").Create(
					context.Background(), nad4, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo2, err := util.ParseNADInfo(nad4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo2 := util.NewMutableNetInfo(netInfo2)
				mutableNetInfo2.AddNADs("pudn2-ns/primary-udn")
				fakeNM.PrimaryNetworks["pudn2-ns"] = mutableNetInfo2
				fakeNM.NADNetworks["pudn2-ns/primary-udn"] = netInfo2

				// ============================================================
				// Create NADs for CNC2 (1 CUDN + 1 P-UDN)
				// ============================================================
				nad5 := newTestCUDNNAD("cudn3-nad", "ns-cudn3", cudn3Network, cnc2CUDNLabel, "5")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns-cudn3").Create(
					context.Background(), nad5, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ns3 := newTestNamespace("pudn3-ns", map[string]string{
					"cnc2-pudn":                     "true",
					types.RequiredUDNNamespaceLabel: "",
				})
				_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns3, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				pudn3Network := util.GenerateUDNNetworkName("pudn3-ns", "primary-udn")
				nad6 := newTestUDNNAD("primary-udn", "pudn3-ns", pudn3Network, "6")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("pudn3-ns").Create(
					context.Background(), nad6, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo3, err := util.ParseNADInfo(nad6)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mutableNetInfo3 := util.NewMutableNetInfo(netInfo3)
				mutableNetInfo3.AddNADs("pudn3-ns/primary-udn")
				fakeNM.PrimaryNetworks["pudn3-ns"] = mutableNetInfo3
				fakeNM.NADNetworks["pudn3-ns/primary-udn"] = netInfo3

				// ============================================================
				// Create CNCs
				// ============================================================
				cnc1 := newTestCNC(cnc1Name, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: cnc1CUDNLabel,
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"cnc1-pudn": "true"},
							},
						},
					},
				}, nil) // uses default 192.168.0.0/16 /24
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cnc2 := newTestCNC(cnc2Name, []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: cnc2CUDNLabel,
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"cnc2-pudn": "true"},
							},
						},
					},
				}, []networkconnectv1.ConnectSubnet{{CIDR: "10.100.0.0/16", NetworkPrefix: 24}})
				_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ============================================================
				// Wait for annotations to be set
				// ============================================================
				getCNCAnnotations := func(cncName string) (map[string]string, error) {
					cnc, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), cncName, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					return cnc.Annotations, nil
				}

				getSubnetAnnotationNetworkCount := func(cncName string) int {
					annotations, err := getCNCAnnotations(cncName)
					if err != nil {
						return -1
					}
					subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]
					if !exists {
						return 0
					}
					if subnetAnnotation == "{}" {
						return 0
					}
					var subnets map[string]util.NetworkConnectSubnetAnnotation
					if err := json.Unmarshal([]byte(subnetAnnotation), &subnets); err != nil {
						return -1
					}
					return len(subnets)
				}

				// Wait for CNC1 to have 4 networks (2 CUDNs + 2 PUDNs)
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cnc1Name)
				}).WithTimeout(10 * time.Second).Should(gomega.Equal(4))

				// Wait for CNC2 to have 2 networks (1 CUDN + 1 PUDN)
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cnc2Name)
				}).WithTimeout(10 * time.Second).Should(gomega.Equal(2))

				// ============================================================
				// PHASE 2: Capture state before restart
				// ============================================================
				cnc1BeforeRestart, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cnc1Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cnc2BeforeRestart, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cnc2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Store original annotations
				cnc1TunnelIDBefore := cnc1BeforeRestart.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]
				cnc1SubnetsBefore := cnc1BeforeRestart.Annotations[ovnNetworkConnectSubnetAnnotation]
				cnc2TunnelIDBefore := cnc2BeforeRestart.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]
				cnc2SubnetsBefore := cnc2BeforeRestart.Annotations[ovnNetworkConnectSubnetAnnotation]

				gomega.Expect(cnc1TunnelIDBefore).NotTo(gomega.BeEmpty())
				gomega.Expect(cnc1SubnetsBefore).NotTo(gomega.BeEmpty())
				gomega.Expect(cnc2TunnelIDBefore).NotTo(gomega.BeEmpty())
				gomega.Expect(cnc2SubnetsBefore).NotTo(gomega.BeEmpty())

				// ============================================================
				// PHASE 3: Stop the controller
				// ============================================================
				controller.Stop()
				wf.Shutdown()

				// ============================================================
				// PHASE 4: Restart with same objects (simulating restart)
				// ============================================================
				// Create new watch factory and controller with same clientset (keeps objects)
				wf2, err := factory.NewClusterManagerWatchFactory(fakeClientset)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create new FakeNetworkManager with same primary networks config
				fakeNM2 := &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
					NADNetworks:     make(map[string]util.NetInfo),
				}
				// Re-setup primary networks (in real deployment this comes from network manager cache)
				fakeNM2.PrimaryNetworks["pudn1-ns"] = mutableNetInfo1
				fakeNM2.NADNetworks["pudn1-ns/primary-udn"] = netInfo1
				fakeNM2.PrimaryNetworks["pudn2-ns"] = mutableNetInfo2
				fakeNM2.NADNetworks["pudn2-ns/primary-udn"] = netInfo2
				fakeNM2.PrimaryNetworks["pudn3-ns"] = mutableNetInfo3
				fakeNM2.NADNetworks["pudn3-ns/primary-udn"] = netInfo3

				tunnelKeysAllocator2 := id.NewTunnelKeyAllocator("TunnelKeys")
				controller2 := NewController(wf2, fakeClientset, fakeNM2.Interface(), tunnelKeysAllocator2)

				err = wf2.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = controller2.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ============================================================
				// PHASE 5: Verify state is correctly restored
				// ============================================================

				// Get CNCs after restart
				cnc1AfterRestart, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cnc1Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cnc2AfterRestart, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cnc2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify tunnel IDs are unchanged
				gomega.Expect(cnc1AfterRestart.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]).To(
					gomega.Equal(cnc1TunnelIDBefore), "CNC1 tunnel ID should be preserved after restart")
				gomega.Expect(cnc2AfterRestart.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]).To(
					gomega.Equal(cnc2TunnelIDBefore), "CNC2 tunnel ID should be preserved after restart")

				// Verify subnet annotations are unchanged
				gomega.Expect(cnc1AfterRestart.Annotations[ovnNetworkConnectSubnetAnnotation]).To(
					gomega.Equal(cnc1SubnetsBefore), "CNC1 subnet allocations should be preserved after restart")
				gomega.Expect(cnc2AfterRestart.Annotations[ovnNetworkConnectSubnetAnnotation]).To(
					gomega.Equal(cnc2SubnetsBefore), "CNC2 subnet allocations should be preserved after restart")

				// Verify network counts are unchanged
				gomega.Expect(getSubnetAnnotationNetworkCount(cnc1Name)).To(gomega.Equal(4))
				gomega.Expect(getSubnetAnnotationNetworkCount(cnc2Name)).To(gomega.Equal(2))

				// ============================================================
				// PHASE 6: Verify allocator state by adding new networks
				// ============================================================
				// Add a new CUDN to CNC1 and verify it gets a NEW subnet (not conflicting)
				newCUDNNetwork := util.GenerateCUDNNetworkName("new-cudn")
				newNAD := newTestCUDNNAD("new-cudn-nad", "ns-new-cudn", newCUDNNetwork, cnc1CUDNLabel, "7")
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns-new-cudn").Create(
					context.Background(), newNAD, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for CNC1 to have 5 networks now (4 original + 1 new)
				gomega.Eventually(func() int {
					return getSubnetAnnotationNetworkCount(cnc1Name)
				}).WithTimeout(10 * time.Second).Should(gomega.Equal(5))

				// Get the updated CNC1 and verify new subnet doesn't conflict with existing ones
				cnc1Final, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), cnc1Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Parse the subnet annotations
				var subnetsBefore map[string]util.NetworkConnectSubnetAnnotation
				err = json.Unmarshal([]byte(cnc1SubnetsBefore), &subnetsBefore)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				var subnetsAfter map[string]util.NetworkConnectSubnetAnnotation
				err = json.Unmarshal([]byte(cnc1Final.Annotations[ovnNetworkConnectSubnetAnnotation]), &subnetsAfter)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// All original subnets should still be present with same values
				for owner, subnet := range subnetsBefore {
					gomega.Expect(subnetsAfter).To(gomega.HaveKey(owner))
					gomega.Expect(subnetsAfter[owner]).To(gomega.Equal(subnet),
						"Original subnet for %s should be unchanged", owner)
				}

				// The new network should get the next sequential subnet after the existing ones
				// CNC1 uses default 192.168.0.0/16 with /24 prefix, subnets are allocated sequentially:
				// 192.168.0.0/24, 192.168.1.0/24, 192.168.2.0/24, 192.168.3.0/24, ...
				// CNC1 had 4 networks before (subnets 0-3), so the 5th should get 192.168.4.0/24
				expectedNextSubnet := "192.168.4.0/24"

				// Find the new network's subnet
				var newNetworkSubnet string
				for owner, subnet := range subnetsAfter {
					if _, existed := subnetsBefore[owner]; !existed {
						// This is the new network
						newNetworkSubnet = subnet.IPv4
						break
					}
				}

				gomega.Expect(newNetworkSubnet).To(gomega.Equal(expectedNextSubnet),
					"New network should get the next sequential subnet %s, but got %s",
					expectedNextSubnet, newNetworkSubnet)

				// Cleanup
				controller2.Stop()
				wf2.Shutdown()

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
