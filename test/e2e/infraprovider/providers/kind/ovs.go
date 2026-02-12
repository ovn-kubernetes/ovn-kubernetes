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
