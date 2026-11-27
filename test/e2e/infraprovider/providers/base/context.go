// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package base

import (
	"context"
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

// Context is a per-test sandbox. It sets up underlay networks by running
// ovs-vsctl in the ovnkube-node Pods, and finds each Node's underlay
// interface through Provider.
type Context struct {
	*testcontext.TestContext
	api.ExternalContainerContextProvider
	Provider api.Provider
}

func (c *Context) SetupUnderlay(f *framework.Framework, underlay api.Underlay) error {
	if underlay.LogicalNetworkName == "" {
		return fmt.Errorf("underlay logical network name must be set")
	}

	if underlay.PhysicalNetworkName == "" {
		underlay.PhysicalNetworkName = "underlay"
	}

	if underlay.BridgeName == "" {
		underlay.BridgeName = secondaryBridge
	}

	c.AddCleanUpFn(func() error {
		// Find the OVS pods again to cover cases that restart the PODs
		ovsPods, err := findOVSPods(f)
		if err != nil {
			return fmt.Errorf("failed finding OVS pods during underlay tear down: %w", err)
		}
		for _, ovsPod := range ovsPods {
			if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
				if err := removeOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
					return fmt.Errorf("failed to remove OVS bridge %s for pod %s/%s during cleanup: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
			if err := configureBridgeMappings(
				ovsPod.Namespace,
				ovsPod.Name,
				defaultNetworkBridgeMapping(),
			); err != nil {
				return fmt.Errorf("failed to restore default bridge mappings for pod %s/%s during cleanup: %w", ovsPod.Namespace, ovsPod.Name, err)
			}
		}
		return nil
	})

	ovsPods, err := findOVSPods(f)
	if err != nil {
		return fmt.Errorf("failed finding OVS pods during underlay setup: %w", err)
	}
	for _, ovsPod := range ovsPods {
		if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
			underlayNetwork, err := c.Provider.GetNetwork(underlay.PhysicalNetworkName)
			if err != nil {
				return fmt.Errorf("failed to get underlay network %s: %w", underlay.PhysicalNetworkName, err)
			}
			underlayInterface, err := c.Provider.GetK8NodeNetworkInterface(ovsPod.Spec.NodeName, underlayNetwork)
			if err != nil {
				return fmt.Errorf("failed to get underlay interface for network %s on node %s: %w", underlay.PhysicalNetworkName, ovsPod.Spec.NodeName, err)
			}
			if err := ensureOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
				return fmt.Errorf("failed to add OVS bridge %s for pod %s/%s: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}

			if err := ovsAttachPortToBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName); err != nil {
				return fmt.Errorf("failed to attach port %s to bridge %s for pod %s/%s: %w", underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}
			if underlay.VlanID > 0 {
				if err := ovsEnableVLANAccessPort(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName, underlay.VlanID); err != nil {
					return fmt.Errorf("failed to enable VLAN %d on port %s for bridge %s for pod %s/%s: %w", underlay.VlanID, underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
		}
		if err := configureBridgeMappings(
			ovsPod.Namespace,
			ovsPod.Name,
			defaultNetworkBridgeMapping(),
			bridgeMapping(underlay.LogicalNetworkName, underlay.BridgeName),
		); err != nil {
			return fmt.Errorf("failed to configure bridge mappings for pod %s/%s for logical network %s to bridge %s: %w", ovsPod.Namespace, ovsPod.Name, underlay.LogicalNetworkName, underlay.BridgeName, err)
		}
	}

	return nil
}

func findOVSPods(f *framework.Framework) ([]corev1.Pod, error) {
	const ovsKubeNodeLabel = "app=ovnkube-node"
	ovsPodList, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: ovsKubeNodeLabel},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list OVS pods with label %q at namespace %q: %w", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace(), err)
	}

	if len(ovsPodList.Items) == 0 {
		return nil, fmt.Errorf("no pods with label %q in namespace %q", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace())
	}
	return ovsPodList.Items, nil
}
