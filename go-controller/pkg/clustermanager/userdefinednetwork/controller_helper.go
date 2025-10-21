package userdefinednetwork

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utiludn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/udn"
)

func (c *Controller) updateNAD(obj client.Object, namespace string) (*netv1.NetworkAttachmentDefinition, error) {
	if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
		// check if required UDN label is on namespace
		ns, err := c.namespaceInformer.Lister().Get(namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
		}

		if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; !exists {
			// No Required label set on namespace while trying to render NAD for primary network on this namespace
			return nil, util.NewInvalidPrimaryNetworkError(namespace)
		}
	}

	desiredNAD, err := c.renderNadFn(obj, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to generate NetworkAttachmentDefinition: %w", err)
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}
	nadCopy := nad.DeepCopy()

	if nadCopy == nil {
		// creating NAD in case no primary network exist should be atomic and synchronized with
		// any other thread that create NADs.
		c.createNetworkLock.Lock()
		defer c.createNetworkLock.Unlock()

		if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
			actualNads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
			if err != nil {
				return nil, fmt.Errorf("failed to list  NetworkAttachmentDefinition: %w", err)
			}
			// This is best-effort check no primary NAD exist before creating one,
			// noting prevent primary NAD from being created right after this check.
			if err := PrimaryNetAttachDefNotExist(actualNads); err != nil {
				return nil, err
			}
		}

		newNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), desiredNAD, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create NetworkAttachmentDefinition: %w", err)
		}
		klog.Infof("Created NetworkAttachmentDefinition [%s/%s]", newNAD.Namespace, newNAD.Name)

		return newNAD, nil
	}

	if !metav1.IsControlledBy(nadCopy, obj) {
		return nil, fmt.Errorf("foreign NetworkAttachmentDefinition with the desired name already exist [%s/%s]", nadCopy.Namespace, nadCopy.Name)
	}

	// NAD update path, need to merge internal (k8s.ovn.org) current annotations with desired
	for k, v := range nadCopy.Annotations {
		if strings.HasPrefix(k, types.OvnK8sPrefix) {
			if desiredNAD.Annotations == nil {
				desiredNAD.Annotations = make(map[string]string)
			}
			desiredNAD.Annotations[k] = v
		}
	}

	if reflect.DeepEqual(nadCopy.Spec.Config, desiredNAD.Spec.Config) && reflect.DeepEqual(nadCopy.ObjectMeta.Labels, desiredNAD.ObjectMeta.Labels) &&
		reflect.DeepEqual(desiredNAD.Annotations, nadCopy.Annotations) {
		return nadCopy, nil
	}

	nadCopy.Spec.Config = desiredNAD.Spec.Config
	nadCopy.ObjectMeta.Labels = desiredNAD.ObjectMeta.Labels
	nadCopy.Annotations = desiredNAD.Annotations
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadCopy.Namespace).Update(context.Background(), nadCopy, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update NetworkAttachmentDefinition: %w", err)
	}
	klog.Infof("Updated NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return updatedNAD, nil
}

func (c *Controller) deleteNAD(obj client.Object, namespace string) error {
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}
	nadCopy := nad.DeepCopy()

	if nadCopy == nil ||
		!metav1.IsControlledBy(nadCopy, obj) ||
		!controllerutil.ContainsFinalizer(nadCopy, template.FinalizerUserDefinedNetwork) {
		return nil
	}

	pods, err := c.podInformer.Lister().Pods(nadCopy.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods at target namespace %q: %w", nadCopy.Namespace, err)
	}
	// This is best-effort check no pod using the subject NAD,
	// nothing prevents pod creation right after this check.
	if err := NetAttachDefNotInUse(nadCopy, pods); err != nil {
		return &networkInUseError{err: err}
	}

	// Check if an egress firewall still exists in this namespace
	efs, err := c.efInformer.Lister().EgressFirewalls(nadCopy.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list egressFirewalls at target namespace %q: %w", nadCopy.Namespace, err)
	}
	if len(efs) > 0 {
		efNames := make([]string, 0, len(efs))
		for _, ef := range efs {
			efNames = append(efNames, ef.Name)
		}
		return &networkInUseError{err: fmt.Errorf("network still in use and has egress firewalls configured: %v", strings.Join(efNames, ","))}
	}
	controllerutil.RemoveFinalizer(nadCopy, template.FinalizerUserDefinedNetwork)
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadCopy.Namespace).Update(context.Background(), nadCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove NetworkAttachmentDefinition finalizer: %w", err)
	}
	klog.Infof("Finalizer removed from NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(updatedNAD.Namespace).Delete(context.Background(), updatedNAD.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.Infof("Deleted NetworkAttachmetDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return nil
}
