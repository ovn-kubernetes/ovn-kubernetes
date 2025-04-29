package e2e

import (
	"fmt"
	"os"
	"os/exec"
)

// TODO: make this function idempotent; use golang netlink instead
func createVLANInterface(deviceName string, vlanID string, ipAddress *string) error {
	vlan := vlanName(deviceName, vlanID)
	cmd := exec.Command("sudo", "ip", "link", "add", "link", deviceName, "name", vlan, "type", "vlan", "id", vlanID)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create vlan interface %s: %v", vlan, err)
	}

	cmd = exec.Command("sudo", "ip", "link", "set", "dev", vlan, "up")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable vlan interface %s: %v", vlan, err)
	}

	if ipAddress != nil {
		cmd = exec.Command("sudo", "ip", "addr", "add", *ipAddress, "dev", vlan)
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to define the vlan interface %q IP Address %s: %v", vlan, *ipAddress, err)
		}
	}
	return nil
}

// TODO: make this function idempotent; use golang netlink instead
func deleteVLANInterface(deviceName string, vlanID string) error {
	vlan := vlanName(deviceName, vlanID)
	cmd := exec.Command("sudo", "ip", "link", "del", vlan)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete vlan interface %s: %v", vlan, err)
	}
	return nil
}

func vlanName(deviceName string, vlanID string) string {
	// MAX IFSIZE 16; got to truncate it to add the vlan suffix
	if len(deviceName)+len(vlanID)+1 > 16 {
		deviceName = deviceName[:len(deviceName)-len(vlanID)-1]
	}
	return fmt.Sprintf("%s.%s", deviceName, vlanID)
}
