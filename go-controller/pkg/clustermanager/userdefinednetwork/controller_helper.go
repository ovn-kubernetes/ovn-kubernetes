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
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
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

	existingNAD, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}

	renderOpts, err := c.allocateEVPNVIDsIfNeeded(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate EVPN VIDs: %w", err)
	}

	desiredNAD, err := c.renderNadFn(obj, namespace, renderOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to generate NetworkAttachmentDefinition: %w", err)
	}

	nadCopy := existingNAD.DeepCopy()

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

	if nadCopy == nil {
		// NAD not found in lister cache; verify it's truly gone from the API
		// before treating as success. The lister cache may lag behind the API,
		// especially right after the NAD was created.
		_, apiErr := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(context.Background(), obj.GetName(), metav1.GetOptions{})
		if apiErr == nil {
			return fmt.Errorf("NetworkAttachmentDefinition %s/%s not found in cache but still exists in the API, retrying", namespace, obj.GetName())
		}
		if !apierrors.IsNotFound(apiErr) {
			return fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from API: %v", namespace, obj.GetName(), apiErr)
		}
		return nil
	}

	if !metav1.IsControlledBy(nadCopy, obj) ||
		!controllerutil.ContainsFinalizer(nadCopy, template.FinalizerUserDefinedNetwork) {
		return nil
	}

	pods, err := c.podInformer.Lister().Pods(nadCopy.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods at target namespace %q: %w", nadCopy.Namespace, err)
	}
	// This is best-effort check no pod using the subject NAD,
	// noting prevent a from being pod creation right after this check.
	if err := NetAttachDefNotInUse(nadCopy, pods); err != nil {
		return &networkInUseError{err: err}
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

// allocateEVPNVIDsIfNeeded checks if the object is an EVPN network and allocates VIDs if needed.
// Returns render options containing the allocated VIDs, or empty options for non-EVPN networks.
// Returns an error if EVPN transport is requested but the feature flag is disabled.
//
// This function relies on the idempotency of AllocateID: if a VID was already allocated for a key
// (either during recovery or a previous reconciliation), AllocateID returns the same VID.
// This means VIDs are stable across reconciliations without needing to parse the existing NAD.
func (c *Controller) allocateEVPNVIDsIfNeeded(obj client.Object) ([]template.RenderOption, error) {
	spec := template.GetSpec(obj)
	if spec.GetTransport() != userdefinednetworkv1.TransportOptionEVPN {
		return nil, nil
	}

	// EVPN transport is requested - ensure the feature is enabled.
	if !util.IsEVPNEnabled() {
		return nil, fmt.Errorf("EVPN transport requested but EVPN feature is not enabled")
	}

	evpnCfg := spec.GetEVPN()
	if evpnCfg == nil {
		return nil, nil
	}

	networkName := obj.GetName()
	var macVRFVID, ipVRFVID int

	// Allocate VID for MAC-VRF if present
	if evpnCfg.MACVRF != nil {
		vid, err := c.vidAllocator.AllocateID(macVRFKey(networkName))
		if err != nil {
			return nil, fmt.Errorf("failed to allocate VID for MAC-VRF: %w", err)
		}
		macVRFVID = vid
		klog.V(4).InfoS("Allocated VID for MAC-VRF", "network", networkName, "vid", vid)
	}

	// Allocate VID for IP-VRF if present
	if evpnCfg.IPVRF != nil {
		vid, err := c.vidAllocator.AllocateID(ipVRFKey(networkName))
		if err != nil {
			return nil, fmt.Errorf("failed to allocate VID for IP-VRF: %w", err)
		}
		ipVRFVID = vid
		klog.V(4).InfoS("Allocated VID for IP-VRF", "network", networkName, "vid", vid)
	}

	// Return render options with allocated VIDs.
	// Note: API validation ensures at least one of macVRF or ipVRF is specified,
	// so at least one VID will be allocated if we reach here.
	return []template.RenderOption{template.WithEVPNVIDs(macVRFVID, ipVRFVID)}, nil
}
