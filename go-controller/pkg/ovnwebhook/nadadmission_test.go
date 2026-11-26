// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"strings"
	"testing"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNADAdmission(t *testing.T) {
	const (
		nadNamespace = "team-a"
		// The fields ovnkube requires in the config of a user-defined network.
		ovnFields   = `"type":"ovn-k8s-cni-overlay","netAttachDefName":"team-a/nad","topology":"layer2"`
		ovnPlugin   = `{` + ovnFields + `}`
		otherPlugin = `{"type":"macvlan","master":"eth1"}`
		udnErr      = "reserved for UserDefinedNetwork"
		cudnErr     = "reserved for ClusterUserDefinedNetwork"
	)
	single := func(name string) string {
		return `{"cniVersion":"1.0.0","name":"` + name + `",` + ovnFields + `}`
	}
	list := func(name string, plugins ...string) string {
		return `{"cniVersion":"1.0.0","name":"` + name + `","plugins":[` + strings.Join(plugins, ",") + `]}`
	}
	// A JSON escape sequence, built here so the source holds no escape itself.
	escape := func(hex string) string { return string([]byte{'\\', 'u'}) + hex }

	nad := func(cniConfig string) *nadv1.NetworkAttachmentDefinition {
		return &nadv1.NetworkAttachmentDefinition{Spec: nadv1.NetworkAttachmentDefinitionSpec{Config: cniConfig}}
	}
	ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Namespace: nadNamespace},
	})
	validator := NewNADAdmissionWebhook()

	configs := []struct {
		name      string
		cniConfig string
		// wantErr is a substring of the expected error, empty if admitted.
		wantErr string
	}{
		{name: "UDN name of another namespace", cniConfig: single("victim_blue"), wantErr: `reserved for UserDefinedNetwork "blue" in namespace "victim"`},
		{name: "name colliding with the prefix of a UDN of another namespace", cniConfig: single("victim_app.net"), wantErr: udnErr},
		{name: "UDN name of a namespace that does not exist", cniConfig: single("future_net"), wantErr: udnErr},
		{name: "empty namespace part", cniConfig: single("_blue"), wantErr: udnErr},
		{name: "CUDN name", cniConfig: single("cluster_udn_shared"), wantErr: `reserved for ClusterUserDefinedNetwork "shared"`},
		{name: "plugin list", cniConfig: list("victim_blue", ovnPlugin), wantErr: udnErr},
		{name: "plugin list with a CUDN name", cniConfig: list("cluster_udn_shared", ovnPlugin), wantErr: cudnErr},

		// The config must be read the way ovnkube reads it, or a crafted one
		// shows the webhook one name and ovnkube another. ovnkube matches the
		// keys of a single config ignoring case, with the last of repeated keys
		// winning. For a plugin list it matches the top-level keys exactly and
		// ignores a top-level type.
		{name: "escaped CNI type", cniConfig: `{"name":"victim_blue","type":"ovn-k8s-cni-` + escape("006f") + `verlay","netAttachDefName":"team-a/nad","topology":"layer2"}`, wantErr: udnErr},
		{name: "escaped underscore in the name", cniConfig: single("victim" + escape("005f") + "blue"), wantErr: udnErr},
		{name: "single config with upper-case keys", cniConfig: `{"NAME":"victim_blue","TYPE":"ovn-k8s-cni-overlay","NetAttachDefName":"team-a/nad","Topology":"layer2"}`, wantErr: udnErr},
		{name: "single config with a repeated name, reserved last", cniConfig: `{"name":"blue","name":"victim_blue",` + ovnFields + `}`, wantErr: udnErr},
		{name: "single config with a name repeated in another case, reserved last", cniConfig: `{"name":"blue","Name":"victim_blue",` + ovnFields + `}`, wantErr: udnErr},
		{name: "plugin list with a top-level type that is not a string", cniConfig: `{"type":0,"name":"victim_blue","plugins":[` + ovnPlugin + `]}`, wantErr: udnErr},
		{name: "plugin list with a name repeated in another case, reserved first", cniConfig: `{"name":"victim_blue","Name":"blue","plugins":[` + ovnPlugin + `]}`, wantErr: udnErr},
		{name: "plugin list with a name repeated in another case, reserved last", cniConfig: `{"Name":"blue","name":"victim_blue","plugins":[` + ovnPlugin + `]}`, wantErr: udnErr},
		{name: "plugin list with a name that is not a string before the reserved one", cniConfig: `{"name":7,"name":"victim_blue","plugins":[` + ovnPlugin + `]}`, wantErr: udnErr},
		{name: "plugin with an upper-case type key", cniConfig: list("victim_blue", `{"TYPE":"ovn-k8s-cni-overlay","netAttachDefName":"team-a/nad","topology":"layer2"}`), wantErr: udnErr},

		{name: "UDN name of the NAD namespace", cniConfig: single(nadNamespace + "_blue")},
		{name: "UDN name of the NAD namespace in a plugin list", cniConfig: list(nadNamespace+"_blue", ovnPlugin)},
		{name: "name without an underscore", cniConfig: single("blue")},
		{name: "name with several underscores", cniConfig: single("a_b_c")},
		{name: "name ending with an underscore", cniConfig: single("tenant_")},
		{name: "CUDN prefix without a name", cniConfig: single("cluster_udn_")},
		{name: "config of another CNI", cniConfig: `{"type":"macvlan","name":"victim_blue"}`},
		{name: "plugin list of other CNIs", cniConfig: list("victim_blue", otherPlugin, `{"type":"tuning"}`)},
		{name: "invalid JSON", cniConfig: `{not json`},
		{name: "empty config"},

		// Configs with a reserved-looking name that ovnkube does not turn into
		// a network of that name, so there is nothing to protect.
		{name: "no topology, which ovnkube reads as the default network", cniConfig: `{"name":"victim_blue","type":"ovn-k8s-cni-overlay"}`},
		{name: "no netAttachDefName, which ovnkube rejects", cniConfig: `{"name":"victim_blue","type":"ovn-k8s-cni-overlay","topology":"layer2"}`},
		{name: "single config with a repeated name, reserved first", cniConfig: `{"name":"victim_blue","name":"blue",` + ovnFields + `}`},
		{name: "plugin list whose first plugin is another CNI", cniConfig: list("victim_blue", otherPlugin, ovnPlugin)},
		{name: "plugin list with a second plugin, which ovnkube rejects", cniConfig: list("victim_blue", ovnPlugin, otherPlugin)},
		{name: "plugin list whose first plugin is another CNI, with a top-level OVN-Kubernetes type", cniConfig: `{"type":"ovn-k8s-cni-overlay","name":"victim_blue","plugins":[` + otherPlugin + `]}`},
		{name: "empty plugin list, which ovnkube rejects", cniConfig: `{"name":"victim_blue","plugins":[],` + ovnFields + `}`},
		{name: "plugin list that is also a valid single config, which ovnkube rejects", cniConfig: `{"name":"victim_blue","plugins":[` + ovnPlugin + `],` + ovnFields + `}`},
		{name: "reserved name only inside the plugin, which ovnkube overwrites", cniConfig: list("blue", `{"name":"victim_blue",`+ovnFields+`}`)},
		{name: "plugin with a repeated type, another CNI last", cniConfig: list("victim_blue", `{"type":"ovn-k8s-cni-overlay","type":"macvlan","netAttachDefName":"team-a/nad","topology":"layer2"}`)},
	}
	for _, tc := range configs {
		t.Run("create/"+tc.name, func(t *testing.T) {
			_, err := validator.ValidateCreate(ctx, nad(tc.cniConfig))
			expectAdmission(t, err, tc.wantErr)
		})
		// The same rule applies to a config an update introduces.
		t.Run("update to/"+tc.name, func(t *testing.T) {
			_, err := validator.ValidateUpdate(ctx, nad(single("blue")), nad(tc.cniConfig))
			expectAdmission(t, err, tc.wantErr)
		})
	}

	reserved, plain := single("victim_blue"), single("blue")
	updates := []struct {
		name               string
		oldConfig, newConf string
		wantErr            string
	}{
		{name: "unchanged reserved config, as in a metadata-only update", oldConfig: reserved, newConf: reserved},
		{name: "unchanged plain config", oldConfig: plain, newConf: plain},
		{name: "change of a plain config", oldConfig: plain, newConf: single("green")},
		{name: "change within the NAD namespace", oldConfig: single(nadNamespace + "_blue"), newConf: single(nadNamespace + "_green")},
		{name: "change of a reserved config to a plain one", oldConfig: reserved, newConf: plain, wantErr: udnErr},
		{name: "change of a reserved config to another CNI", oldConfig: reserved, newConf: `{"type":"macvlan"}`, wantErr: udnErr},
		{name: "change of a CUDN config", oldConfig: single("cluster_udn_shared"), newConf: plain, wantErr: cudnErr},
	}
	for _, tc := range updates {
		t.Run("update/"+tc.name, func(t *testing.T) {
			_, err := validator.ValidateUpdate(ctx, nad(tc.oldConfig), nad(tc.newConf))
			expectAdmission(t, err, tc.wantErr)
		})
	}

	t.Run("delete is not validated", func(t *testing.T) {
		_, err := validator.ValidateDelete(ctx, nad(reserved))
		expectAdmission(t, err, "")
	})

	t.Run("the namespace comes from the request, not from the object", func(t *testing.T) {
		spoofed := nad(reserved)
		spoofed.Namespace = "victim"
		_, err := validator.ValidateCreate(ctx, spoofed)
		expectAdmission(t, err, udnErr)
	})

	t.Run("a request without admission context is an error", func(t *testing.T) {
		_, err := validator.ValidateCreate(context.Background(), nad(plain))
		if err == nil {
			t.Fatal("expected an error without an admission request in the context")
		}
	})
}

func expectAdmission(t *testing.T, err error, wantErr string) {
	t.Helper()
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("expected the NAD to be admitted, got: %v", err)
	case wantErr != "" && err == nil:
		t.Fatalf("expected the NAD to be rejected with %q, but it was admitted", wantErr)
	case wantErr != "" && !strings.Contains(err.Error(), wantErr):
		t.Fatalf("expected the rejection to contain %q, got: %v", wantErr, err)
	}
}
