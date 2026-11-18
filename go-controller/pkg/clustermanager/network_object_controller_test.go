// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// recordingPodHandler records what a network object controller asks a
// network to do.
type recordingPodHandler struct {
	lock       sync.Mutex
	calls      []string
	releaseErr error
	// detached makes the network one the pods are not on
	detached bool
}

func (h *recordingPodHandler) record(call string) {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.calls = append(h.calls, call)
}

func (h *recordingPodHandler) recorded() []string {
	h.lock.Lock()
	defer h.lock.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *recordingPodHandler) set(detached bool, releaseErr error) {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.detached, h.releaseErr = detached, releaseErr
}

func (h *recordingPodHandler) attached(*corev1.Pod) bool {
	h.lock.Lock()
	defer h.lock.Unlock()
	return !h.detached
}

func (h *recordingPodHandler) handler() networkObjectHandler[corev1.Pod] {
	return networkObjectHandler[corev1.Pod]{
		attached: h.attached,
		held:     h.attached,
		reconcile: func(_ context.Context, key string) error {
			h.record("reconcile " + key)
			return nil
		},
		release: func(_ context.Context, pod *corev1.Pod) error {
			h.lock.Lock()
			err := h.releaseErr
			h.lock.Unlock()
			if err != nil {
				return err
			}
			h.record("release " + string(pod.UID))
			return nil
		},
	}
}

func (h *recordingPodHandler) sync() error {
	h.record("sync")
	return nil
}

func newTestPodController(t *testing.T, g *gomega.WithT, pods ...*corev1.Pod) (*networkObjectController[corev1.Pod], kubernetes.Interface) {
	t.Helper()
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	// the pod informer is only there with multiple networks
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	objs := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	client := util.GetOVNClientset(objs...).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(client)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	t.Cleanup(wf.Shutdown)
	return newAllocationControllers(wf).pods, client.KubeClient
}

func testPod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: "uid-" + types.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: "node1"},
	}
}

// The controller is not started: the test plays the part of its workers.
func TestNetworkObjectControllerReconcile(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	c, _ := newTestPodController(t, g, testPod("pod", "live"))
	net1, net2 := &recordingPodHandler{}, &recordingPodHandler{}
	g.Expect(c.Register("net1", net1.handler(), net1.sync)).To(gomega.Succeed())
	g.Expect(c.Register("net2", net2.handler(), net2.sync)).To(gomega.Succeed())

	t.Log("pods deleted under the same name are all released, before the pod is reconciled")
	c.onDelete(testPod("pod", "1"))
	c.onDelete(testPod("pod", "2"))
	g.Expect(c.reconcile(ctx, "ns/pod|net1")).To(gomega.Succeed())
	g.Expect(net1.recorded()).To(gomega.Equal([]string{"sync", "release uid-1", "release uid-2", "reconcile ns/pod"}))
	g.Expect(deletedObjectsOf(c, "net1", "ns/pod")).To(gomega.BeEmpty())

	t.Log("each network releases them on its own, and keeps the ones it fails to release")
	g.Expect(net2.recorded()).To(gomega.Equal([]string{"sync"}))
	net2.set(false, errors.New("release failed"))
	g.Expect(c.reconcile(ctx, "ns/pod|net2")).NotTo(gomega.Succeed())
	g.Expect(net2.recorded()).To(gomega.Equal([]string{"sync"}))
	g.Expect(deletedObjectsOf(c, "net2", "ns/pod")).To(gomega.HaveLen(2))
	net2.set(false, nil)
	g.Expect(c.reconcile(ctx, "ns/pod|net2")).To(gomega.Succeed())
	g.Expect(net2.recorded()).To(gomega.Equal([]string{"sync", "release uid-1", "release uid-2", "reconcile ns/pod"}))

	t.Log("a pod is neither reconciled nor kept once deleted for a network it is not on")
	net2.set(true, nil)
	c.onDelete(testPod("pod", "3"))
	g.Expect(deletedObjectsOf(c, "net1", "ns/pod")).To(gomega.HaveLen(1))
	g.Expect(deletedObjectsOf(c, "net2", "ns/pod")).To(gomega.BeEmpty())
	g.Expect(c.reconcile(ctx, "ns/pod|net1")).To(gomega.Succeed())
	g.Expect(c.reconcile(ctx, "ns/pod|net2")).To(gomega.Succeed())
	g.Expect(net1.recorded()[4:]).To(gomega.Equal([]string{"release uid-3", "reconcile ns/pod"}))
	g.Expect(net2.recorded()).To(gomega.HaveLen(4))

	t.Log("nothing is reconciled for a pod that is gone and has nothing to release")
	g.Expect(c.reconcile(ctx, "ns/missing|net1")).To(gomega.Succeed())
	g.Expect(net1.recorded()).To(gomega.HaveLen(6))

	t.Log("nothing is reconciled for a network that is deregistered or unknown")
	c.Deregister("net1")
	c.onDelete(testPod("pod", "4"))
	g.Expect(c.reconcile(ctx, "ns/pod|net1")).To(gomega.Succeed())
	g.Expect(c.reconcile(ctx, "ns/pod|unknown")).To(gomega.Succeed())
	g.Expect(net1.recorded()).To(gomega.HaveLen(6))
}

func TestNetworkObjectControllerEvents(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	pod := testPod("existing", "1")
	c, client := newTestPodController(t, g, pod)
	g.Expect(c.Start()).To(gomega.Succeed())
	t.Cleanup(c.Stop)

	t.Log("the pods that exist are reconciled for a network once its initial sync is done")
	on, off := &recordingPodHandler{}, &recordingPodHandler{detached: true}
	g.Expect(c.Register("on", on.handler(), on.sync)).To(gomega.Succeed())
	g.Expect(c.Register("off", off.handler(), off.sync)).To(gomega.Succeed())
	g.Eventually(on.recorded, 3*time.Second).Should(gomega.Equal([]string{"sync", "reconcile ns/existing"}))

	t.Log("an updated pod is reconciled again, a deleted one is released")
	updated := pod.DeepCopy()
	updated.Labels = map[string]string{"updated": "true"}
	_, err := client.CoreV1().Pods("ns").Update(ctx, updated, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(on.recorded, 3*time.Second).Should(gomega.HaveLen(3))
	g.Expect(client.CoreV1().Pods("ns").Delete(ctx, "existing", metav1.DeleteOptions{})).To(gomega.Succeed())
	g.Eventually(on.recorded, 3*time.Second).Should(gomega.ContainElement("release uid-1"))

	t.Log("nothing of it is done for the network the pod is not on")
	g.Consistently(off.recorded, 200*time.Millisecond).Should(gomega.Equal([]string{"sync"}))

	t.Log("a network that fails its initial sync is not registered")
	failing := &recordingPodHandler{}
	g.Expect(c.Register("failing", failing.handler(), func() error { return errors.New("sync failed") })).NotTo(gomega.Succeed())
	c.lock.RLock()
	g.Expect(c.networks).NotTo(gomega.HaveKey("failing"))
	c.lock.RUnlock()
}

// The networks all write to the same pod: it is reconciled for one network at
// a time.
func TestNetworkObjectControllerSerializesObject(t *testing.T) {
	g := gomega.NewWithT(t)
	// cancelled at the end, so a failure leaves no reconcile blocked
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, _ := newTestPodController(t, g, testPod("pod", "1"), testPod("other", "2"))

	const wait = 3 * time.Second
	entered, leave := make(chan string), make(chan struct{})
	blocking := func(netName string) networkObjectHandler[corev1.Pod] {
		yes := func(*corev1.Pod) bool { return true }
		return networkObjectHandler[corev1.Pod]{
			attached: yes,
			held:     yes,
			reconcile: func(ctx context.Context, _ string) error {
				select {
				case entered <- netName:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-leave:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			},
			release: func(context.Context, *corev1.Pod) error { return nil },
		}
	}
	noSync := func() error { return nil }
	g.Expect(c.Register("net1", blocking("net1"), noSync)).To(gomega.Succeed())
	g.Expect(c.Register("net2", blocking("net2"), noSync)).To(gomega.Succeed())

	done := make(chan error, 3)
	for _, queueKey := range []string{"ns/pod|net1", "ns/pod|net2", "ns/other|net2"} {
		go func() { done <- c.reconcile(ctx, queueKey) }()
	}

	t.Log("another pod is reconciled meanwhile, the same pod for another network is not")
	var first, second string
	g.Eventually(entered, wait).Should(gomega.Receive(&first))
	g.Eventually(entered, wait).Should(gomega.Receive(&second))
	g.Expect([]string{first, second}).To(gomega.ContainElement("net2"))
	g.Consistently(entered, 200*time.Millisecond).ShouldNot(gomega.Receive())

	t.Log("a network the pod is not on does not wait for it")
	detached := &recordingPodHandler{detached: true}
	g.Expect(c.Register("net3", detached.handler(), noSync)).To(gomega.Succeed())
	g.Expect(c.reconcile(ctx, "ns/pod|net3")).To(gomega.Succeed())
	g.Expect(detached.recorded()).To(gomega.BeEmpty())

	g.Eventually(leave, wait).Should(gomega.BeSent(struct{}{}))
	g.Eventually(leave, wait).Should(gomega.BeSent(struct{}{}))
	g.Eventually(entered, wait).Should(gomega.Receive())
	g.Eventually(leave, wait).Should(gomega.BeSent(struct{}{}))
	for range 3 {
		g.Eventually(done, wait).Should(gomega.Receive(gomega.Succeed()))
	}
}
