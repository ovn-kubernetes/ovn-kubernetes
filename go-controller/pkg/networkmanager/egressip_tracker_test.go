package networkmanager

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestEgressIPTrackerControllerWithInformer(t *testing.T) {
	type callbackEvent struct {
		node   string
		nad    string
		active bool
	}

	tests := []struct {
		name          string
		nodeName      string
		namespace     string
		eipName       string
		labelKey      string
		labelValue    string
		newNodeName   string
		updateFn      func(fakeClient *util.OVNKubeControllerClientset, g *gomega.WithT, tracker *EgressIPTrackerController)
		expectAdds    []callbackEvent
		expectUpdates []callbackEvent
	}{
		{
			name:       "basic EIP add/delete",
			nodeName:   "node1",
			namespace:  "ns1",
			eipName:    "eip1",
			labelKey:   "team",
			labelValue: "a",
			updateFn: func(fc *util.OVNKubeControllerClientset, g *gomega.WithT, _ *EgressIPTrackerController) {
				err := fc.EgressIPClient.K8sV1().EgressIPs().Delete(context.Background(), "eip1", metav1.DeleteOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			},
			expectAdds: []callbackEvent{
				{"node1", "ns1/primary", true},
			},
			expectUpdates: []callbackEvent{
				{"node1", "ns1/primary", false},
			},
		},
		{
			name:       "namespace label change stops EIP",
			nodeName:   "node2",
			namespace:  "ns2",
			eipName:    "eip2",
			labelKey:   "team",
			labelValue: "b",
			updateFn: func(fc *util.OVNKubeControllerClientset, g *gomega.WithT, _ *EgressIPTrackerController) {
				ns, err := fc.KubeClient.CoreV1().Namespaces().Get(context.Background(), "ns2", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				ns.Labels = map[string]string{"team": "x"}
				_, err = fc.KubeClient.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			},
			expectAdds: []callbackEvent{
				{"node2", "ns2/primary", true},
			},
			expectUpdates: []callbackEvent{
				{"node2", "ns2/primary", false},
			},
		},
		{
			name:        "EIP node reassignment",
			nodeName:    "node3",
			newNodeName: "node4",
			namespace:   "ns3",
			eipName:     "eip3",
			labelKey:    "env",
			labelValue:  "prod",
			updateFn: func(fc *util.OVNKubeControllerClientset, g *gomega.WithT, _ *EgressIPTrackerController) {
				eip, err := fc.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), "eip3", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				eip.Status.Items = []egressipv1.EgressIPStatusItem{
					{Node: "node4", EgressIP: "3.3.3.3"},
				}
				_, err = fc.EgressIPClient.K8sV1().EgressIPs().Update(context.Background(), eip, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())

			},
			expectAdds: []callbackEvent{
				{"node3", "ns3/primary", true},
			},
			expectUpdates: []callbackEvent{
				{"node3", "ns3/primary", false},
				{"node4", "ns3/primary", true}, // new node add
			},
		},
		{
			name:       "primary UDN change on namespace",
			nodeName:   "node5",
			namespace:  "ns5",
			eipName:    "eip5",
			labelKey:   "team",
			labelValue: "blue",
			updateFn: func(fc *util.OVNKubeControllerClientset, g *gomega.WithT, tracker *EgressIPTrackerController) {
				// Simulate primary network change by replacing the NetInfo
				netConf := &ovncnitypes.NetConf{
					NetConf:  cnitypes.NetConf{Name: "new-primary", Type: "ovn-k8s-cni-overlay"},
					Topology: "layer3",
					Role:     "primary",
					MTU:      1400,
					NADName:  "ns5/new-primary",
				}
				bytes, err := json.Marshal(netConf)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				err = fc.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns5").
					Delete(context.Background(), "primary", metav1.DeleteOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				// Trigger the NAD create event
				_, err = fc.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns5").
					Create(context.Background(), &nadv1.NetworkAttachmentDefinition{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "new-primary",
							Namespace: "ns5",
							Labels:    map[string]string{"role": "primary"},
						},
						Spec: nadv1.NetworkAttachmentDefinitionSpec{
							Config: string(bytes),
						},
					}, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())

				// Simulate NAD controller notification
				g.Eventually(func() (string, error) {
					return tracker.primaryNADForNamespace("ns5")
				}, 2*time.Second, 100*time.Millisecond).Should(gomega.Equal(util.GetNADName("ns5", "new-primary")))
				tracker.nadReconciler.Reconcile(util.GetNADName("ns5", "new-primary"))
			},
			expectAdds: []callbackEvent{
				{"node5", "ns5/primary", true},
			},
			expectUpdates: []callbackEvent{
				{"node5", "ns5/primary", false},
				{"node5", "ns5/new-primary", true},
			},
		},
		{
			name:       "multiple EgressIPs select same namespace triggers single callback",
			nodeName:   "node7",
			namespace:  "ns7",
			eipName:    "eip7a", // the first EgressIP
			labelKey:   "team",
			labelValue: "shared",
			updateFn: func(fc *util.OVNKubeControllerClientset, g *gomega.WithT, _ *EgressIPTrackerController) {
				// Create a second EgressIP that selects the same namespace,
				// has a different EgressIP name/address, but same node.
				_, err := fc.EgressIPClient.K8sV1().EgressIPs().Create(
					context.Background(),
					&egressipv1.EgressIP{
						ObjectMeta: metav1.ObjectMeta{Name: "eip7b"},
						Spec: egressipv1.EgressIPSpec{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"team": "shared"},
							},
						},
						Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{
							{Node: "node7", EgressIP: "7.7.7.7"},
						}},
					},
					metav1.CreateOptions{},
				)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			},
			expectAdds: []callbackEvent{
				// Only one initial callback is expected even if a second EgressIP
				// later selects the same namespace and NAD on the same node.
				{"node7", "ns7/primary", true},
			},
			expectUpdates: []callbackEvent{
				// No removals or additional adds, because the node+nad stays active.
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).NotTo(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			var got []callbackEvent
			var gotMu sync.Mutex

			// Fake client and watch factory
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tracker := NewEgressIPTrackerController("test", wf, func(node, nad string, active bool) {
				gotMu.Lock()
				got = append(got, callbackEvent{node, nad, active})
				gotMu.Unlock()
			}, nil)

			g.Expect(wf.Start()).To(gomega.Succeed())
			defer wf.Shutdown()
			g.Expect(tracker.Start()).To(gomega.Succeed())
			defer tracker.Stop()

			// Create NAD
			netConf := &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "primary", Type: "ovn-k8s-cni-overlay"},
				Topology: "layer3",
				Role:     "primary",
				MTU:      1400,
				NADName:  tt.namespace + "/primary",
			}
			bytes, err := json.Marshal(netConf)
			g.Expect(err).NotTo(gomega.HaveOccurred())
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
			_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(tt.namespace).
				Create(context.Background(), nad, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			// Create nodes
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(),
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tt.nodeName}}, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tt.newNodeName != "" {
				_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(),
					&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tt.newNodeName}}, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}

			// Create namespace matching selector
			_, err = fakeClient.KubeClient.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.namespace,
					Labels: map[string]string{
						tt.labelKey:                        tt.labelValue,
						ovntypes.RequiredUDNNamespaceLabel: "",
					},
				},
			}, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			// Create EgressIP selecting the namespace
			_, err = fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{Name: tt.eipName},
				Spec: egressipv1.EgressIPSpec{
					NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{tt.labelKey: tt.labelValue}},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{
					{Node: tt.nodeName, EgressIP: "1.1.1.1"},
				}},
			}, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			// Mirror production ordering: NAD controller notifies registered reconcilers
			// after the primary NAD is observed, so namespace reconcile isn't dropped due
			// to a transient "primary not found" window in informer caches.
			primaryNADKey := util.GetNADName(tt.namespace, "primary")
			g.Eventually(func() (string, error) {
				return tracker.primaryNADForNamespace(tt.namespace)
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.Equal(primaryNADKey))
			tracker.NADReconciler().Reconcile(primaryNADKey)

			// Expect add events
			g.Eventually(func() []callbackEvent {
				gotMu.Lock()
				gotCopy := make([]callbackEvent, len(got))
				copy(gotCopy, got)
				gotMu.Unlock()
				return gotCopy
			}, 3*time.Second, 100*time.Millisecond).Should(gomega.ConsistOf(tt.expectAdds))

			g.Eventually(func(g gomega.Gomega) {
				tracker.cacheMutex.Lock()
				defer tracker.cacheMutex.Unlock()
				for _, ev := range tt.expectAdds {
					g.Expect(tracker.cache[ev.node]).To(gomega.HaveKey(ev.nad))
				}
			}, 3*time.Second, 100*time.Millisecond).Should(gomega.Succeed())

			// Apply the update (delete EIP, change label, or reassign node)
			if tt.updateFn != nil {
				tt.updateFn(fakeClient, g, tracker)
			}

			expectedFinal := append(tt.expectAdds, tt.expectUpdates...)

			// Expect removal or new node events
			g.Eventually(func() []callbackEvent {
				gotMu.Lock()
				gotCopy := make([]callbackEvent, len(got))
				copy(gotCopy, got)
				gotMu.Unlock()
				return gotCopy
			}, 3*time.Second, 100*time.Millisecond).Should(gomega.ConsistOf(expectedFinal))

			g.Consistently(func() []callbackEvent {
				gotMu.Lock()
				gotCopy := make([]callbackEvent, len(got))
				copy(gotCopy, got)
				gotMu.Unlock()
				return gotCopy
			}, 500*time.Millisecond, 100*time.Millisecond).Should(gomega.ConsistOf(expectedFinal))

			g.Eventually(func(g gomega.Gomega) {
				tracker.cacheMutex.Lock()
				defer tracker.cacheMutex.Unlock()
				for _, ev := range tt.expectUpdates {
					if !ev.active { // removal
						g.Expect(tracker.cache[ev.node]).NotTo(gomega.HaveKey(ev.nad))
					}
				}
			}, 3*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
		})
	}
}
