package cni

import (
	"context"
	"fmt"
	"time"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func newPod(namespace, name string, annotations map[string]string) *corev1.Pod {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         types.UID(name),
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func newFakeClientSet(pod *corev1.Pod, podNamespaceLister *mocks.PodNamespaceLister) *ClientSet {
	podLister := mocks.PodLister{}
	podLister.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)
	podList := &corev1.PodList{}
	if pod != nil {
		podList.Items = []corev1.Pod{*pod}
	}
	fakeClient := fake.NewSimpleClientset(podList)

	return &ClientSet{
		kclient:   fakeClient,
		podLister: &podLister,
	}
}

var _ = Describe("CNI Utils tests", func() {
	var defaultPodAnnotation string
	var pod *corev1.Pod

	const (
		namespace = "some-ns"
		podName   = "some-pod"
	)

	BeforeEach(func() {
		pod = newPod(namespace, podName, nil)
		defaultPodAnnotation = `{
  "default":{"ip_addresses":["192.168.2.3/24"],
  "mac_address":"0a:58:c0:a8:02:03",
  "gateway_ips":["192.168.2.1"],
  "ip_address":"192.168.2.3/24",
  "gateway_ip":"192.168.2.1"}
}`
	})

	Context("isOvnReady", func() {
		It("Returns true if OVN pod network annotation exists", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: defaultPodAnnotation}
			pod.Annotations = podAnnot
			_, ready, _ := isOvnReady(pod, ovntypes.DefaultNetworkName)
			Expect(ready).To(BeTrue())
		})

		It("Returns false if OVN pod network annotation does not exist", func() {
			podAnnot := map[string]string{}
			pod.Annotations = podAnnot
			_, ready, _ := isOvnReady(pod, ovntypes.DefaultNetworkName)
			Expect(ready).To(BeFalse())
		})
	})

	Context("isDPUReady", func() {
		It("Returns true if dpu.connection-status is present and Status is Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:     defaultPodAnnotation,
				util.DPUConnectionStatusAnnot: `{"Status":"Ready"}`}
			pod.Annotations = podAnnot
			_, ready, err := isDPUReady(nil, ovntypes.DefaultNetworkName)(pod, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(ready).To(BeTrue())
		})

		It("Returns false if dpu.connection-status is present and Status is not Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:     defaultPodAnnotation,
				util.DPUConnectionStatusAnnot: `{"Status":"NotReady"}`}
			pod.Annotations = podAnnot
			_, ready, err := isDPUReady(nil, ovntypes.DefaultNetworkName)(pod, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("Returns false if dpu.connection-status Status is not present", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:     defaultPodAnnotation,
				util.DPUConnectionStatusAnnot: `{"Foo":"Bar"}`}
			pod.Annotations = podAnnot
			_, ready, err := isDPUReady(nil, ovntypes.DefaultNetworkName)(pod, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("Returns false if dpu.connection-status is not present", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: defaultPodAnnotation}
			pod.Annotations = podAnnot
			_, ready, err := isDPUReady(nil, ovntypes.DefaultNetworkName)(pod, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("Returns false if OVN pod annotation is empty", func() {
			podAnnot := map[string]string{}
			pod.Annotations = podAnnot
			_, ready, err := isDPUReady(nil, ovntypes.DefaultNetworkName)(pod, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(ready).To(BeFalse())
		})
	})

	Context("GetPodWithAnnotations", func() {
		var podNamespaceLister mocks.PodNamespaceLister

		BeforeEach(func() {
			podNamespaceLister = mocks.PodNamespaceLister{}
		})

		It("Returns Pod annotation if annotation condition is met", func() {
			podAnnot := map[string]string{"foo": "bar"}
			pod.Annotations = podAnnot
			ctx, cancelFunc := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelFunc()

			cond := func(pod *corev1.Pod, _ string) (*util.PodAnnotation, bool, error) {
				if _, ok := pod.Annotations["foo"]; ok {
					return nil, true, nil
				}
				return nil, false, nil
			}

			clientset := newFakeClientSet(pod, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			returnedPod, annot, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
			Expect(annot).To(Equal(podAnnot))
			Expect(string(returnedPod.UID)).To(Equal(string(pod.UID)))
		})

		It("Returns with Error if context is canceled", func() {
			ctx, cancelFunc := context.WithCancel(context.Background())

			cond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				return nil, false, nil
			}

			go func() {
				time.Sleep(20 * time.Millisecond)
				cancelFunc()
			}()

			clientset := newFakeClientSet(pod, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
		})

		It("Retries Until pod annotation condition is met", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancelFunc()

			calledOnce := false
			cond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				if calledOnce {
					return nil, true, nil
				}
				calledOnce = true
				return nil, false, nil
			}

			clientset := newFakeClientSet(pod, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if PodLister fails to get pod annotations", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				return nil, false, nil
			}

			clientset := newFakeClientSet(pod, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, fmt.Errorf("failed to list pods"))
			_, _, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list pods"))
		})

		It("Tries kube client if PodLister can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			calledOnce := false
			cond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				if calledOnce {
					return nil, true, nil
				}
				calledOnce = true
				return nil, false, nil
			}

			clientset := newFakeClientSet(pod, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, apierrors.NewNotFound(corev1.Resource("pod"), podName))
			_, _, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Returns an error if PodLister and kube client can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				return nil, false, nil
			}

			clientset := newFakeClientSet(nil, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, apierrors.NewNotFound(corev1.Resource("pod"), podName))
			_, _, _, err := GetPodWithAnnotations(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out waiting for pod after 1s"))
		})
	})

	Context("PodAnnotation2PodInfo", func() {
		podAnnot := map[string]string{
			util.OvnPodAnnotationName: `{
"default":{"ip_addresses":["192.168.2.3/24"],
"mac_address":"0a:58:c0:a8:02:03",
"gateway_ips":["192.168.2.1"],
"ip_address":"192.168.2.3/24",
"gateway_ip":"192.168.2.1"}}`,
		}
		podUID := "4d06bae8-9c38-41f6-945c-f92320e782e4"
		It("Creates PodInterfaceInfo in NodeModeFull mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeFull
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeFalse())
		})

		It("Creates PodInterfaceInfo in NodeModeDPUHost mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = true
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeTrue())
		})

		It("Creates PodInterfaceInfo without EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = false
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeFalse())
		})
	})
})
