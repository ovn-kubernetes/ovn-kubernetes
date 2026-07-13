// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

func TestNetConfMarshalJSONIncludesOutboundSNAT(t *testing.T) {
	netConf := NetConf{
		Transport:         "no-overlay",
		OutboundSNAT:      "enabled",
		CNIRequestTimeout: "45s",
	}

	rawNetConf, err := json.Marshal(netConf)
	if err != nil {
		t.Fatalf("failed to marshal NetConf: %v", err)
	}

	var parsedNetConf struct {
		Transport         string `json:"transport"`
		OutboundSNAT      string `json:"outboundSNAT"`
		CNIRequestTimeout string `json:"cniRequestTimeout"`
	}
	if err := json.Unmarshal(rawNetConf, &parsedNetConf); err != nil {
		t.Fatalf("failed to unmarshal NetConf: %v", err)
	}

	if parsedNetConf.Transport != netConf.Transport {
		t.Fatalf("expected transport %q, got %q", netConf.Transport, parsedNetConf.Transport)
	}
	if parsedNetConf.OutboundSNAT != netConf.OutboundSNAT {
		t.Fatalf("expected outboundSNAT %q, got %q", netConf.OutboundSNAT, parsedNetConf.OutboundSNAT)
	}
	if parsedNetConf.CNIRequestTimeout != netConf.CNIRequestTimeout {
		t.Fatalf("expected CNI request timeout %q, got %q", netConf.CNIRequestTimeout, parsedNetConf.CNIRequestTimeout)
	}
}
