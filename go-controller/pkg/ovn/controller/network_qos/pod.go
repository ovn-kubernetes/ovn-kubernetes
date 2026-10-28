// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	if oldObj == nil {
		return util.PodScheduled(newObj) && !util.PodCompleted(newObj)
	}
	if newObj == nil {
		return true
	}
	if !oldObj.DeletionTimestamp.Equal(newObj.DeletionTimestamp) ||
		oldObj.Spec.NodeName != newObj.Spec.NodeName ||
		oldObj.Spec.HostNetwork != newObj.Spec.HostNetwork ||
		util.PodCompleted(oldObj) != util.PodCompleted(newObj) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(oldObj.Labels, newObj.Labels) {
		return true
	}
	oldAnnotations, newAnnotations := oldObj.Annotations, newObj.Annotations
	if oldAnnotations == nil {
		oldAnnotations = map[string]string{}
	}
	if newAnnotations == nil {
		newAnnotations = map[string]string{}
	}
	return oldAnnotations[nettypes.NetworkAttachmentAnnot] != newAnnotations[nettypes.NetworkAttachmentAnnot] ||
		oldAnnotations[types.OvnPodAnnotationName] != newAnnotations[types.OvnPodAnnotationName]
}

func (c *Controller) reconcilePod(key string) error {
	namespace, podName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Dropping malformed pod key %q: %v", key, err)
		return nil
	}
	nqoses, err := c.nqosLister.NetworkQoSes(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list NetworkQoS in namespace %s: %w", namespace, err)
	}
	if len(nqoses) == 0 {
		return nil
	}
	pod, err := c.podLister.Pods(namespace).Get(podName)
	if err != nil {
		pod = nil
	}
	if pod != nil && pod.Spec.NodeName != c.nodeName {
		return nil
	}
	for _, nqos := range nqoses {
		c.nqosController.Reconcile(nqosKey(nqos.Namespace, nqos.Name))
	}
	return nil
}

func (c *Controller) ensureSourcePortGroup(
	nqos *networkqosapi.NetworkQoS,
	qosState *networkQoSState,
	nctx *networkContext,
) error {
	selector, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid source pod selector: %w", err)
	}
	pods, err := c.podLister.Pods(nqos.Namespace).List(selector)
	if err != nil {
		return fmt.Errorf("failed to list source pods in namespace %s: %w", nqos.Namespace, err)
	}

	ports, missingPorts, err := c.sourcePodLSPUUIDs(pods, nctx)
	if err != nil {
		return err
	}
	if err := c.applySourcePortGroup(nqos, qosState, nctx, ports); err != nil {
		return err
	}
	if len(missingPorts) == 0 {
		return nil
	}
	return fmt.Errorf("%w: NetworkQoS %s/%s is missing logical switch ports %s; requeueing until they are created",
		errLogicalSwitchPortMissing, nqos.Namespace, nqos.Name,
		strings.Join(slices.Sorted(slices.Values(missingPorts)), ", "))
}

func (c *Controller) sourcePodLSPUUIDs(
	pods []*corev1.Pod,
	nctx *networkContext,
) ([]string, []string, error) {
	ports := []string{}
	missingPorts := []string{}
	for _, pod := range pods {
		if !sourcePodEligible(pod, c.nodeName) {
			continue
		}
		portNames, err := c.sourceLogicalPortNames(pod, nctx)
		if err != nil {
			return nil, nil, err
		}
		for _, portName := range portNames {
			uuid, found, err := c.logicalSwitchPortUUID(portName)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				missingPorts = append(missingPorts, portName)
				continue
			}
			ports = append(ports, uuid)
		}
	}
	return ports, missingPorts, nil
}

func sourcePodEligible(pod *corev1.Pod, nodeName string) bool {
	return util.PodScheduled(pod) && pod.Spec.NodeName == nodeName &&
		!util.PodWantsHostNetwork(pod) && !util.PodCompleted(pod) &&
		pod.DeletionTimestamp.IsZero()
}

func (c *Controller) sourceLogicalPortNames(
	pod *corev1.Pod,
	nctx *networkContext,
) ([]string, error) {
	var activeNetwork util.NetInfo
	needsActiveNetwork := nctx.netInfo.IsPrimaryNetwork() || !nctx.netInfo.IsUserDefinedNetwork()
	if needsActiveNetwork {
		var err error
		activeNetwork, err = c.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to find active network for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// A selected primary UDN may not be active for the pod's namespace.
	primaryUDNIsActiveNetwork := activeNetwork != nil && activeNetwork.GetNetworkName() == nctx.netInfo.GetNetworkName()
	if nctx.netInfo.IsPrimaryNetwork() && !primaryUDNIsActiveNetwork {
		return nil, nil
	}

	// Pods in a namespace with an active primary UDN do not use the default network.
	namespaceHasNonDefaultActiveNetwork := activeNetwork != nil && !activeNetwork.IsDefault()
	if !nctx.netInfo.IsUserDefinedNetwork() && namespaceHasNonDefaultActiveNetwork {
		return nil, nil
	}

	onNetwork, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
		pod,
		nctx.netInfo,
		activeNetwork,
		c.networkManager.GetNetworkNameForNADKey,
		c.networkManager.GetPrimaryNADForNamespace,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the network attachments of pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	// Skip pods that are not attached to the selected network.
	if !onNetwork {
		return nil, nil
	}
	if !nctx.netInfo.IsUserDefinedNetwork() {
		return []string{util.GetLogicalPortName(pod.Namespace, pod.Name)}, nil
	}
	names := make([]string, 0, len(networkMap))
	for nadKey := range networkMap {
		names = append(names, util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey))
	}
	return names, nil
}

func (c *Controller) logicalSwitchPortUUID(portName string) (string, bool, error) {
	lsp, err := libovsdbops.GetLogicalSwitchPort(c.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to look up logical switch port %s: %w", portName, err)
	}
	return lsp.UUID, true, nil
}

func (c *Controller) applySourcePortGroup(
	nqos *networkqosapi.NetworkQoS,
	qosState *networkQoSState,
	nctx *networkContext,
	ports []string,
) error {
	ids := sourcePortGroupDbIDs(nctx.controllerName, qosState.getObjectNameKey())
	portGroup := libovsdbutil.BuildPortGroup(ids, nil, nil)
	portGroup.Ports = sets.List(sets.New(ports...))
	qosState.sourcePortGroupName = portGroup.Name
	current, err := libovsdbops.GetPortGroup(c.nbClient, portGroup)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to look up source port group for NetworkQoS %s/%s: %w", nqos.Namespace, nqos.Name, err)
	}
	if err == nil && maps.Equal(current.ExternalIDs, portGroup.ExternalIDs) &&
		slices.Equal(current.Ports, portGroup.Ports) {
		return nil
	}
	if err := libovsdbops.CreateOrUpdatePortGroups(c.nbClient, portGroup); err != nil {
		return fmt.Errorf("failed to create or update the source port group of NetworkQoS %s/%s: %w", nqos.Namespace, nqos.Name, err)
	}
	return nil
}
