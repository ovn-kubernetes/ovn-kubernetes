// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"
	"net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type ipv4Neighbor struct {
	ip  net.IP
	mac net.HardwareAddr
}

func sendNeighborGARPs(interfaceName string, neighbors []ipv4Neighbor, broadcastGARP func(string, util.GARP) error) error {
	for _, neighbor := range neighbors {
		garp, err := util.NewGARP(neighbor.ip, &neighbor.mac)
		if err != nil {
			return fmt.Errorf("failed to create GARP for neighbor IP %s: %w", neighbor.ip, err)
		}
		if err := broadcastGARP(interfaceName, garp); err != nil {
			return fmt.Errorf("failed broadcasting GARP for neighbor %s: %w", neighbor.ip, err)
		}
	}
	return nil
}
