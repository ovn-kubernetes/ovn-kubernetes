package kube

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Kube", func() {

	Describe("Taint node operations", func() {
		var kube Kube
		var existingNodeTaints []corev1.Taint
		var node *corev1.Node

		BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			kube = Kube{
				KClient: fakeClient,
			}
		})

		JustBeforeEach(func() {
			// create the node with the specified taints just before the tests
			newNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-node",
				},
				Spec: corev1.NodeSpec{
					Taints: existingNodeTaints,
				},
			}

			var err error
			node, err = kube.KClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node).NotTo(BeZero())
		})
	})

	Describe("SetAnnotationsOnPod", func() {
		var kube Kube

		BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			kube = Kube{
				KClient: fakeClient,
			}
		})

		Context("With a pod having annotations", func() {
			var (
				pod                 *corev1.Pod
				existingAnnotations map[string]string
			)

			BeforeEach(func() {
				existingAnnotations = map[string]string{"foo": "foofoo", "bar": "barbar", "baz": "bazbaz"}

				// create the pod
				newPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:   "default",
						Name:        "my-pod",
						Annotations: existingAnnotations,
					},
				}

				var err error
				pod, err = kube.KClient.CoreV1().Pods(newPod.Namespace).Create(context.TODO(), newPod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(pod).NotTo(BeZero())
			})

			Context("With adding additional annotations", func() {
				var newAnnotations map[string]interface{}

				BeforeEach(func() {
					newAnnotations = map[string]interface{}{"foobar": "foobarfoobar", "foobarbaz": "foobarbazfoobarbaz"}

					// update the annotations
					err := kube.SetAnnotationsOnPod(pod.Namespace, pod.Name, newAnnotations)
					Expect(err).ToNot(HaveOccurred())

					// load the updated pod
					pod, err = kube.KClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(pod).ToNot(BeZero())
				})

				It("Should add the new annotations", func() {
					for newAnnotationKey := range newAnnotations {
						_, found := pod.Annotations[newAnnotationKey]
						Expect(found).To(BeTrue())
					}
				})

				It("Should keep the existing annotations", func() {
					for existingAnnotationKey := range existingAnnotations {
						_, found := pod.Annotations[existingAnnotationKey]
						Expect(found).To(BeTrue())
					}
				})
			})
		})
	})

	Describe("PatchPodStatusAnnotations", func() {
		var kube Kube

		BeforeEach(func() {
			kube = Kube{
				KClient: fake.NewSimpleClientset(),
			}
		})

		It("adds annotations without dropping existing ones", func() {
			_, err := kube.KClient.CoreV1().Pods("default").Create(context.TODO(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "default",
					Name:        "my-pod",
					Annotations: map[string]string{"existing": "value"},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			pod, err := kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldPod := pod.DeepCopy()
			newPod := pod.DeepCopy()
			newPod.Annotations["added"] = "new-value"

			err = kube.PatchPodStatusAnnotations(oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).To(HaveKeyWithValue("existing", "value"))
			Expect(pod.Annotations).To(HaveKeyWithValue("added", "new-value"))
		})

		It("can create and remove annotations", func() {
			_, err := kube.KClient.CoreV1().Pods("default").Create(context.TODO(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pod",
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			pod, err := kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldPod := pod.DeepCopy()
			newPod := pod.DeepCopy()
			newPod.Annotations = map[string]string{"ovn": "value"}

			err = kube.PatchPodStatusAnnotations(oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).To(HaveKeyWithValue("ovn", "value"))

			oldPod = pod.DeepCopy()
			newPod = pod.DeepCopy()
			delete(newPod.Annotations, "ovn")

			err = kube.PatchPodStatusAnnotations(oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).ToNot(HaveKey("ovn"))
		})

		It("adds a resourceVersion guard when creating a missing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Pod{}, nil
			})

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					ResourceVersion: "7",
				},
			}
			newPod := oldPod.DeepCopy()
			newPod.Annotations = map[string]string{"ovn": "value"}

			err := kube.PatchPodStatusAnnotations(oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(3))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/resourceVersion"))
			Expect(patchOps[0].Value).To(Equal("7"))
			Expect(patchOps[1].Op).To(Equal("add"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations"))
			Expect(patchOps[2].Op).To(Equal("add"))
			Expect(patchOps[2].Path).To(Equal("/metadata/annotations/ovn"))
		})

		It("uses a per-key guard when updating an existing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Pod{}, nil
			})

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					ResourceVersion: "9",
					Annotations:     map[string]string{"ovn": "old"},
				},
			}
			newPod := oldPod.DeepCopy()
			newPod.Annotations["ovn"] = "new"

			err := kube.PatchPodStatusAnnotations(oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(2))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[0].Value).To(Equal("old"))
			Expect(patchOps[1].Op).To(Equal("replace"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[1].Value).To(Equal("new"))
		})
	})
})
