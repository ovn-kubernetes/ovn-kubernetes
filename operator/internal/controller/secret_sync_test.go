// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testWorkloadNamespace = "ovn-kubernetes"
	testSecretName        = "registry-credentials"
	testCRName            = "ovn-kubernetes"
)

func newSyncScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(corev1.AddToScheme(s)).To(Succeed())
	Expect(k8sv1alpha1.AddToScheme(s)).To(Succeed())
	return s
}

func newCRWithSecrets(secrets []string, propagate *bool) *k8sv1alpha1.OVNK8sOperator {
	refs := make([]corev1.LocalObjectReference, len(secrets))
	for i, name := range secrets {
		refs[i] = corev1.LocalObjectReference{Name: name}
	}
	return &k8sv1alpha1.OVNK8sOperator{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName},
		Spec: k8sv1alpha1.OVNK8sOperatorSpec{
			Global: k8sv1alpha1.GlobalConfig{
				Namespace:                 testWorkloadNamespace,
				ImagePullSecrets:          refs,
				PropagateImagePullSecrets: propagate,
			},
		},
	}
}

func sourceSecret(name string, secretType corev1.SecretType, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k8sv1alpha1.OperatorNamespace,
		},
		Type: secretType,
		Data: data,
	}
}

var _ = Describe("SyncImagePullSecrets", func() {
	var (
		ctx        context.Context
		reconciler *OVNK8sOperatorReconciler
		fakeCli    client.Client
	)

	dockerData := map[string][]byte{
		".dockerconfigjson": []byte(`{"auths":{"registry.example.com":{"auth":"Zm9vOmJhcg=="}}}`),
	}

	BeforeEach(func() {
		ctx = context.Background()
	})

	buildReconciler := func(objs ...client.Object) {
		fakeCli = fake.NewClientBuilder().
			WithScheme(newSyncScheme()).
			WithObjects(objs...).
			Build()
		reconciler = &OVNK8sOperatorReconciler{Client: fakeCli}
	}

	Context("happy path", func() {
		It("copies the source Secret into the workload namespace with labels + annotations", func() {
			src := sourceSecret(testSecretName, corev1.SecretTypeDockerConfigJson, dockerData)
			src.ResourceVersion = "42"
			cr := newCRWithSecrets([]string{testSecretName}, nil)
			buildReconciler(src)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			copied := &corev1.Secret{}
			Expect(fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: testSecretName,
			}, copied)).To(Succeed())

			Expect(copied.Type).To(Equal(corev1.SecretTypeDockerConfigJson))
			Expect(copied.Data).To(Equal(dockerData))
			Expect(copied.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))
			Expect(copied.Labels).To(HaveKeyWithValue(ownerLabel, testCRName))
			Expect(copied.Annotations).To(HaveKeyWithValue(sourceNamespaceAnno, k8sv1alpha1.OperatorNamespace))
		})
	})

	Context("source Secret missing", func() {
		It("returns a descriptive error", func() {
			cr := newCRWithSecrets([]string{"missing"}, nil)
			buildReconciler()

			err := reconciler.SyncImagePullSecrets(ctx, cr)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing"))
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Context("target Secret pre-exists with an incompatible type", func() {
		It("returns a descriptive error instead of an immutable-field API error", func() {
			src := sourceSecret(testSecretName, corev1.SecretTypeDockerConfigJson, dockerData)
			preExisting := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testSecretName,
					Namespace: testWorkloadNamespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{"x": []byte("y")},
			}
			cr := newCRWithSecrets([]string{testSecretName}, nil)
			buildReconciler(src, preExisting)

			err := reconciler.SyncImagePullSecrets(ctx, cr)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already exists with type"))
			Expect(err.Error()).To(ContainSubstring("delete or rename"))
		})
	})

	Context("source Secret has wrong type", func() {
		It("returns a descriptive error without copying", func() {
			src := sourceSecret("opaque", corev1.SecretTypeOpaque, map[string][]byte{"x": []byte("y")})
			cr := newCRWithSecrets([]string{"opaque"}, nil)
			buildReconciler(src)

			err := reconciler.SyncImagePullSecrets(ctx, cr)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expected"))

			copied := &corev1.Secret{}
			getErr := fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: "opaque",
			}, copied)
			Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "expected NotFound, got: %v", getErr)
		})
	})

	Context("existing unowned Secret in workload namespace", func() {
		It("overwrites it and claims ownership via labels", func() {
			src := sourceSecret(testSecretName, corev1.SecretTypeDockerConfigJson, dockerData)
			unowned := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testSecretName,
					Namespace: testWorkloadNamespace,
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{".dockerconfigjson": []byte("stale")},
			}
			cr := newCRWithSecrets([]string{testSecretName}, nil)
			buildReconciler(src, unowned)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			copied := &corev1.Secret{}
			Expect(fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: testSecretName,
			}, copied)).To(Succeed())

			Expect(copied.Data).To(Equal(dockerData))
			Expect(copied.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))
		})
	})

	Context("propagateImagePullSecrets=false", func() {
		It("skips propagation entirely; source lookup never happens", func() {
			// No source Secret exists. Would normally error out. With
			// propagate=false we don't even look it up.
			no := false
			cr := newCRWithSecrets([]string{"does-not-exist"}, &no)
			buildReconciler()

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())
		})
	})

	Context("pruning stale copies", func() {
		It("deletes a managed propagated Secret no longer in the CR list", func() {
			src := sourceSecret("current", corev1.SecretTypeDockerConfigJson, dockerData)
			stale := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale",
					Namespace: testWorkloadNamespace,
					Labels: map[string]string{
						managedByLabel: managedByValue,
						ownerLabel:     testCRName,
					},
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: dockerData,
			}
			cr := newCRWithSecrets([]string{"current"}, nil)
			buildReconciler(src, stale)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			got := &corev1.Secret{}
			err := fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: "stale",
			}, got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected stale Secret to be NotFound, got: %v", err)
		})

		It("leaves unowned Secrets with the same namespace alone", func() {
			src := sourceSecret("current", corev1.SecretTypeDockerConfigJson, dockerData)
			foreign := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foreign",
					Namespace: testWorkloadNamespace,
					// No managed-by label — admin-owned.
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: dockerData,
			}
			cr := newCRWithSecrets([]string{"current"}, nil)
			buildReconciler(src, foreign)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			got := &corev1.Secret{}
			Expect(fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: "foreign",
			}, got)).To(Succeed())
		})

		It("skips prune entirely when propagateImagePullSecrets=false", func() {
			no := false
			stale := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale",
					Namespace: testWorkloadNamespace,
					Labels: map[string]string{
						managedByLabel: managedByValue,
						ownerLabel:     testCRName,
					},
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: dockerData,
			}
			cr := newCRWithSecrets([]string{}, &no)
			buildReconciler(stale)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			got := &corev1.Secret{}
			Expect(fakeCli.Get(ctx, types.NamespacedName{
				Namespace: testWorkloadNamespace, Name: "stale",
			}, got)).To(Succeed())
		})
	})

	Context("multiple secrets", func() {
		It("copies all of them", func() {
			src1 := sourceSecret("a", corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockerconfigjson": []byte("one")})
			src2 := sourceSecret("b", corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockerconfigjson": []byte("two")})
			cr := newCRWithSecrets([]string{"a", "b"}, nil)
			buildReconciler(src1, src2)

			Expect(reconciler.SyncImagePullSecrets(ctx, cr)).To(Succeed())

			for _, name := range []string{"a", "b"} {
				got := &corev1.Secret{}
				Expect(fakeCli.Get(ctx, types.NamespacedName{
					Namespace: testWorkloadNamespace, Name: name,
				}, got)).To(Succeed())
			}
		})
	})
})
