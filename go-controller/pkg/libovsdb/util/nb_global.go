// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// GetNBZone returns the zone name configured in the OVN Northbound database.
// It returns an error if there is no NBGlobal row or if the zone name is not configured.
func GetNBZone(nbClient libovsdbclient.Client) (string, error) {
	nbGlobal := &nbdb.NBGlobal{}
	nbGlobal, err := libovsdbops.GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return "", fmt.Errorf("error in getting the NBGlobal row  from Northbound db : err - %w", err)
	}

	if nbGlobal.Name == "" {
		return "", fmt.Errorf("OVN Northbound DB zone name is not set")
	}

	return nbGlobal.Name, nil
}
