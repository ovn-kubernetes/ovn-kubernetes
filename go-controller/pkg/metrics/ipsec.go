// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type ovsAppctlClient func(timeout int, args ...string) (string, string, error)

// IPsecStatus represents the output of 'ovs-appctl -t ovs-monitor-ipsec ipsec/status'
// The outer map keys are connection names (e.g., "ovn-194484-0")
// The inner map keys are tunnel names (e.g., "ovn-194484-0-in-1", "ovn-194484-0-out-1")
// The values are tunnel status strings containing ESP information
type IPsecStatus map[string]map[string]string

// areAllIPsecTunnelsEstablished checks if all expected IPsec tunnels (based on geneve interfaces)
// have established Child SAs by querying ovs-monitor-ipsec daemon.
// 1. Retrieve geneve interfaces from OVS DB.
// 2. Query 'ovs-appctl -t ovs-monitor-ipsec ipsec/status' to get tunnel status.
// 3. Parse the JSON output to extract tunnel names.
// 4. Return true when all expected tunnels (-in-1 and -out-1 for each geneve interface) are found.
func areAllIPsecTunnelsEstablished(ovsDBClient libovsdbclient.Client, ovsAppctl ovsAppctlClient) (bool, error) {
	// Get geneve interfaces from OVS DB
	geneveInterfaces, err := getGeneveInterfaces(ovsDBClient)
	if err != nil {
		return false, fmt.Errorf("failed to retrieve geneve interfaces: %v", err)
	}
	if len(geneveInterfaces) == 0 {
		return false, fmt.Errorf("no geneve interfaces found")
	}

	// Each geneve interface has -in-1 and -out-1 tunnels.
	expectedTunnels := sets.New[string]()
	for _, geneveTunnel := range geneveInterfaces {
		expectedTunnels.Insert(fmt.Sprintf("%s-in-1", geneveTunnel))
		expectedTunnels.Insert(fmt.Sprintf("%s-out-1", geneveTunnel))
	}

	// Query ovs-monitor-ipsec for tunnel status
	stdout, stderr, err := ovsAppctl(5, "-t", "ovs-monitor-ipsec", "ipsec/status")
	if err != nil {
		return false, fmt.Errorf("failed to retrieve ipsec status, stderr: %v, err: %v", stderr, err)
	}
	if stdout == "" {
		return false, fmt.Errorf("no IPsec tunnels found")
	}

	// The output is in Python dict format with single quotes, convert to JSON
	// First, escape any double quotes inside the string values
	jsonOutput := strings.ReplaceAll(stdout, "\"", "\\\"")
	// Then replace single quotes with double quotes to get valid JSON
	jsonOutput = strings.ReplaceAll(jsonOutput, "'", "\"")

	// Parse the JSON output
	var status IPsecStatus
	if err := json.Unmarshal([]byte(jsonOutput), &status); err != nil {
		return false, fmt.Errorf("failed to parse ipsec status output: %v", err)
	}

	// Extract all tunnel names from the nested map structure
	foundTunnels := sets.New[string]()
	for _, tunnels := range status {
		for tunnelName := range tunnels {
			if expectedTunnels.Has(tunnelName) {
				foundTunnels.Insert(tunnelName)
			}
		}
	}

	// Check if all expected tunnels were found
	return foundTunnels.Len() == expectedTunnels.Len(), nil
}

func getGeneveInterfaces(ovsDBClient libovsdbclient.Client) ([]string, error) {
	interfaces, err := ovsops.FindInterfacesWithPredicate(ovsDBClient, func(intf *vswitchd.Interface) bool {
		return intf.Type == "geneve"
	})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve Geneve interfaces: %v", err)
	}

	var infNames []string
	for _, intf := range interfaces {
		infNames = append(infNames, intf.Name)
	}
	return infNames, nil
}
