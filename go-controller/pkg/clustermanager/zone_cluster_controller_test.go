package clustermanager

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockNodeIDAllocator is a mock implementation of id.Allocator for testing
type mockNodeIDAllocator struct {
	mock.Mock
}

// AllocateID is not implemented - will panic if called
func (m *mockNodeIDAllocator) AllocateID(name string) (int, error) {
	args := m.Called(name)
	return args.Int(0), args.Error(1)
}

// ReserveID is not implemented - will panic if called
func (m *mockNodeIDAllocator) ReserveID(name string, idVal int) error {
	args := m.Called(name, idVal)
	return args.Error(0)
}

// ReleaseID mocks the ReleaseID method
func (m *mockNodeIDAllocator) ReleaseID(name string) {
	m.Called(name)
}

// ForName is not implemented - will panic if called
func (m *mockNodeIDAllocator) ForName(name string) id.NamedAllocator {
	args := m.Called(name)
	return args.Get(0).(id.NamedAllocator)
}

// GetID mocks the GetID method
func (m *mockNodeIDAllocator) GetID(name string) int {
	args := m.Called(name)
	return args.Int(0)
}

var _ = Describe("Test needsZoneAllocation", func() {

	BeforeEach(func() {
		// Reset to default values
		config.HybridOverlay.Enabled = false
		config.OVNKubernetesFeature.EnableInterconnect = false
		config.OVNKubernetesFeature.EnableMultiVTEP = false
		config.Kubernetes.NoHostSubnetNodes = nil
	})

	AfterEach(func() {

	})

	It("should return true for a node without OvnNodeID annotation", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: map[string]string{},
			},
		}
		Expect(needsZoneAllocation(node)).To(BeTrue())
	})

	It("should return false when OvnNodeID is present and interconnect is disabled", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
				Annotations: map[string]string{
					util.OvnNodeID: "1",
				},
			},
		}
		config.OVNKubernetesFeature.EnableInterconnect = false
		Expect(needsZoneAllocation(node)).To(BeFalse())
	})

	Context("when EnableInterconnect is true", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiVTEP = true
		})

		It("should return true when OvnNodeID is present but OvnTransitSwitchPortAddr is missing", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID: "1",
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeTrue())
		})

		It("should return false with multiple encap IPs and matching tunnel IDs", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID:                     "1",
						util.OvnTransitSwitchPortAddr:      `{"ipAddress":"100.89.0.2/16"}`,
						util.OVNNodeEncapIPs:               `["10.1.1.1", "10.1.1.2", "10.1.1.3"]`,
						util.OvnTransitSwitchPortTunnelIDs: `[1, 2, 3]`,
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeFalse())
		})

		It("should return true when encap IPs and tunnel IDs count mismatch", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID:                     "1",
						util.OvnTransitSwitchPortAddr:      `{"ipv4":"100.88.0.1/16"}`,
						util.OVNNodeEncapIPs:               `["10.1.1.1", "10.1.1.2"]`,
						util.OvnTransitSwitchPortTunnelIDs: `[1]`,
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeTrue())
		})

		It("should return true when encap IPs present but tunnel IDs missing", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID:                "1",
						util.OvnTransitSwitchPortAddr: `{"ipv4":"100.88.0.1/16"}`,
						util.OVNNodeEncapIPs:          `["10.1.1.1"]`,
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeTrue())
		})

		It("should return true when tunnel IDs present but encap IPs missing", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID:                     "1",
						util.OvnTransitSwitchPortAddr:      `{"ipv4":"100.88.0.1/16"}`,
						util.OvnTransitSwitchPortTunnelIDs: `[1]`,
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeTrue())
		})

		It("should return false when both encap IPs and tunnel IDs are missing", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNodeID:                "1",
						util.OvnTransitSwitchPortAddr: `{"ipv4":"100.88.0.1/16"}`,
					},
				},
			}
			Expect(needsZoneAllocation(node)).To(BeFalse())
		})
	})
})

var _ = Describe("Zone Cluster Controller", func() {
	var (
		fakeClient *util.OVNClusterManagerClientset
		zcc        *zoneClusterController
		wf         *factory.WatchFactory
		stopChan   chan struct{}
		wg         *sync.WaitGroup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())

		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
	})

	AfterEach(func() {

		if zcc != nil {
			zcc.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
		close(stopChan)
		wg.Wait()
	})

	Context("when EnableInterconnect is enabled", func() {
		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiVTEP = true
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.ClusterManager.V4TransitSubnet = "100.88.0.0/16"
			config.ClusterManager.V6TransitSubnet = "fd97::/64"
		})

		It("add nodes with different number of encap IPs", func() {
			// Create three test nodes with different number of encap IPs
			nodeNoEncapIP := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-no-encap",
					Annotations: map[string]string{
						util.OvnTransitSwitchPortTunnelIDs: `[3]`, // validate unuseful annotation should be removed
					},
				},
			}

			nodeOneEncapIP := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-one-encap",
					Annotations: map[string]string{
						util.OVNNodeEncapIPs: `["10.1.1.1"]`,
					},
				},
			}

			nodeTwoEncapIPs := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-two-encap",
					Annotations: map[string]string{
						util.OVNNodeEncapIPs: `["10.1.1.2", "10.1.1.3"]`,
					},
				},
			}

			// Create fake client with the nodes
			fakeClient = &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(nodeNoEncapIP, nodeOneEncapIP, nodeTwoEncapIPs),
			}

			var err error
			wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			// Create and start zoneClusterController to trigger add events
			zcc, err = newZoneClusterController(fakeClient, wf)
			Expect(err).NotTo(HaveOccurred())
			Expect(zcc).NotTo(BeNil())

			err = zcc.Start(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("Verify node with no encap IP")
			var updatedNode *corev1.Node
			Eventually(func() (int, error) {
				updatedNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeNoEncapIP.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				return util.GetNodeID(updatedNode)
			}, 3).Should(BeNumerically(">", 1))

			// k8s.ovn.org/node-transit-switch-port-tunnel-ids should not be set
			_, ok := updatedNode.Annotations[util.OvnTransitSwitchPortTunnelIDs]
			Expect(ok).To(BeFalse())

			updatedNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeNoEncapIP.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Verify transit switch port address is set
			_, ok = updatedNode.Annotations[util.OvnTransitSwitchPortAddr]
			Expect(ok).To(BeTrue())

			// Verify tunnel IDs annotation is not set (numEncapIPs <= 1)
			_, ok = updatedNode.Annotations[util.OvnTransitSwitchPortTunnelIDs]
			Expect(ok).To(BeFalse())

			By("Verify node with one encap IP")
			Eventually(func() (int, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeOneEncapIP.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				return util.GetNodeID(updatedNode)
			}, 3).Should(BeNumerically(">", 1))

			updatedNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeOneEncapIP.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Verify tunnel IDs annotation is not set (numEncapIPs <= 1)
			_, ok = updatedNode.Annotations[util.OvnTransitSwitchPortTunnelIDs]
			Expect(ok).To(BeFalse())

			By("Verify node with two encap IPs")
			var nodeID int
			Eventually(func() (int, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeTwoEncapIPs.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				nodeID, err = util.GetNodeID(updatedNode)
				return nodeID, err
			}, 3).Should(BeNumerically(">", 1))

			updatedNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeTwoEncapIPs.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Verify tunnel IDs annotation is set (numEncapIPs > 1)
			tunnelIDs, err := util.GetNodeTransitSwitchPortTunnelIDs(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(tunnelIDs).To(HaveLen(2))
			Expect(tunnelIDs[0]).To(Equal(nodeID)) // First tunnel ID should be the node ID
		})

		It("update node to change the number of encap IPs", func() {
			By("Add a node with one encap IP")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OVNNodeEncapIPs: `["10.1.1.1"]`,
					},
				},
			}

			fakeClient = &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(node),
			}

			var err error
			wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			zcc, err = newZoneClusterController(fakeClient, wf)
			Expect(err).NotTo(HaveOccurred())

			err = zcc.Start(context.Background())
			Expect(err).NotTo(HaveOccurred())

			// Wait for initial node processing (add event)
			Eventually(func() (int, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				return util.GetNodeID(updatedNode)
			}, 3).Should(BeNumerically(">", 1))

			By("Update node to add one more encap IP")
			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			updatedNode.Annotations[util.OVNNodeEncapIPs] = `["10.1.1.1", "10.1.1.2"]`
			updatedNode.ResourceVersion = "2"
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), updatedNode, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for tunnel IDs to be allocated via update event
			Eventually(func() (int, error) {
				node, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), updatedNode.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				tunnelIDs, err := util.GetNodeTransitSwitchPortTunnelIDs(node)
				return len(tunnelIDs), err
			}, 5, 0.5).Should(Equal(2))

			// Verify tunnel IDs are properly set
			finalNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			tunnelIDs, err := util.GetNodeTransitSwitchPortTunnelIDs(finalNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(tunnelIDs).To(HaveLen(2))

			nodeID, err := util.GetNodeID(finalNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(tunnelIDs[0]).To(Equal(nodeID))
		})

		It("delete node with multiple encap IPs", func() {
			// Start with a node with two encap IPs
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OVNNodeEncapIPs: `["10.1.1.3", "10.1.1.4"]`,
					},
				},
			}

			fakeClient = &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(node),
			}

			var err error
			wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			zcc, err = newZoneClusterController(fakeClient, wf)
			Expect(err).NotTo(HaveOccurred())

			err = zcc.Start(context.Background())
			Expect(err).NotTo(HaveOccurred())

			var updatedNode *corev1.Node
			Eventually(func() (int, error) {
				updatedNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				if err != nil {
					return 0, err
				}
				return util.GetNodeID(updatedNode)
			}, 3).Should(BeNumerically(">", 1))

			// Verify tunnel IDs annotation is set (numEncapIPs > 1)
			tunnelIDs, err := util.GetNodeTransitSwitchPortTunnelIDs(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(tunnelIDs).To(HaveLen(2))

			// mock zcc.nodeIDAllocator to test release tunnel IDs
			var callCount int32
			mockAllocator := &mockNodeIDAllocator{}
			mockAllocator.On("ReleaseID", node.Name).Run(func(args mock.Arguments) {
				atomic.AddInt32(&callCount, 1)
			}).Return()
			mockAllocator.On("ReleaseID", util.GetNodeIdName(node.Name, 1)).Run(func(args mock.Arguments) {
				atomic.AddInt32(&callCount, 1)
			}).Return()

			// Replace the real allocator with mock BEFORE deleting
			zcc.nodeIDAllocator = mockAllocator

			// Delete node to trigger the delete handler
			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for delete event to be processed and ReleaseID to be called
			// Thread-safe check using atomic counter
			Eventually(func() int32 {
				// use atomic.LoadInt32 instead of len(mockAllocator.Calls) here to avoid `WARNING: DATA RACE`
				return atomic.LoadInt32(&callCount)
			}, 3, "100ms").Should(Equal(int32(2)), "ReleaseID should be called twice (node ID + tunnel ID)")

			// Now verify the calls were made with correct arguments
			mockAllocator.AssertExpectations(GinkgoT())
		})
	})

})
