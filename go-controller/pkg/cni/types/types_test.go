// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
)

func TestNetConfMarshalJSONIncludesOutboundSNAT(t *testing.T) {
	conf := NetConf{
		NetConf: cnitypes.NetConf{
			Name: "test-network",
			Type: "ovn-k8s-cni-overlay",
		},
		Transport:    "no-overlay",
		OutboundSNAT: "enabled",
	}

	data, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("failed to marshal NetConf: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal marshaled NetConf: %v", err)
	}

	if got["outboundSNAT"] != "enabled" {
		t.Fatalf("expected outboundSNAT to be marshaled, got %v from %s", got["outboundSNAT"], data)
	}
}
