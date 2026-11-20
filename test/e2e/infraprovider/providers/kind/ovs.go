// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"fmt"
	"strings"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"

	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	secondaryBridge = "ovsbr1"
)

func ensureOVSBridge(podNamespace, podName string, bridgeName string) error {
	cmd := fmt.Sprintf("ovs-vsctl br-exists %[1]s || ovs-vsctl add-br %[1]s", bridgeName)
	if _, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5); err != nil {
		return fmt.Errorf("failed to add ovs bridge %q: %v", bridgeName, err)
	}
	return nil
}

func removeOVSBridge(podNamespace, podName string, bridgeName string) error {
	cmd := fmt.Sprintf("if ovs-vsctl br-exists %[1]s; then ovs-vsctl del-br %[1]s; fi", bridgeName)
	if _, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5); err != nil {
		return fmt.Errorf("failed to remove ovs bridge %q: %v", bridgeName, err)
	}
	return nil
}

func ovsAttachPortToBridge(podNamespace, podName string, bridgeName string, portName string) error {
	cmd := fmt.Sprintf("ovs-vsctl list port %[2]s || ovs-vsctl add-port %[1]s %[2]s", bridgeName, portName)
	if _, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5); err != nil {
		return fmt.Errorf("failed to addadd  port %s from OVS bridge %s: %v", portName, bridgeName, err)
	}
	return nil
}

func ovsEnableVLANAccessPort(podNamespace, podName string, bridgeName string, portName string, vlanID int) error {
	cmd := fmt.Sprintf("ovs-vsctl set port %[1]s tag=%[2]d vlan_mode=access", portName, vlanID)
	if _, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5); err != nil {
		return fmt.Errorf("failed to enable vlan access port %s from OVS bridge %s: %v", portName, bridgeName, err)
	}
	return nil
}

type BridgeMapping struct {
	physnet   string
	ovsBridge string
}

func (bm BridgeMapping) String() string {
	return fmt.Sprintf("%s:%s", bm.physnet, bm.ovsBridge)
}

type BridgeMappings []BridgeMapping

func (bms BridgeMappings) String() string {
	return strings.Join(Map(bms, func(bm BridgeMapping) string { return bm.String() }), ",")
}

// without returns a copy with any mapping for the given physnet removed.
func (bms BridgeMappings) without(physnet string) BridgeMappings {
	result := make(BridgeMappings, 0, len(bms))
	for _, bm := range bms {
		if bm.physnet != physnet {
			result = append(result, bm)
		}
	}
	return result
}

func Map[T, V any](items []T, fn func(T) V) []V {
	result := make([]V, len(items))
	for i, t := range items {
		result[i] = fn(t)
	}
	return result
}

func configureBridgeMappings(podNamespace, podName string, mappings ...BridgeMapping) error {
	mappingsString := fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", BridgeMappings(mappings).String())
	cmd := strings.Join([]string{"ovs-vsctl", "set", "open", ".", mappingsString}, " ")
	if _, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5); err != nil {
		return fmt.Errorf("failed to configure bridge mappings %q: %v", mappingsString, err)
	}
	return nil
}

// readBridgeMappings returns the OVN bridge mappings currently configured on the node.
func readBridgeMappings(podNamespace, podName string) (BridgeMappings, error) {
	cmd := "ovs-vsctl --if-exists get open . external_ids:ovn-bridge-mappings"
	out, err := e2epodoutput.RunHostCmdWithRetries(podNamespace, podName, cmd, time.Second, time.Second*5)
	if err != nil {
		return nil, fmt.Errorf("failed to read bridge mappings: %v", err)
	}
	return parseBridgeMappings(out), nil
}

// parseBridgeMappings parses the value returned by `ovs-vsctl get open . external_ids:ovn-bridge-mappings`,
// e.g. `"physnet:br-ex,blue:ovsbr-vlan30"`, into a BridgeMappings slice.
func parseBridgeMappings(raw string) BridgeMappings {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return BridgeMappings{}
	}
	var mappings BridgeMappings
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			continue
		}
		mappings = append(mappings, BridgeMapping{physnet: parts[0], ovsBridge: parts[1]})
	}
	return mappings
}

// addBridgeMappings merges the given mappings into the node's existing OVN bridge mappings,
// replacing any entry that shares a physnet name so repeated calls stay idempotent. Unlike
// configureBridgeMappings it preserves mappings configured by other underlays/tests.
func addBridgeMappings(podNamespace, podName string, mappings ...BridgeMapping) error {
	existing, err := readBridgeMappings(podNamespace, podName)
	if err != nil {
		return fmt.Errorf("failed to read bridge mappings for pod %s/%s: %w", podNamespace, podName, err)
	}
	merged := existing
	for _, bm := range mappings {
		merged = merged.without(bm.physnet)
		merged = append(merged, bm)
	}
	if err := configureBridgeMappings(podNamespace, podName, merged...); err != nil {
		return fmt.Errorf("failed to add bridge mappings for pod %s/%s: %w", podNamespace, podName, err)
	}
	return nil
}

// removeBridgeMappings removes the mappings for the given physnet names from the node's
// existing OVN bridge mappings, leaving mappings owned by other underlays/tests intact.
func removeBridgeMappings(podNamespace, podName string, physnets ...string) error {
	existing, err := readBridgeMappings(podNamespace, podName)
	if err != nil {
		return fmt.Errorf("failed to read bridge mappings for pod %s/%s: %w", podNamespace, podName, err)
	}
	remaining := existing
	for _, physnet := range physnets {
		remaining = remaining.without(physnet)
	}
	if err := configureBridgeMappings(podNamespace, podName, remaining...); err != nil {
		return fmt.Errorf("failed to remove bridge mappings for pod %s/%s: %w", podNamespace, podName, err)
	}
	return nil
}

func defaultNetworkBridgeMapping() BridgeMapping {
	return BridgeMapping{
		physnet:   "physnet",
		ovsBridge: deploymentconfig.Get().ExternalBridgeName(),
	}
}

func bridgeMapping(physnet, ovsBridge string) BridgeMapping {
	return BridgeMapping{
		physnet:   physnet,
		ovsBridge: ovsBridge,
	}
}
