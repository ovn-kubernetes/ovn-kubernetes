package networkmanager

import (
	"context"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestPodTrackerControllerWithInformerAndDelete(t *testing.T) {
	primaryNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "primary",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: "layer3",
		Role:     "primary",
		MTU:      1400,
		NADName:  "testns/primary",
	}

	type callbackEvent struct {
		node   string
		nad    string
		active bool
	}

	tests := []struct {
		name         string
		nodeName     string
		podName      string
		namespace    string
		annotations  map[string]string
		networkIsDef bool
		createPod    bool
		deletePod    bool
		expectedNADs []string
		expectEvents []callbackEvent
	}{
		{
			name:         "pod with primary and secondary NADs triggers callback on add",
			nodeName:     "node1",
			podName:      "pod1",
			namespace:    "testns",
			annotations:  map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "sec1", "namespace": "testns"} ]`},
			networkIsDef: false,
			createPod:    true,
			expectedNADs: []string{"testns/primary", "testns/sec1"},
			expectEvents: []callbackEvent{
				{"node1", "testns/primary", true},
				{"node1", "testns/sec1", true},
			},
		},
		{
			name:         "pod deletion triggers callback on last pod removal",
			nodeName:     "node2",
			podName:      "pod2",
			namespace:    "testns",
			annotations:  map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "sec1", "namespace": "testns"} ]`},
			networkIsDef: false,
			createPod:    true,
			deletePod:    true,
			expectedNADs: nil,
			expectEvents: []callbackEvent{
				{"node2", "testns/primary", true}, // first pod add
				{"node2", "testns/sec1", true},
				{"node2", "testns/primary", false}, // last pod delete
				{"node2", "testns/sec1", false},
			},
		},
		{
			name:         "pod with default network and secondary NADs",
			nodeName:     "node3",
			podName:      "pod3",
			namespace:    "testns",
			annotations:  map[string]string{nadv1.NetworkAttachmentAnnot: `[ {"name": "secA", "namespace": "testns"}, {"name": "secB", "namespace": "testns"} ]`},
			networkIsDef: true, // default -> no primary UDN
			createPod:    true,
			expectedNADs: []string{"testns/secA", "testns/secB"},
			expectEvents: []callbackEvent{
				{"node3", "testns/secA", true},
				{"node3", "testns/secB", true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			// Track callback events
			var events []callbackEvent

			// Setup fake client + watch factory
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Fake network manager returning primary NAD
			fakeNM := &FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
			}
			if !tt.networkIsDef {
				nInfo, err := util.NewNetInfo(primaryNetwork)
				m := util.NewMutableNetInfo(nInfo)
				m.SetNADs(util.GetNADName(tt.namespace, nInfo.GetNetworkName()))
				g.Expect(err).ToNot(gomega.HaveOccurred())
				fakeNM.PrimaryNetworks[tt.namespace] = m
			}

			// Create PodTrackerController with dummy callback
			ptc := NewPodTrackerController("test-pod-tracker", wf, fakeNM, func(node, nad string, active bool) {
				events = append(events, callbackEvent{node, nad, active})
			})

			// Start informers
			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Start pod controller
			g.Expect(ptc.Start()).Should(gomega.Succeed())
			defer ptc.Stop()

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
					ptc.Lock()
					_, ok := ptc.reverse[key]
					ptc.Unlock()
					return ok
				}, "2s", "50ms").Should(gomega.BeTrue(), "pod add was not processed by controller")
			}

			if tt.deletePod {
				// Now delete; do this *after* we've observed the add
				err = fakeClient.KubeClient.CoreV1().Pods(tt.namespace).Delete(context.Background(), tt.podName, metav1.DeleteOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Wait for the controller to process the DELETE and remove the reverse entry
				g.Eventually(func() bool {
					ptc.Lock()
					_, ok := ptc.reverse[key]
					ptc.Unlock()
					return !ok
				}, "2s", "50ms").Should(gomega.BeTrue(), "pod delete was not processed by controller")
			}

			// Now assert final cache + events (allowing the controller a moment to deliver callbacks)
			g.Eventually(func(g gomega.Gomega) {
				ptc.Lock()
				defer ptc.Unlock()

				if tt.expectedNADs == nil {
					g.Expect(ptc.reverse).ToNot(gomega.HaveKey(key))
				} else {
					g.Expect(ptc.reverse).To(gomega.HaveKey(key))
					for _, nad := range tt.expectedNADs {
						g.Expect(ptc.cache[tt.nodeName]).To(gomega.HaveKey(nad))
						g.Expect(ptc.cache[tt.nodeName][nad]).To(gomega.HaveKey(key))
					}
				}

				// Verify callback events equal expected sequence
				g.Expect(events).To(gomega.Equal(tt.expectEvents))
			}, "2s", "50ms").Should(gomega.Succeed())
		})
	}
}

func TestPodTrackerControllerSyncAll(t *testing.T) {
	g := gomega.NewWithT(t)

	// Setup fake client + watch factory
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Fake network manager returning a primary NAD
	primaryNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "primary",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: "layer3",
		Role:     "primary",
		MTU:      1400,
		NADName:  "testns/primary",
	}
	nInfo, err := util.NewNetInfo(primaryNetwork)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	m := util.NewMutableNetInfo(nInfo)
	m.SetNADs(util.GetNADName("testns", nInfo.GetNetworkName()))

	fakeNM := &FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			"testns": m,
		},
	}

	// Track callback events
	var events []struct {
		node   string
		nad    string
		active bool
	}

	// Create PodTrackerController
	ptc := NewPodTrackerController("test-pod-tracker", wf, fakeNM, func(node, nad string, active bool) {
		events = append(events, struct {
			node   string
			nad    string
			active bool
		}{node, nad, active})
	})

	// Start informers
	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Start pod controller
	g.Expect(ptc.Start()).Should(gomega.Succeed())
	defer ptc.Stop()

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
		ptc.Lock()
		_, ok := ptc.reverse[key]
		ptc.Unlock()
		return ok
	}, "2s", "50ms").Should(gomega.BeTrue())

	// Manually clear controller state to simulate stale cache
	ptc.Lock()
	ptc.cache = make(map[string]map[string]map[string]struct{})
	ptc.reverse = make(map[string]nodeNAD)
	ptc.Unlock()

	// Call syncAll to rebuild state
	g.Expect(ptc.syncAll()).To(gomega.Succeed())

	// Verify that syncAll restored the pod->NAD mappings
	g.Eventually(func(g gomega.Gomega) {
		ptc.Lock()
		defer ptc.Unlock()
		g.Expect(ptc.reverse).To(gomega.HaveKey(key))
		g.Expect(ptc.cache["nodeX"]).To(gomega.HaveKey("testns/primary"))
		g.Expect(ptc.cache["nodeX"]).To(gomega.HaveKey("testns/sec1"))
	}, "2s", "50ms").Should(gomega.Succeed())

	// Verify callbacks included active=true rebuild events
	g.Expect(events).To(gomega.ContainElements(
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
