// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"fmt"

	v1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func MACFromStatus(ctx context.Context, reader client.Reader, key client.ObjectKey, interfaceName string) (string, error) {
	var vmi v1.VirtualMachineInstance
	if err := reader.Get(ctx, key, &vmi); err != nil {
		return "", err
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name == interfaceName && iface.MAC != "" {
			return iface.MAC, nil
		}
	}
	return "", fmt.Errorf("VMI %s has no MAC in status for interface %s", key, interfaceName)
}
