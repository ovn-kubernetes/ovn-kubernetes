// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestInitTunnelKeysAllocatorOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, 1 << 24, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			nad := ovntesting.GenerateNAD("test", "test", "test", types.Layer2Topology, "10.0.0.0/24", types.NetworkRolePrimary)
			// Keep the derived transit key valid so the dynamic key reaches the bitmap.
			nad.Annotations = map[string]string{types.OvnNetworkTunnelKeysAnnotation: fmt.Sprintf("[%d,%d]", 16711683+1, invalid)}
			client := util.GetOVNClientset(nad)
			// Startup skips invalid reservations so reconciliation can allocate valid replacements.
			allocator, err := initTunnelKeysAllocator(client.NetworkAttchDefClient, client.NetworkConnectClient)
			if err != nil {
				t.Fatalf("invalid tunnel key blocked startup: %v", err)
			}
			if allocator == nil {
				t.Fatal("successful initialization returned no allocator")
			}
			// Allocate valid replacement keys for the network whose reservation was skipped.
			keys, err := allocator.AllocateKeys(id.NetworkTunnelKeyOwner("test"), 1, 2)
			if err != nil {
				t.Fatalf("failed to allocate replacement tunnel keys: %v", err)
			}
			if !slices.Equal(keys, []int{16711684, 16715779}) {
				t.Fatalf("unexpected replacement tunnel keys: %v", keys)
			}
		})
	}
}
