package networkmanager

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestPodTrackerControllerWithInformerAndDelete(t *testing.T) {
	type callbackEvent struct {
		node   string
		nad    string
		active bool
	}

	tests := []struct {
		name          string
		nodeName      string
		podName       string
		namespace     string
		annotations   map[string]string
		hasPrimaryUDN bool
		createPod     bool
		deletePod     bool
		expectedNADs  []string
		expectEvents  []callbackEvent
	}{
		{
			name:          "pod with primary and secondary NADs triggers callback on add",
			nodeName:      "node1",
			podName:       "pod1",
			namespace:     "testns",
			annotations:   map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "sec1", "namespace": "testns"} ]`},
			hasPrimaryUDN: true,
			createPod:     true,
			expectedNADs:  []string{"testns/primary", "testns/sec1"},
			expectEvents: []callbackEvent{
				{"node1", "testns/primary", true},
				{"node1", "testns/sec1", true},
			},
		},
		{
			name:          "pod with primary and secondary NADs triggers deletion callback on last pod removal",
			nodeName:      "node2",
			podName:       "pod2",
			namespace:     "testns",
			annotations:   map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "sec1", "namespace": "testns"} ]`},
			hasPrimaryUDN: true,
			createPod:     true,
			deletePod:     true,
			expectedNADs:  nil,
			expectEvents: []callbackEvent{
				{"node2", "testns/primary", true}, // first pod add
				{"node2", "testns/sec1", true},
				{"node2", "testns/primary", false}, // last pod delete
				{"node2", "testns/sec1", false},
			},
		},
		{
			name:          "pod with default network and secondary NADs",
			nodeName:      "node3",
			podName:       "pod3",
			namespace:     "testns",
			annotations:   map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "secA", "namespace": "testns"}, {"name": "secB", "namespace": "testns"} ]`},
			hasPrimaryUDN: false, // default -> no primary UDN
			createPod:     true,
			expectedNADs:  []string{"testns/secA", "testns/secB"},
			expectEvents: []callbackEvent{
				{"node3", "testns/secA", true},
				{"node3", "testns/secB", true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).NotTo(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

			// Track callback events
			var events []callbackEvent
			var eventsMu sync.Mutex

			// Setup fake client + watch factory
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Create PodTrackerController with dummy callback
			ptc := NewPodTrackerController("test-pod-tracker", wf, func(node, nad string, active bool) {
				eventsMu.Lock()
				events = append(events, callbackEvent{node, nad, active})
				eventsMu.Unlock()
			}, nil)

			// Start informers
			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Start pod controller
			g.Expect(ptc.Start()).Should(gomega.Succeed())
			defer ptc.Stop()

			nsLabel := map[string]string{}
			if tt.hasPrimaryUDN {
				nsLabel = map[string]string{ovntypes.RequiredUDNNamespaceLabel: ""}
			}
			// Create namespace
			_, err = fakeClient.KubeClient.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   tt.namespace,
					Labels: nsLabel,
				},
			}, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			if tt.hasPrimaryUDN {
				// Create Primary NAD
				netConf := &ovncnitypes.NetConf{
					NetConf:  cnitypes.NetConf{Name: "primary", Type: "ovn-k8s-cni-overlay"},
					Topology: "layer3",
					Role:     "primary",
					MTU:      1400,
					NADName:  "testns/primary",
				}
				bytes, err := json.Marshal(netConf)
				if err != nil {
					t.Fatalf("failed to marshal netconf: %v", err)
				}
				nad := &nadv1.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						UID:       types.UID(tt.namespace),
						Name:      "primary",
						Namespace: tt.namespace,
					},
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: string(bytes),
					},
				}
				if _, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(tt.namespace).
					Create(context.Background(), nad, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create NAD: %v", err)
				}
			}

			// Create node
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: tt.nodeName},
			}, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			key := tt.namespace + "/" + tt.podName

			if tt.createPod {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:        tt.podName,
						Namespace:   tt.namespace,
						Annotations: tt.annotations,
					},
					Spec: corev1.PodSpec{NodeName: tt.nodeName},
				}
				_, err = fakeClient.KubeClient.CoreV1().Pods(tt.namespace).Create(context.Background(), pod, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Wait for the controller to process the ADD and populate reverse entry
				g.Eventually(func() bool {
					ptc.cacheMutex.Lock()
					_, ok := ptc.podToNodeNAD[key]
					ptc.cacheMutex.Unlock()
					return ok
				}, "2s", "50ms").Should(gomega.BeTrue(), "pod add was not processed by controller")
			}

			if tt.deletePod {
				// Now delete; do this *after* we've observed the add
				err = fakeClient.KubeClient.CoreV1().Pods(tt.namespace).Delete(context.Background(), tt.podName, metav1.DeleteOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Wait for the controller to process the DELETE and remove the reverse entry
				g.Eventually(func() bool {
					ptc.cacheMutex.Lock()
					_, ok := ptc.podToNodeNAD[key]
					ptc.cacheMutex.Unlock()
					return !ok
				}, "2s", "50ms").Should(gomega.BeTrue(), "pod delete was not processed by controller")
			}

			// Now assert final cache + events (allowing the controller a moment to deliver callbacks)
			g.Eventually(func(g gomega.Gomega) {
				ptc.cacheMutex.Lock()
				defer ptc.cacheMutex.Unlock()

				if tt.expectedNADs == nil {
					g.Expect(ptc.podToNodeNAD).ToNot(gomega.HaveKey(key))
				} else {
					g.Expect(ptc.podToNodeNAD).To(gomega.HaveKey(key))
					for _, nad := range tt.expectedNADs {
						g.Expect(ptc.nodeNADToPodCache[tt.nodeName]).To(gomega.HaveKey(nad))
						g.Expect(ptc.nodeNADToPodCache[tt.nodeName][nad]).To(gomega.HaveKey(key))
					}
				}

				// Verify callback events equal expected sequence
				eventsMu.Lock()
				defer eventsMu.Unlock()
				g.Expect(events).To(gomega.ConsistOf(tt.expectEvents))
			}, "2s", "50ms").Should(gomega.Succeed())
		})
	}
}

func TestPodTrackerControllerSyncAll(t *testing.T) {
	g := gomega.NewWithT(t)
	err := config.PrepareTestConfig()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableInterconnect = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	// Setup fake client + watch factory
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Track callback events
	var events []struct {
		node   string
		nad    string
		active bool
	}
	var eventsMu sync.Mutex

	// Create PodTrackerController
	ptc := NewPodTrackerController("test-pod-tracker", wf, func(node, nad string, active bool) {
		eventsMu.Lock()
		events = append(events, struct {
			node   string
			nad    string
			active bool
		}{node, nad, active})
		eventsMu.Unlock()
	}, nil)

	// Start informers
	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Start pod controller
	g.Expect(ptc.Start()).Should(gomega.Succeed())
	defer ptc.Stop()

	// Create NAD
	namespace := "testns"
	netConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: "primary", Type: "ovn-k8s-cni-overlay"},
		Topology: "layer3",
		Role:     "primary",
		MTU:      1400,
		NADName:  "testns/primary",
	}
	bytes, err := json.Marshal(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(namespace),
			Name:      "primary",
			Namespace: namespace,
		},
		Spec: nadv1.NetworkAttachmentDefinitionSpec{
			Config: string(bytes),
		},
	}
	// Create namespace
	_, err = fakeClient.KubeClient.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: map[string]string{ovntypes.RequiredUDNNamespaceLabel: ""},
		},
	}, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Create NAD
	_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).
		Create(context.Background(), nad, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create a node
	_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "nodeX"},
	}, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create a pod with primary + secondary NADs
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "podX",
			Namespace: "testns",
			Annotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: `[ {"name": "sec1", "namespace": "testns"} ]`,
			},
		},
		Spec: corev1.PodSpec{NodeName: "nodeX"},
	}
	_, err = fakeClient.KubeClient.CoreV1().Pods("testns").Create(context.Background(), pod, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	key := "testns/podX"

	// Wait for add
	g.Eventually(func() bool {
		ptc.cacheMutex.Lock()
		_, ok := ptc.podToNodeNAD[key]
		ptc.cacheMutex.Unlock()
		return ok
	}, "2s", "50ms").Should(gomega.BeTrue())

	// Manually clear controller state to simulate stale cache
	ptc.cacheMutex.Lock()
	ptc.nodeNADToPodCache = make(map[string]map[string]map[string]struct{})
	ptc.podToNodeNAD = make(map[string]nodeNAD)
	ptc.cacheMutex.Unlock()

	// Call syncAll to rebuild state
	g.Expect(ptc.syncAll()).To(gomega.Succeed())

	// Verify that syncAll restored the pod->NAD mappings
	g.Eventually(func(g gomega.Gomega) {
		ptc.cacheMutex.Lock()
		defer ptc.cacheMutex.Unlock()
		g.Expect(ptc.podToNodeNAD).To(gomega.HaveKey(key))
		g.Expect(ptc.nodeNADToPodCache["nodeX"]).To(gomega.HaveKey("testns/primary"))
		g.Expect(ptc.nodeNADToPodCache["nodeX"]).To(gomega.HaveKey("testns/sec1"))
	}, "2s", "50ms").Should(gomega.Succeed())

	// Verify callbacks included active=true rebuild events
	g.Eventually(func() []struct {
		node   string
		nad    string
		active bool
	} {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		return append([]struct {
			node   string
			nad    string
			active bool
		}(nil), events...)
	}, "2s", "50ms").Should(gomega.ContainElements(
		struct {
			node, nad string
			active    bool
		}{"nodeX", "testns/primary", true},
		struct {
			node, nad string
			active    bool
		}{"nodeX", "testns/sec1", true},
	))
}
