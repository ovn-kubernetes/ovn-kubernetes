// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/status"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "ovnk8s-operator"
	ownerLabel     = "ovnk8s-operator.ovn.org/ovnk8soperator"

	// Debug-only annotations on propagated Secrets — record where the
	// copy came from and the source's resource version at copy time.
	// Not consumed by reconcile logic; useful when triaging stale copies.
	sourceNamespaceAnno = "ovnk8s-operator.ovn.org/source-namespace"
	sourceRVAnno        = "ovnk8s-operator.ovn.org/source-resource-version"
)

// SyncImagePullSecrets copies each dockerconfigjson Secret referenced by
// spec.global.ImagePullSecrets from k8sv1alpha1.OperatorNamespace into the workload
// namespace (spec.global.Namespace). When PropagateImagePullSecrets is
// explicitly false, propagation is skipped entirely — the admin owns
// Secret placement in the workload namespace.
//
// Existing Secrets in the workload namespace with our managed-by label
// are updated in place; unowned Secrets with the same name are claimed
// and overwritten. Missing sources and wrong-typed sources are errors —
// retries on subsequent reconciles resolve transient races.
func (r *OVNK8sOperatorReconciler) SyncImagePullSecrets(ctx context.Context, cr *k8sv1alpha1.OVNK8sOperator) error {
	logger := log.FromContext(ctx)

	if cr.Spec.Global.PropagateImagePullSecrets != nil && !*cr.Spec.Global.PropagateImagePullSecrets {
		logger.V(1).Info("Skipping image-pull secret propagation (propagateImagePullSecrets=false)")
		return nil
	}

	for _, ref := range cr.Spec.Global.ImagePullSecrets {
		if err := r.copyImagePullSecret(ctx, cr, ref.Name); err != nil {
			return fmt.Errorf("propagate image-pull secret %q: %w", ref.Name, err)
		}
	}

	if err := r.pruneStaleImagePullSecrets(ctx, cr); err != nil {
		return fmt.Errorf("prune stale image-pull secrets: %w", err)
	}
	return nil
}

// pruneStaleImagePullSecrets deletes dockerconfigjson Secrets in the
// OVN-Kubernetes namespace that were previously propagated by this CR
// but no longer appear in spec.global.ImagePullSecrets. Scoped to
// Secrets carrying our managed-by + owner labels — admin-created or
// other-kind Secrets are left alone.
//
// Not called when propagateImagePullSecrets=false: when the admin takes
// ownership of Secret placement, we leave any residue alone.
func (r *OVNK8sOperatorReconciler) pruneStaleImagePullSecrets(ctx context.Context, cr *k8sv1alpha1.OVNK8sOperator) error {
	logger := log.FromContext(ctx)

	keep := make(map[string]struct{}, len(cr.Spec.Global.ImagePullSecrets))
	for _, ref := range cr.Spec.Global.ImagePullSecrets {
		keep[ref.Name] = struct{}{}
	}

	list := &corev1.SecretList{}
	if err := r.List(ctx, list,
		client.InNamespace(cr.Spec.Global.Namespace),
		client.MatchingLabels{
			managedByLabel: managedByValue,
			ownerLabel:     cr.GetName(),
		},
	); err != nil {
		return fmt.Errorf("list managed Secrets in %q: %w", cr.Spec.Global.Namespace, err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		if item.Type != corev1.SecretTypeDockerConfigJson {
			continue
		}
		if _, desired := keep[item.Name]; desired {
			continue
		}
		logger.Info("Pruning stale image-pull Secret",
			"namespace", item.Namespace, "name", item.Name)
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale Secret %s/%s: %w", item.Namespace, item.Name, err)
		}
	}
	return nil
}

// imagePullSecretsSyncedStatus reports a successful image-pull secret
// sync pass. Picks the reason/message based on whether propagation is
// enabled, the number of Secrets handled, etc.
func (r *OVNK8sOperatorReconciler) imagePullSecretsSyncedStatus(ctx context.Context, cr *k8sv1alpha1.OVNK8sOperator) error {
	reason := status.ReasonImagePullSecretsSynced
	message := fmt.Sprintf("Propagated %d image-pull secret(s) to %q",
		len(cr.Spec.Global.ImagePullSecrets), cr.Spec.Global.Namespace)

	if cr.Spec.Global.PropagateImagePullSecrets != nil && !*cr.Spec.Global.PropagateImagePullSecrets {
		reason = status.ReasonImagePullSecretsSkipped
		message = "Propagation disabled (spec.global.propagateImagePullSecrets=false); admin owns Secret placement"
	}

	return r.StatusManager.UpdateImagePullSecretsSynced(ctx, cr, reason, message)
}

func (r *OVNK8sOperatorReconciler) copyImagePullSecret(ctx context.Context, cr *k8sv1alpha1.OVNK8sOperator, name string) error {
	logger := log.FromContext(ctx).WithValues("secret", name)
	src := &corev1.Secret{}
	srcKey := types.NamespacedName{Namespace: k8sv1alpha1.OperatorNamespace, Name: name}
	if err := r.Get(ctx, srcKey, src); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("source Secret %s/%s not found (create it in the operator namespace, or set propagateImagePullSecrets=false)", k8sv1alpha1.OperatorNamespace, name)
		}
		return fmt.Errorf("get source Secret %s/%s: %w", k8sv1alpha1.OperatorNamespace, name, err)
	}

	if src.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("source Secret %s/%s has type %q; expected %q", k8sv1alpha1.OperatorNamespace, name, src.Type, corev1.SecretTypeDockerConfigJson)
	}

	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Spec.Global.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
		// An existing Secret of a different type can't be converted — Secret.Type
		// is immutable. Fail with a clear message instead of the generic apiserver
		// "field is immutable" error.
		if target.ResourceVersion != "" && target.Type != "" && target.Type != corev1.SecretTypeDockerConfigJson {
			return fmt.Errorf("target Secret %s/%s already exists with type %q; delete or rename it so the operator can create a %q copy",
				target.Namespace, target.Name, target.Type, corev1.SecretTypeDockerConfigJson)
		}

		// Skip the write if source/target already match on data + ownership.
		// Keeps the API server quiet during no-op reconciles.
		if target.Type == corev1.SecretTypeDockerConfigJson &&
			secretDataEqual(target.Data, src.Data) &&
			target.Labels[managedByLabel] == managedByValue &&
			target.Labels[ownerLabel] == cr.GetName() {
			return nil
		}

		if target.Labels == nil {
			target.Labels = map[string]string{}
		}
		target.Labels[managedByLabel] = managedByValue
		target.Labels[ownerLabel] = cr.GetName()

		if target.Annotations == nil {
			target.Annotations = map[string]string{}
		}
		target.Annotations[sourceNamespaceAnno] = k8sv1alpha1.OperatorNamespace
		target.Annotations[sourceRVAnno] = src.ResourceVersion

		target.Type = corev1.SecretTypeDockerConfigJson
		target.Data = copySecretData(src.Data)
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert target Secret %s/%s: %w", cr.Spec.Global.Namespace, name, err)
	}
	if op != controllerutil.OperationResultNone {
		logger.V(1).Info("Synced image-pull Secret", "operation", op)
	}
	return nil
}

func copySecretData(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}
