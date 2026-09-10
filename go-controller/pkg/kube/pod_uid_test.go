// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestPatchPodStatusAnnotationsUIDMismatch(t *testing.T) {
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "namespace", Name: "pod", UID: "old-uid",
		Annotations: map[string]string{"annotation": "old-value"},
	}}
	newPod := oldPod.DeepCopy()
	newPod.Annotations["annotation"] = "new-value"
	podResource := schema.GroupResource{Resource: "pods"}
	invalidErr := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, oldPod.Name,
		field.ErrorList{field.Invalid(field.NewPath("metadata"), nil, "test failed")})
	conflictErr := apierrors.NewConflict(podResource, oldPod.Name, errors.New("conflict"))
	forbiddenErr := apierrors.NewForbidden(podResource, oldPod.Name, errors.New("forbidden"))

	for _, tc := range []struct {
		name         string
		patchErr     error
		getErr       error
		replaced     bool
		wantMismatch bool
		wantNotFound bool
	}{
		{name: "invalid patch on replacement", patchErr: invalidErr, replaced: true, wantMismatch: true},
		{name: "conflict on replacement", patchErr: conflictErr, replaced: true, wantMismatch: true},
		{name: "same UID invalid", patchErr: invalidErr},
		{name: "same UID conflict", patchErr: conflictErr},
		{name: "unconfirmed replacement", patchErr: invalidErr, getErr: forbiddenErr, replaced: true},
		{name: "pod deleted after failed patch", patchErr: invalidErr,
			getErr: apierrors.NewNotFound(podResource, oldPod.Name), wantNotFound: true},
		{name: "unrelated patch error", patchErr: forbiddenErr, replaced: true},
		{name: "successful patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentPod := oldPod.DeepCopy()
			if tc.replaced {
				currentPod.UID = "new-uid"
			}
			client := fake.NewSimpleClientset(currentPod)
			getCalls := 0
			client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
				getCalls++
				return true, currentPod.DeepCopy(), tc.getErr
			})
			if tc.patchErr != nil {
				client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.patchErr
				})
			}
			kube := &Kube{KClient: client}
			err := kube.PatchPodStatusAnnotations(oldPod, newPod)
			switch {
			case tc.wantMismatch:
				var mismatch *ovntypes.PodUIDMismatchError
				require.ErrorAs(t, err, &mismatch)
				require.Equal(t, oldPod.UID, mismatch.ExpectedUID)
				require.Equal(t, currentPod.UID, mismatch.ActualUID)
				require.Equal(t, oldPod.Namespace, mismatch.Namespace)
				require.Equal(t, oldPod.Name, mismatch.Name)
			case tc.wantNotFound:
				require.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
			default:
				require.Equal(t, tc.patchErr, err)
			}
			if apierrors.IsInvalid(tc.patchErr) || apierrors.IsConflict(tc.patchErr) {
				require.Equal(t, 1, getCalls)
			} else {
				require.Zero(t, getCalls, "only precondition failures need an identity lookup")
			}
		})
	}
}

func TestPatchPodStatusAnnotationsUIDGuard(t *testing.T) {
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "namespace", Name: "pod", UID: "old-uid", ResourceVersion: "1",
		Annotations: map[string]string{"annotation": "old-value"},
	}}
	for _, addKey := range []bool{false, true} {
		name := "existing annotation"
		if addKey {
			name = "new annotation"
		}
		t.Run(name, func(t *testing.T) {
			replacement := oldPod.DeepCopy()
			replacement.UID = "new-uid"
			replacement.ResourceVersion = "2"
			client := fake.NewSimpleClientset(replacement)
			// The fake tracker applies JSON Patch tests but returns their raw
			// errors. The real apiserver reports these failures as Invalid.
			client.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				handled, obj, err := ktesting.ObjectReaction(client.Tracker())(action)
				require.Error(t, err, "the stale patch must be rejected")
				return handled, obj, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, oldPod.Name,
					field.ErrorList{field.Invalid(field.NewPath("metadata"), nil, err.Error())})
			})
			newPod := oldPod.DeepCopy()
			key := "annotation"
			if addKey {
				key = "new-annotation"
			}
			newPod.Annotations[key] = "new-value"
			kube := &Kube{KClient: client}
			err := kube.PatchPodStatusAnnotations(oldPod, newPod)
			require.True(t, ovntypes.IsPodUIDMismatchError(err), "expected UID mismatch, got %v", err)
			got, err := client.CoreV1().Pods(oldPod.Namespace).Get(context.Background(), oldPod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, replacement, got)
		})
	}
}
