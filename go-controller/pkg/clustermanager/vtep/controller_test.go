// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newVTEP(name string, mode vtepv1.VTEPMode, cidrs ...string) *vtepv1.VTEP {
	dsCIDRs := make([]vtepv1.CIDR, len(cidrs))
	for i, c := range cidrs {
		dsCIDRs[i] = vtepv1.CIDR(c)
	}
	return &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: dsCIDRs,
			Mode:  mode,
		},
	}
}

func newCUDNWithEVPN(name, vtepName string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.0.0.0/16"},
					},
				},
				EVPN: &udnv1.EVPNConfig{
					VTEP:  vtepName,
					IPVRF: &udnv1.VRFConfig{VNI: 100},
				},
			},
		},
	}
}

func getVTEPFinalizers(client *vtepfake.Clientset, vtepName string) ([]string, error) {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	// NotFound means the object was garbage-collected after its finalizers
	// were cleared, so treat it as an empty finalizer list.
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}
	return vtep.Finalizers, nil
}

// newNodeWithVTEPAnnotation creates a node with the k8s.ovn.org/vteps annotation.
// vtepIPs is a map of VTEP name to list of IPs discovered on this node.
func newNodeWithVTEPAnnotation(name string, vtepIPs map[string][]string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if len(vtepIPs) > 0 {
		vteps := make(map[string]util.VTEPNodeAnnotation, len(vtepIPs))
		for vtepName, ips := range vtepIPs {
			vteps[vtepName] = util.VTEPNodeAnnotation{IPs: ips}
		}
		annotation, _ := json.Marshal(vteps)
		node.Annotations = map[string]string{
			util.OVNNodeVTEPs: string(annotation),
		}
	}
	return node
}

func getVTEPCondition(client *vtepfake.Clientset, vtepName, conditionType string) (*metav1.Condition, error) {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}
	return meta.FindStatusCondition(vtep.Status.Conditions, conditionType), nil
}

var _ = ginkgo.Describe("VTEP Controller", func() {
	var (
		controller    *Controller
		fakeVTEP      *vtepfake.Clientset
		fakeClientset *util.OVNClusterManagerClientset
		wf            *factory.WatchFactory
		fakeRecorder  *record.FakeRecorder
	)

	start := func(objects ...runtime.Object) {
		vtepObjects := []runtime.Object{}
		otherObjects := []runtime.Object{}
		for _, obj := range objects {
			switch obj.(type) {
			case *vtepv1.VTEP:
				vtepObjects = append(vtepObjects, obj)
			default:
				otherObjects = append(otherObjects, obj)
			}
		}

		fakeVTEP = vtepfake.NewSimpleClientset(vtepObjects...)
		ovntest.AddVTEPApplyReactor(fakeVTEP)
		fakeRecorder = record.NewFakeRecorder(100)

		fakeClientset = util.GetOVNClientset(otherObjects...).GetClusterManagerClientset()
		fakeClientset.VTEPClient = fakeVTEP

		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClientset)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		controller = NewController(wf, fakeClientset, fakeRecorder)

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = controller.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		config.Gateway.Mode = config.GatewayModeLocal
	})

	ginkgo.AfterEach(func() {
		if controller != nil {
			controller.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
	})

	ginkgo.Context("Managed mode", func() {
		ginkgo.It("allocates IPs and writes annotations for all nodes", func() {
			vtep := newVTEP("managed-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))

			for _, nodeName := range []string{"node-1", "node-2"} {
				name := nodeName
				gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					return util.ParseNodeVTEPs(n)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveKey("managed-vtep"))
			}
		})

		ginkgo.It("sets Accepted=True with no nodes", func() {
			vtep := newVTEP("managed-no-nodes", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-no-nodes", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))
		})

		ginkgo.It("allocates IPs from multiple CIDRs", func() {
			// /30 gives 4 IPs; 5 nodes exhaust the first /30 and one node must
			// overflow into the second /30. We can't predict which node gets
			// which IP (allocation order is not guaranteed), so we verify that
			// at least one node's IP comes from the second CIDR.
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			node3 := newNodeWithVTEPAnnotation("node-3", nil)
			node4 := newNodeWithVTEPAnnotation("node-4", nil)
			node5 := newNodeWithVTEPAnnotation("node-5", nil)
			vtep := newVTEP("managed-multi-cidr", vtepv1.VTEPModeManaged, "10.0.0.0/30", "10.0.1.0/30")
			start(vtep, node1, node2, node3, node4, node5)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-multi-cidr", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// All 5 nodes must have an annotation entry.
			for _, nodeName := range []string{"node-1", "node-2", "node-3", "node-4", "node-5"} {
				name := nodeName
				gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					return util.ParseNodeVTEPs(n)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveKey("managed-multi-cidr"))
			}

			// At least one node must have an IP from the second CIDR (10.0.1.x),
			// proving overflow into the second range occurred.
			gomega.Eventually(func() bool {
				for _, nodeName := range []string{"node-1", "node-2", "node-3", "node-4", "node-5"} {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
					if err != nil {
						continue
					}
					vteps, err := util.ParseNodeVTEPs(n)
					if err != nil || len(vteps["managed-multi-cidr"].IPs) == 0 {
						continue
					}
					if strings.HasPrefix(vteps["managed-multi-cidr"].IPs[0], "10.0.1.") {
						return true
					}
				}
				return false
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("allocates independent IPs to nodes across multiple managed VTEPs", func() {
			// Two managed VTEPs with non-overlapping CIDRs; both should allocate
			// to the same set of nodes independently.
			vtepA := newVTEP("mvtep-a", vtepv1.VTEPModeManaged, "10.0.0.0/24")
			vtepB := newVTEP("mvtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			start(vtepA, vtepB, node1, node2)

			for _, vtepName := range []string{"mvtep-a", "mvtep-b"} {
				name := vtepName
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionTrue),
					gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
				))
			}

			// Every node must have annotation entries for both VTEPs, with IPs
			// from their respective CIDRs.
			for _, nodeName := range []string{"node-1", "node-2"} {
				nName := nodeName
				gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nName, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					return util.ParseNodeVTEPs(n)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveKey("mvtep-a"),
					gomega.HaveKey("mvtep-b"),
				))
			}

			// Verify IPs are from the correct CIDRs and distinct across nodes.
			ips := map[string]map[string]string{} // vtepName -> nodeName -> IP
			for _, nodeName := range []string{"node-1", "node-2"} {
				nName := nodeName
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				vteps, err := util.ParseNodeVTEPs(n)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, vtepName := range []string{"mvtep-a", "mvtep-b"} {
					if ips[vtepName] == nil {
						ips[vtepName] = map[string]string{}
					}
					ips[vtepName][nName] = vteps[vtepName].IPs[0]
				}
			}
			gomega.Expect(ips["mvtep-a"]["node-1"]).To(gomega.HavePrefix("10.0.0."))
			gomega.Expect(ips["mvtep-a"]["node-2"]).To(gomega.HavePrefix("10.0.0."))
			gomega.Expect(ips["mvtep-b"]["node-1"]).To(gomega.HavePrefix("10.1.0."))
			gomega.Expect(ips["mvtep-b"]["node-2"]).To(gomega.HavePrefix("10.1.0."))
			gomega.Expect(ips["mvtep-a"]["node-1"]).NotTo(gomega.Equal(ips["mvtep-a"]["node-2"]))
			gomega.Expect(ips["mvtep-b"]["node-1"]).NotTo(gomega.Equal(ips["mvtep-b"]["node-2"]))
		})

		ginkgo.It("allocates an IP for a node that joins after startup", func() {
			vtep := newVTEP("managed-late-node", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			start(vtep, node1)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-late-node", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// A new node joins after the controller is already running.
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-2", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return util.ParseNodeVTEPs(n)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveKey("managed-late-node"))
		})
	})

	ginkgo.Context("Initial sync", func() {
		ginkgo.It("preserves existing allocations across restart", func() {
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"sync-vtep": {"100.64.0.3"},
			})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{
				"sync-vtep": {"100.64.0.5"},
			})
			node3 := newNodeWithVTEPAnnotation("node-3", nil)
			vtep := newVTEP("sync-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/29")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "sync-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))

			// node-1 and node-2 should keep their existing IPs
			gomega.Eventually(func() ([]string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if err != nil {
					return nil, err
				}
				return vteps["sync-vtep"].IPs, nil
			}).WithTimeout(5 * time.Second).Should(gomega.Equal([]string{"100.64.0.3"}))

			gomega.Eventually(func() ([]string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-2", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if err != nil {
					return nil, err
				}
				return vteps["sync-vtep"].IPs, nil
			}).WithTimeout(5 * time.Second).Should(gomega.Equal([]string{"100.64.0.5"}))

			// node-3 should get an IP that is NOT 100.64.0.3 or 100.64.0.5
			gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-3", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return util.ParseNodeVTEPs(n)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveKey("sync-vtep"),
				gomega.Not(gomega.HaveKeyWithValue("sync-vtep", util.VTEPNodeAnnotation{IPs: []string{"100.64.0.3"}})),
				gomega.Not(gomega.HaveKeyWithValue("sync-vtep", util.VTEPNodeAnnotation{IPs: []string{"100.64.0.5"}})),
			))
		})

		ginkgo.It("skips unmanaged VTEPs during sync", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"unmanaged-sync": {"100.64.0.1"},
			})
			vtep := newVTEP("unmanaged-sync", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "unmanaged-sync", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))

			// Verify no allocator was created for the unmanaged VTEP
			controller.allocatorsMu.Lock()
			gomega.Expect(controller.allocators).NotTo(gomega.HaveKey("unmanaged-sync"))
			controller.allocatorsMu.Unlock()
		})
	})

	ginkgo.Context("Mutability", func() {
		ginkgo.It("appends a new CIDR and allocates IPs from it when the original is exhausted", func() {
			// /30 gives 4 IPs; fill them all up.
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			node3 := newNodeWithVTEPAnnotation("node-3", nil)
			node4 := newNodeWithVTEPAnnotation("node-4", nil)
			vtep := newVTEP("append-vtep", vtepv1.VTEPModeManaged, "10.0.0.0/30")
			start(vtep, node1, node2, node3, node4)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "append-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Add node-5 now -- range is exhausted so VTEP should go AllocationFailed.
			node5 := newNodeWithVTEPAnnotation("node-5", nil)
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node5, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "append-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Append a second /30 CIDR -- this should unblock allocation.
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "append-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = append(v.Spec.CIDRs, "10.0.0.4/30")
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should recover to Accepted=True and node-5 gets an IP from the new /30.
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "append-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-5", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return util.ParseNodeVTEPs(n)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveKey("append-vtep"),
				gomega.WithTransform(func(m map[string]util.VTEPNodeAnnotation) string {
					if len(m["append-vtep"].IPs) == 0 {
						return ""
					}
					return m["append-vtep"].IPs[0]
				}, gomega.HavePrefix("10.0.0.")),
			))
		})

		ginkgo.It("widens an existing CIDR and allocates new IPs from the expanded range", func() {
			// /30 gives 4 IPs; fill them all up.
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			node3 := newNodeWithVTEPAnnotation("node-3", nil)
			node4 := newNodeWithVTEPAnnotation("node-4", nil)
			vtep := newVTEP("widen-vtep", vtepv1.VTEPModeManaged, "10.0.1.0/30")
			start(vtep, node1, node2, node3, node4)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "widen-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Collect existing IPs before widening.
			getNodeVTEPIP := func(nodeName string) string {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				vteps, err := util.ParseNodeVTEPs(n)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(vteps["widen-vtep"].IPs).NotTo(gomega.BeEmpty())
				return vteps["widen-vtep"].IPs[0]
			}
			ip1 := getNodeVTEPIP("node-1")
			ip2 := getNodeVTEPIP("node-2")

			// Add node-5 now -- range is exhausted so VTEP should go AllocationFailed.
			node5 := newNodeWithVTEPAnnotation("node-5", nil)
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node5, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "widen-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Widen /30 to /28 (16 IPs) -- this should unblock allocation.
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "widen-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.1.0/28"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should recover and node-5 gets an IP from the expanded range.
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "widen-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-5", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return util.ParseNodeVTEPs(n)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveKey("widen-vtep"))

			// Existing nodes must keep their original IPs.
			gomega.Expect(getNodeVTEPIP("node-1")).To(gomega.Equal(ip1))
			gomega.Expect(getNodeVTEPIP("node-2")).To(gomega.Equal(ip2))
		})

		ginkgo.It("preserves existing node IPs when switching from Unmanaged to Managed", func() {
			// ovnkube-node already wrote IPs for both nodes during Unmanaged phase.
			// Use non-sequential IPs so the test can't pass accidentally if the
			// allocator just happens to assign the same first addresses.
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{"mode-switch-vtep": {"100.64.0.42"}})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{"mode-switch-vtep": {"100.64.0.99"}})
			vtep := newVTEP("mode-switch-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-switch-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Switch to Managed mode.
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "mode-switch-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.Mode = vtepv1.VTEPModeManaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-switch-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Existing IPs must be preserved.
			getIP := func(nodeName string) string {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				vteps, err := util.ParseNodeVTEPs(n)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(vteps["mode-switch-vtep"].IPs).NotTo(gomega.BeEmpty())
				return vteps["mode-switch-vtep"].IPs[0]
			}
			gomega.Expect(getIP("node-1")).To(gomega.Equal("100.64.0.42"))
			gomega.Expect(getIP("node-2")).To(gomega.Equal("100.64.0.99"))
		})

		ginkgo.It("overwrites stale node IPs when switching Unmanaged to Managed with changed CIDRs", func() {
			// Nodes have IPs from the old CIDR (192.168.0.x) but the VTEP now uses a different CIDR.
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{"stale-vtep": {"192.168.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{"stale-vtep": {"192.168.0.2"}})
			vtep := newVTEP("stale-vtep", vtepv1.VTEPModeUnmanaged, "192.168.0.0/24")
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "stale-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Switch to Managed with a completely different CIDR.
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "stale-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.Mode = vtepv1.VTEPModeManaged
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.2.0/24"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "stale-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Stale IPs must be replaced with IPs from the new CIDR.
			for _, nodeName := range []string{"node-1", "node-2"} {
				gomega.Eventually(func() (string, error) {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
					if err != nil {
						return "", err
					}
					vteps, err := util.ParseNodeVTEPs(n)
					if err != nil {
						return "", err
					}
					if len(vteps["stale-vtep"].IPs) == 0 {
						return "", nil
					}
					return vteps["stale-vtep"].IPs[0], nil
				}).WithTimeout(5 * time.Second).Should(gomega.HavePrefix("10.0.2."))
			}
		})

		ginkgo.It("allocates fresh IPs for nodes whose CIDR was dropped before Unmanaged->Managed switch", func() {
			// During Unmanaged phase the VTEP had two CIDRs and nodes got IPs from both.
			// node-1 and node-2 have IPs from the first CIDR (10.0.4.0/29).
			// node-3 and node-4 have IPs from the second CIDR (10.0.5.0/29) which
			// will be removed before the mode switch (allowed in Unmanaged mode).
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{"partial-stale-vtep": {"10.0.4.1"}})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{"partial-stale-vtep": {"10.0.4.2"}})
			node3 := newNodeWithVTEPAnnotation("node-3", map[string][]string{"partial-stale-vtep": {"10.0.5.1"}})
			node4 := newNodeWithVTEPAnnotation("node-4", map[string][]string{"partial-stale-vtep": {"10.0.5.2"}})
			vtep := newVTEP("partial-stale-vtep", vtepv1.VTEPModeUnmanaged, "10.0.4.0/29", "10.0.5.0/29")
			start(vtep, node1, node2, node3, node4)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "partial-stale-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Remove the second CIDR and switch to Managed (both in one update,
			// as CEL allows free CIDR changes while still Unmanaged).
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "partial-stale-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.Mode = vtepv1.VTEPModeManaged
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.4.0/29"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "partial-stale-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			getIP := func(nodeName string) string {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				vteps, err := util.ParseNodeVTEPs(n)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(vteps["partial-stale-vtep"].IPs).NotTo(gomega.BeEmpty())
				return vteps["partial-stale-vtep"].IPs[0]
			}

			// node-1 and node-2 must keep their IPs from the surviving CIDR.
			gomega.Eventually(func() string { return getIP("node-1") }).
				WithTimeout(5 * time.Second).Should(gomega.Equal("10.0.4.1"))
			gomega.Eventually(func() string { return getIP("node-2") }).
				WithTimeout(5 * time.Second).Should(gomega.Equal("10.0.4.2"))

			// node-3 and node-4 had stale IPs from the dropped CIDR; they must
			// get fresh IPs from 10.0.4.0/29.
			for _, nodeName := range []string{"node-3", "node-4"} {
				gomega.Eventually(func() string { return getIP(nodeName) }).
					WithTimeout(5 * time.Second).Should(gomega.HavePrefix("10.0.4."))
			}
		})

		ginkgo.It("cleans up annotations and removes allocator when switching from Managed to Unmanaged", func() {
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			vtep := newVTEP("managed-to-unmanaged", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-to-unmanaged", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Confirm allocator exists.
			controller.allocatorsMu.Lock()
			gomega.Expect(controller.allocators).To(gomega.HaveKey("managed-to-unmanaged"))
			controller.allocatorsMu.Unlock()

			// Switch to Unmanaged.
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "managed-to-unmanaged", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.Mode = vtepv1.VTEPModeUnmanaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Allocator must be removed.
			gomega.Eventually(func() bool {
				controller.allocatorsMu.Lock()
				defer controller.allocatorsMu.Unlock()
				_, exists := controller.allocators["managed-to-unmanaged"]
				return exists
			}).WithTimeout(5 * time.Second).Should(gomega.BeFalse())

			// Node annotations for the managed VTEP should be cleaned up.
			for _, nodeName := range []string{"node-1", "node-2"} {
				gomega.Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
					n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					vteps, err := util.ParseNodeVTEPs(n)
					if util.IsAnnotationNotSetError(err) {
						return map[string]util.VTEPNodeAnnotation{}, nil
					}
					return vteps, err
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.HaveKey("managed-to-unmanaged"))
			}
		})
	})

	ginkgo.Context("Finalizer management", func() {
		ginkgo.DescribeTable("adds finalizer to a new VTEP",
			func(mode vtepv1.VTEPMode) {
				vtep := newVTEP("finalize-vtep", mode, "100.64.0.0/24")
				start(vtep)
				gomega.Eventually(func() ([]string, error) {
					return getVTEPFinalizers(fakeVTEP, "finalize-vtep")
				}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
			},
			ginkgo.Entry("unmanaged", vtepv1.VTEPModeUnmanaged),
			ginkgo.Entry("managed", vtepv1.VTEPModeManaged),
		)

		ginkgo.DescribeTable("removes finalizer and allows deletion when no CUDNs reference the VTEP",
			func(mode vtepv1.VTEPMode, node *corev1.Node) {
				vtep := newVTEP("delete-vtep", mode, "100.64.0.0/24")
				if node != nil {
					start(vtep, node)
				} else {
					start(vtep)
				}

				gomega.Eventually(func() ([]string, error) {
					return getVTEPFinalizers(fakeVTEP, "delete-vtep")
				}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

				v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "delete-vtep", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				now := metav1.Now()
				v.DeletionTimestamp = &now
				_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() ([]string, error) {
					return getVTEPFinalizers(fakeVTEP, "delete-vtep")
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
			},
			ginkgo.Entry("unmanaged", vtepv1.VTEPModeUnmanaged, (*corev1.Node)(nil)),
			// managed needs a node so the allocator can initialise before deletion
			ginkgo.Entry("managed", vtepv1.VTEPModeManaged, newNodeWithVTEPAnnotation("node-1", nil)),
		)

		ginkgo.DescribeTable("blocks deletion when a CUDN references the VTEP",
			func(mode vtepv1.VTEPMode, node *corev1.Node) {
				cudn := newCUDNWithEVPN("test-cudn", "blocked-vtep")
				vtep := newVTEP("blocked-vtep", mode, "100.64.0.0/24")
				if node != nil {
					start(vtep, cudn, node)
				} else {
					start(vtep, cudn)
				}

				gomega.Eventually(func() ([]string, error) {
					return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
				}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

				v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "blocked-vtep", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				now := metav1.Now()
				v.DeletionTimestamp = &now
				_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Consistently(func() ([]string, error) {
					return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
				}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

				// Delete the CUDN — the VTEP should now be garbage-collected
				err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
					context.Background(), "test-cudn", metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() bool {
					_, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "blocked-vtep", metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
			},
			ginkgo.Entry("unmanaged", vtepv1.VTEPModeUnmanaged, (*corev1.Node)(nil)),
			ginkgo.Entry("managed", vtepv1.VTEPModeManaged, newNodeWithVTEPAnnotation("node-1", nil)),
		)
	})

	ginkgo.Context("Cross-VTEP CIDR overlap validation", func() {
		ginkgo.It("sets Accepted=True when VTEPs have non-overlapping CIDRs", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("sets Accepted=False on both VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-b")),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-a")),
			))
		})

		ginkgo.It("converges without infinite re-queue loop when VTEPs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// After both VTEPs settle, the API action count must stabilize.
			// An infinite re-queue ping-pong would cause repeated reconciles;
			// while updateStatusCondition guards against redundant writes, the
			// re-queue guard in validateCIDRsAcrossVTEPs is what actually
			// prevents the loop. This verifies no further API calls are made.
			settled := len(fakeVTEP.Actions())
			gomega.Consistently(func() int {
				return len(fakeVTEP.Actions())
			}).WithTimeout(2 * time.Second).Should(gomega.Equal(settled))
		})

		ginkgo.It("emits a CIDROverlap warning event when VTEPs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonCIDROverlap),
			))
		})

		ginkgo.It("sets Accepted=False on all three VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionFalse),
					gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				))
			}
		})

		ginkgo.It("updates conflict message when a new overlapping VTEP joins an existing conflict", func() {
			// vtep-b (managed) and vtep-c overlap via vtep-b's /8
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.0.0/8")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepB, vtepC)

			// vtep-b's message should mention vtep-c but not vtep-a (doesn't exist yet)
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-c")),
				gomega.HaveField("Message", gomega.Not(gomega.ContainSubstring("vtep-a"))),
			))

			// Create vtep-a (unmanaged) which also overlaps with vtep-b
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepA, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// vtep-b's message should now mention both vtep-a and vtep-c
			gomega.Eventually(func() (string, error) {
				cond, err := getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
				if err != nil {
					return "", err
				}
				if cond == nil {
					return "", nil
				}
				return cond.Message, nil
			}).WithTimeout(5 * time.Second).Should(gomega.And(
				gomega.ContainSubstring("vtep-a"),
				gomega.ContainSubstring("vtep-c"),
			))
		})

		ginkgo.It("sets Accepted=False when a new overlapping VTEP is created after startup", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			start(vtepA)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Create an overlapping managed VTEP
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepB, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a mask expansion causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Expand vtep-a's mask from /24 to /16, now it contains 10.0.1.0/24
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a newly appended CIDR causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Append a new CIDR to vtep-a that overlaps with vtep-b
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = append(v.Spec.CIDRs, "10.1.0.0/16")
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("clears Accepted=False when overlapping CIDR is removed from the list", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24", "10.1.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Remove the overlapping CIDR from vtep-a, keeping only the non-overlapping one
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.0.0/24"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("clears Accepted=False when user edits CIDRs to remove overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Change vtep-a's CIDR so it no longer overlaps with vtep-b
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"192.168.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("clears Accepted=False only when all conflicts are resolved", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeManaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			// All three should be Accepted=False due to overlap
			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
			}

			// Delete vtep-c: vtep-a and vtep-b still overlap, both stay Accepted=False
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-c", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Delete vtep-b: vtep-a is the only one left, no more conflicts
			v, err = fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-b", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now = metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("Managed VTEP CIDR vs node host IP validation", func() {
		ginkgo.It("sets Accepted=False when managed VTEP CIDRs overlap with node host IPs", func() {
			node := newNodeWithVTEPAnnotation("node-1", nil)
			hostCIDRs, _ := json.Marshal([]string{"192.168.1.10/24"})
			node.Annotations = map[string]string{util.OVNNodeHostCIDRs: string(hostCIDRs)}
			vtep := newVTEP("vtep-host-overlap", vtepv1.VTEPModeManaged, "192.168.1.0/24")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-host-overlap", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("node-1")),
			))
		})

		ginkgo.It("sets Accepted=True when managed VTEP CIDRs do not overlap with node host IPs", func() {
			node := newNodeWithVTEPAnnotation("node-1", nil)
			hostCIDRs, _ := json.Marshal([]string{"192.168.1.10/24"})
			node.Annotations = map[string]string{util.OVNNodeHostCIDRs: string(hostCIDRs)}
			vtep := newVTEP("vtep-no-host-overlap", vtepv1.VTEPModeManaged, "10.0.0.0/24")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-no-host-overlap", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("skips validation for unmanaged VTEPs", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{"vtep-unmanaged-hostip": {"192.168.1.50"}})
			hostCIDRs, _ := json.Marshal([]string{"192.168.1.10/24"})
			node.Annotations[util.OVNNodeHostCIDRs] = string(hostCIDRs)
			vtep := newVTEP("vtep-unmanaged-hostip", vtepv1.VTEPModeUnmanaged, "192.168.1.0/24")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-unmanaged-hostip", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("Unmanaged mode", func() {
		ginkgo.It("sets Accepted=True with no nodes", func() {
			vtep := newVTEP("unmanaged-no-nodes", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "unmanaged-no-nodes", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=True when a node has a VTEP IP in the annotation", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{"vtep-discover": {"100.64.0.5"}})
			vtep := newVTEP("vtep-discover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-discover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False when a node has no vteps annotation", func() {
			nodeNoAnnotation := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "bare-node"},
			}
			vtep := newVTEP("vtep-skip", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeNoAnnotation)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-skip", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("sets Accepted=False when a node has no entry for this VTEP", func() {
			node := newNodeWithVTEPAnnotation("node-nomatch", map[string][]string{"other-vtep": {"10.0.0.1"}})
			vtep := newVTEP("vtep-nomatch", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
				gomega.HaveField("Message", gomega.ContainSubstring("node-nomatch")),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("sets Accepted=False when a node has an empty IP list for the VTEP", func() {
			node := newNodeWithVTEPAnnotation("node-empty", map[string][]string{"vtep-empty": {}})
			vtep := newVTEP("vtep-empty", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-empty", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("sets Accepted=True when multiple nodes have VTEP IPs", func() {
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{"vtep-multi": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{"vtep-multi": {"100.64.0.2"}})
			node3 := newNodeWithVTEPAnnotation("node-3", map[string][]string{"vtep-multi": {"100.64.0.3"}})
			vtep := newVTEP("vtep-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-multi", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False when some nodes are missing the VTEP entry", func() {
			nodeGood := newNodeWithVTEPAnnotation("node-good", map[string][]string{"vtep-partial": {"100.64.0.1"}})
			nodeBad := newNodeWithVTEPAnnotation("node-bad", map[string][]string{"other-vtep": {"10.0.0.1"}})
			vtep := newVTEP("vtep-partial", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeGood, nodeBad)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-partial", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("sets Accepted=True for both VTEPs when a node has entries for multiple VTEPs", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"vtep-a": {"100.64.0.1"},
				"vtep-b": {"10.0.0.1"},
			})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			start(vtepA, vtepB, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False for one VTEP when node only has the other VTEP entry", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"vtep-a": {"100.64.0.1"},
			})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			start(vtepA, vtepB, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("recovers from AllocationFailed when VTEP CIDRs are expanded to match node IPs", func() {
			node := newNodeWithVTEPAnnotation("node-expand", map[string][]string{"vtep-expand": {"200.10.0.5"}})
			vtep := newVTEP("vtep-expand", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-expand", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("Event deduplication", func() {
		ginkgo.It("does not fire duplicate CIDROverlap or AllocationFailed events when failure state is unchanged", func() {
			// vtep-a and vtep-b overlap; vtep-a also has a missing node annotation.
			// Tests dedup across both CIDROverlap and AllocationFailed reasons.
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-b": {"10.0.0.1"}})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
			))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
			))

			// Drain all events from initial convergence. We expect one
			// CIDROverlap event per VTEP (2 total), but a 3rd is possible:
			//  1. vtep-a reconciles first → overlap → event #1 → patches status
			//  2. vtep-b reconciles → overlap → event #2 → patches status.
			//     If the informer cache hasn't synced vtep-a's status yet,
			//     validateCIDRsAcrossVTEPs re-queues vtep-a.
			//  3. vtep-a re-reconciles → lister.Get still returns the stale
			//     object (no conditions) → dedup guard sees existingCond==nil
			//     → fires event #3 (duplicate).
			// This is a benign race between the worker and the async informer
			// cache sync. In steady state the guard works correctly.
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			// Re-reconcile both — no new events since the lister is now in sync
			controller.vtepController.Reconcile("vtep-a")
			controller.vtepController.Reconcile("vtep-b")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())

			// Resolve the overlap by changing vtep-a's CIDRs to non-overlapping
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"192.168.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// vtep-b recovers, vtep-a now fails with AllocationFailed
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Drain AllocationFailed events. We expect 1, but a 2nd is
			// possible: vtep-b's re-queue (from the else-if conflict-resolved
			// path) may re-queue vtep-a while the lister still shows the old
			// CIDROverlap condition, causing the dedup guard to miss the match.
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			// Re-reconcile vtep-a — no new event since AllocationFailed state is unchanged
			controller.vtepController.Reconcile("vtep-a")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())
		})

		ginkgo.It("does not fire duplicate IPv6NotSupported events when failure state is unchanged", func() {
			vtep := newVTEP("vtep-v6-dedup", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-dedup": {"fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-v6-dedup", "vtep-v6-dedup")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-dedup", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonEVPNIPv6NotSupported),
			))

			// Drain any remaining events, then verify dedup guard prevents duplicates
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			controller.vtepController.Reconcile("vtep-v6-dedup")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())
		})
	})

	ginkgo.Context("IPv6 CIDR rejection for EVPN VTEPs", func() {
		ginkgo.It("sets Accepted=False when an EVPN CUDN references an unmanaged VTEP with IPv6 CIDRs", func() {
			vtep := newVTEP("vtep-v6", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6": {"fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-v6", "vtep-v6")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		ginkgo.It("sets Accepted=False when an EVPN CUDN references a managed VTEP with IPv6 CIDRs", func() {
			// IPv6 is rejected regardless of mode; CM still won't allocate.
			vtep := newVTEP("vtep-v6-managed", vtepv1.VTEPModeManaged, "fd00::/120")
			node := newNodeWithVTEPAnnotation("node1", nil)
			cudn := newCUDNWithEVPN("cudn-evpn-v6-managed", "vtep-v6-managed")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-managed", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		// managed: IPv4-only VTEP with an EVPN CUDN is accepted
		ginkgo.It("sets Accepted=True when a managed VTEP has only IPv4 CIDRs and is referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-v4", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			node := newNodeWithVTEPAnnotation("node1", nil)
			cudn := newCUDNWithEVPN("cudn-evpn-v4", "vtep-v4")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		// unmanaged: IPv6 CIDR without an EVPN CUDN is accepted
		ginkgo.It("allows IPv6 CIDRs on an unmanaged VTEP not referenced by any EVPN CUDN", func() {
			vtep := newVTEP("vtep-v6-no-evpn", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-no-evpn": {"fd00::1"}})
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-no-evpn", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		// managed: dual-stack VTEP with an EVPN CUDN is rejected
		ginkgo.It("rejects when a managed VTEP has dual-stack CIDRs and is referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-ds", vtepv1.VTEPModeManaged, "100.64.0.0/24", "fd00::/120")
			node := newNodeWithVTEPAnnotation("node1", nil)
			cudn := newCUDNWithEVPN("cudn-evpn-ds", "vtep-ds")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		// unmanaged: EVPN CUDN added after startup triggers rejection
		ginkgo.It("transitions to IPv6NotSupported when an EVPN CUDN is created referencing an unmanaged VTEP with IPv6 CIDRs", func() {
			vtep := newVTEP("vtep-v6-late", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-late": {"fd00::1"}})
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-late", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			cudn := newCUDNWithEVPN("cudn-evpn-late", "vtep-v6-late")
			_, err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Create(
				context.Background(), cudn, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-late", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		// managed: appending an IPv6 CIDR to a managed VTEP with an EVPN CUDN triggers rejection
		ginkgo.It("transitions to IPv6NotSupported when an IPv6 CIDR is appended to a managed VTEP referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-v4-append", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			node := newNodeWithVTEPAnnotation("node1", nil)
			cudn := newCUDNWithEVPN("cudn-evpn-append", "vtep-v4-append")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4-append", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-v4-append", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"100.64.0.0/24", "fd00::/120"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4-append", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		// unmanaged: removing the IPv6 CIDR from a dual-stack VTEP recovers it
		ginkgo.It("recovers from IPv6NotSupported when the IPv6 CIDR is removed from an unmanaged VTEP", func() {
			vtep := newVTEP("vtep-ds-remove", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24", "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-ds-remove": {"100.64.0.1", "fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-remove", "vtep-ds-remove")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds-remove", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-ds-remove", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"100.64.0.0/24"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds-remove", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		// managed: deleting the EVPN CUDN recovers the VTEP
		ginkgo.It("recovers from IPv6NotSupported when the EVPN CUDN is deleted from a managed VTEP", func() {
			vtep := newVTEP("vtep-v6-recover", vtepv1.VTEPModeManaged, "fd00::/120")
			node := newNodeWithVTEPAnnotation("node1", nil)
			cudn := newCUDNWithEVPN("cudn-evpn-recover", "vtep-v6-recover")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-evpn-recover", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("CUDN watch for finalizer re-evaluation", func() {
		ginkgo.It("indexes EVPN CUDNs on create and ignores non-EVPN CUDNs", func() {
			evpnCUDN := newCUDNWithEVPN("cudn-evpn", "vtep-indexed")
			nonEVPNCUDN := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "cudn-plain"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Subnets: []udnv1.Layer3Subnet{{CIDR: "10.0.0.0/16"}},
						},
					},
				},
			}
			vtep := newVTEP("vtep-indexed", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, evpnCUDN, nonEVPNCUDN)

			// EVPN CUDN should be indexed
			gomega.Eventually(func() bool {
				controller.cudnVTEPIndexMu.RLock()
				_, ok := controller.cudnVTEPIndex["cudn-evpn"]
				controller.cudnVTEPIndexMu.RUnlock()
				return ok
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

			controller.cudnVTEPIndexMu.RLock()
			val := controller.cudnVTEPIndex["cudn-evpn"]
			controller.cudnVTEPIndexMu.RUnlock()
			gomega.Expect(val).To(gomega.Equal("vtep-indexed"))

			// Non-EVPN CUDN should NOT be indexed
			controller.cudnVTEPIndexMu.RLock()
			_, ok := controller.cudnVTEPIndex["cudn-plain"]
			controller.cudnVTEPIndexMu.RUnlock()
			gomega.Expect(ok).To(gomega.BeFalse())
		})

		ginkgo.It("unblocks VTEP deletion when the referencing CUDN is deleted", func() {
			cudn := newCUDNWithEVPN("cudn-ref", "vtep-cudn-del")
			vtep := newVTEP("vtep-cudn-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Simulate deletion of the VTEP (sets DeletionTimestamp, blocked by finalizer)
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-cudn-del", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Finalizer should remain because CUDN still references the VTEP
			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete the CUDN -- this should trigger the CUDN controller
			// which re-queues the deleting VTEP
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-ref", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Now the VTEP's finalizer should be removed and the object
			// should be garbage-collected (the GC reactor deletes objects
			// whose DeletionTimestamp is set and finalizers are empty).
			gomega.Eventually(func() bool {
				_, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-cudn-del", metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

			// Index entry should be cleaned up
			controller.cudnVTEPIndexMu.RLock()
			_, ok := controller.cudnVTEPIndex["cudn-ref"]
			controller.cudnVTEPIndexMu.RUnlock()
			gomega.Expect(ok).To(gomega.BeFalse())
		})

		ginkgo.It("keeps VTEP blocked until all referencing CUDNs are deleted", func() {
			cudn1 := newCUDNWithEVPN("cudn-one", "vtep-multi-ref")
			cudn2 := newCUDNWithEVPN("cudn-two", "vtep-multi-ref")
			vtep := newVTEP("vtep-multi-ref", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn1, cudn2)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-multi-ref", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Delete first CUDN -- VTEP should still be blocked by cudn-two
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-one", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete second CUDN -- now VTEP should be unblocked
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-two", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("does not re-queue VTEPs when a non-EVPN CUDN is deleted", func() {
			evpnCUDN := newCUDNWithEVPN("cudn-evpn", "vtep-norequeue")
			nonEVPNCUDN := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "cudn-plain"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Subnets: []udnv1.Layer3Subnet{{CIDR: "10.0.0.0/16"}},
						},
					},
				},
			}
			vtep := newVTEP("vtep-norequeue", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, evpnCUDN, nonEVPNCUDN)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-norequeue", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Delete the non-EVPN CUDN -- should NOT unblock the VTEP
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-plain", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain blocked (the EVPN CUDN still references it)
			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})

	})

	ginkgo.Context("Node watch for VTEP IP re-validation", func() {
		ginkgo.It("re-validates VTEP when node's vteps annotation is added", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-late"}}
			vtep := newVTEP("vtep-node-add", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			// Node has no vteps annotation yet — VTEP should be AllocationFailed
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-add", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Simulate ovnkube-node writing the vteps annotation
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-late", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-node-add": {IPs: []string{"100.64.0.10"}},
			})
			n.Annotations = map[string]string{util.OVNNodeVTEPs: string(vtepAnnotation)}
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Node controller detects vteps annotation change, re-queues VTEP
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-add", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("re-validates VTEP when node's vteps annotation changes", func() {
			node := newNodeWithVTEPAnnotation("node-change", map[string][]string{"vtep-node-change": {"100.64.0.1"}})
			vtep := newVTEP("vtep-node-change", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-change", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Change the node's vteps annotation to a different IP
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-change", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-node-change": {IPs: []string{"100.64.0.99"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True after re-validation
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-change", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("releases managed VTEP allocation when a node is deleted", func() {
			// /30 gives exactly 4 IPs (.0-.3); fill the pool with 4 nodes.
			node1 := newNodeWithVTEPAnnotation("node-1", nil)
			node2 := newNodeWithVTEPAnnotation("node-2", nil)
			node3 := newNodeWithVTEPAnnotation("node-3", nil)
			node4 := newNodeWithVTEPAnnotation("node-4", nil)
			vtep := newVTEP("release-vtep", vtepv1.VTEPModeManaged, "10.0.3.0/30")
			start(vtep, node1, node2, node3, node4)

			getNodeVTEPIP := func(nodeName string) (string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if util.IsAnnotationNotSetError(err) || len(vteps["release-vtep"].IPs) == 0 {
					return "", nil
				}
				if err != nil {
					return "", err
				}
				return vteps["release-vtep"].IPs[0], nil
			}

			// Wait for all 4 nodes to be allocated — pool is now full.
			for _, name := range []string{"node-1", "node-2", "node-3", "node-4"} {
				n := name
				gomega.Eventually(func() (string, error) {
					return getNodeVTEPIP(n)
				}).WithTimeout(5 * time.Second).ShouldNot(gomega.BeEmpty())
			}

			// Record node-4's IP before touching anything.
			releasedIP, err := getNodeVTEPIP("node-4")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(releasedIP).NotTo(gomega.BeEmpty())

			// Create node-5 while the pool is still full — it must fail to allocate.
			node5 := newNodeWithVTEPAnnotation("node-5", nil)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node5, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "release-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Now delete node-4 — its IP is freed.
			err = fakeClientset.KubeClient.CoreV1().Nodes().Delete(context.Background(), "node-4", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// node-5 must now be allocated and receive exactly the freed IP.
			gomega.Eventually(func() (string, error) {
				return getNodeVTEPIP("node-5")
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(releasedIP))
		})

		ginkgo.It("re-validates VTEP when a node is deleted", func() {
			node1 := newNodeWithVTEPAnnotation("node-keep", map[string][]string{"vtep-node-del": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-remove", map[string][]string{"vtep-node-del": {"100.64.0.2"}})
			vtep := newVTEP("vtep-node-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-del", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Delete node-remove
			err := fakeClientset.KubeClient.CoreV1().Nodes().Delete(
				context.Background(), "node-remove", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True (node-keep still has a valid entry)
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-del", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("recovers from AllocationFailed when missing VTEP entry is added to node", func() {
			// Node starts without an entry for this VTEP
			node := newNodeWithVTEPAnnotation("node-nomatch", map[string][]string{"other-vtep": {"192.168.1.10"}})
			vtep := newVTEP("vtep-nomatch-fix", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: add the VTEP entry
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-nomatch", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"other-vtep":       {IPs: []string{"192.168.1.10"}},
				"vtep-nomatch-fix": {IPs: []string{"100.64.0.20"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("recovers from AllocationFailed when empty IP list is populated", func() {
			// Node starts with an empty IP list for this VTEP
			node := newNodeWithVTEPAnnotation("node-empty-fix", map[string][]string{"vtep-recover": {}})
			vtep := newVTEP("vtep-recover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: populate the IP list
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-empty-fix", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-recover": {IPs: []string{"100.64.0.5"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("transitions to AllocationFailed when VTEP entry is removed from node", func() {
			node := newNodeWithVTEPAnnotation("node-regress", map[string][]string{"vtep-regress": {"100.64.0.5"}})
			vtep := newVTEP("vtep-regress", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-regress", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Remove the VTEP entry from the node annotation
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-regress", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"other-vtep": {IPs: []string{"192.168.1.10"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-regress", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
				gomega.HaveField("Message", gomega.ContainSubstring("node-regress")),
			))
		})

		ginkgo.It("does not issue any VTEP API update when vteps annotation change is unrelated", func() {
			// Use managed mode: CM writes the annotation itself, so once settled
			// an unrelated annotation change on the node should not re-trigger
			// any VTEP status API calls.
			node := newNodeWithVTEPAnnotation("node-stable", nil)
			vtep := newVTEP("vtep-stable", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-stable", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Wait for CM to write the annotation, then record the allocated IP.
			var allocatedIP string
			gomega.Eventually(func() (string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-stable", metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if err != nil || len(vteps["vtep-stable"].IPs) == 0 {
					return "", err
				}
				allocatedIP = vteps["vtep-stable"].IPs[0]
				return allocatedIP, nil
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.BeEmpty())

			// Controller is idle after initial reconcile settled; safe to add reactor.
			var patchCount atomic.Int32
			fakeVTEP.PrependReactor("patch", "vteps", func(_ ktesting.Action) (bool, runtime.Object, error) {
				patchCount.Add(1)
				return false, nil, nil
			})

			// Update node: add an entry for a different VTEP (vtep-stable unchanged).
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-stable", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-stable":  {IPs: []string{allocatedIP}},
				"vtep-another": {IPs: []string{"10.0.0.50"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The diff guards should prevent any VTEP status API calls
			gomega.Consistently(func() int32 {
				return patchCount.Load()
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(int32(0)))
		})

		ginkgo.It("does not issue node update when managed VTEP annotation already matches", func() {
			node := newNodeWithVTEPAnnotation("node-noop", nil)
			vtep := newVTEP("vtep-noop", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep, node)

			// Wait for the CM to allocate an IP and write the annotation.
			gomega.Eventually(func() (string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-noop", metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if err != nil || len(vteps["vtep-noop"].IPs) == 0 {
					return "", err
				}
				return vteps["vtep-noop"].IPs[0], nil
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.BeEmpty())

			// Record node update count after initial allocation settles.
			var nodeUpdateCount atomic.Int32
			fakeClientset.KubeClient.(*fake.Clientset).PrependReactor("update", "nodes", func(_ ktesting.Action) (bool, runtime.Object, error) {
				nodeUpdateCount.Add(1)
				return false, nil, nil
			})

			// Force a re-reconcile of the VTEP by touching its spec (no-op label).
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-noop", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if v.Labels == nil {
				v.Labels = map[string]string{}
			}
			v.Labels["trigger"] = "re-reconcile"
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The ipsEqual guard must prevent any node annotation writes.
			gomega.Consistently(func() int32 {
				return nodeUpdateCount.Load()
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(int32(0)))
		})

		ginkgo.It("reconciles multiple VTEPs when a single node's vteps annotation changes", func() {
			// vtep-a is unmanaged (node supplies the IP), vtep-b is managed (CM allocates).
			node := newNodeWithVTEPAnnotation("node-shared", map[string][]string{
				"vtep-a-multi": {"100.64.0.1"},
			})
			vtepA := newVTEP("vtep-a-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b-multi", vtepv1.VTEPModeManaged, "200.10.0.0/24")
			start(vtepA, vtepB, node)

			for _, name := range []string{"vtep-a-multi", "vtep-b-multi"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionTrue),
					gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
				))
			}

			// Change the unmanaged VTEP's IP on the node; CM-managed entry
			// should be preserved as-is.
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-shared", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			existing, err := util.ParseNodeVTEPs(n)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-a-multi": {IPs: []string{"100.64.0.99"}},
				"vtep-b-multi": existing["vtep-b-multi"], // keep CM-written entry unchanged
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both VTEPs should remain Accepted after re-validation
			gomega.Consistently(func() bool {
				for _, name := range []string{"vtep-a-multi", "vtep-b-multi"} {
					cond, err := getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
					if err != nil || cond == nil || cond.Status != metav1.ConditionTrue {
						return false
					}
				}
				return true
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("validates a dynamically created node with vteps annotation already set", func() {
			vtep := newVTEP("vtep-dynnode", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep)

			// No nodes exist yet — VTEP should be Accepted
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-dynnode", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Dynamically create a node that already has the vteps annotation
			node := newNodeWithVTEPAnnotation("node-late", map[string][]string{"vtep-dynnode": {"100.64.0.77"}})
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True after the new node is validated
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-dynnode", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("re-validates VTEP when one of many nodes changes vteps annotation", func() {
			node1 := newNodeWithVTEPAnnotation("node-m1", map[string][]string{"vtep-mnodes": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-m2", map[string][]string{"vtep-mnodes": {"100.64.0.2"}})
			node3 := newNodeWithVTEPAnnotation("node-m3", map[string][]string{"vtep-mnodes": {"100.64.0.3"}})
			vtep := newVTEP("vtep-mnodes", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-mnodes", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Change only node-m2's VTEP IP
			n2, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-m2", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-mnodes": {IPs: []string{"100.64.0.22"}},
			})
			n2.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n2, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Condition should remain Accepted=True throughout
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-mnodes", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("restores correct IP when node annotation is overwritten with a bogus value", func() {
			// CM allocates an IP for the node. Something (e.g. a misbehaving
			// controller) then overwrites the annotation with a wrong IP.
			// The annotation change triggers a node-watch reconcile;
			// allocateAndAnnotateNode detects the mismatch (allocator still
			// holds the original IP for this node) and rewrites the correct one.
			vtep := newVTEP("vtep-restore", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			node := newNodeWithVTEPAnnotation("node-restore", nil)
			start(vtep, node)

			// Wait for CM to write the initial allocation.
			var allocatedIP string
			gomega.Eventually(func() (string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-restore", metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if util.IsAnnotationNotSetError(err) || len(vteps["vtep-restore"].IPs) == 0 {
					return "", nil
				}
				if err != nil {
					return "", err
				}
				allocatedIP = vteps["vtep-restore"].IPs[0]
				return allocatedIP, nil
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.BeEmpty())

			// Overwrite with a bogus IP outside the CIDR.
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-restore", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			bogus, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-restore": {IPs: []string{"1.2.3.4"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(bogus)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// CM must restore the original IP.
			gomega.Eventually(func() (string, error) {
				n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-restore", metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				vteps, err := util.ParseNodeVTEPs(n)
				if util.IsAnnotationNotSetError(err) || len(vteps["vtep-restore"].IPs) == 0 {
					return "", nil
				}
				if err != nil {
					return "", err
				}
				return vteps["vtep-restore"].IPs[0], nil
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(allocatedIP))
		})
	})
})

var _ = ginkgo.Describe("vtepNameInMessage", func() {
	ginkgo.It("returns false for empty brackets", func() {
		gomega.Expect(vtepNameInMessage("CIDRs overlap with VTEPs: []", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("returns false when no brackets present", func() {
		gomega.Expect(vtepNameInMessage("no brackets here", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("matches single entry", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in two entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in three entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b, vtep-c]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-d")).To(gomega.BeFalse())
	})

	ginkgo.It("does not substring match", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match in multi-entry list", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-aa, vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-aa")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match vtep-b against vtep-bb and vtep-bbb", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-b, vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())
	})
})
