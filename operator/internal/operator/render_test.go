// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// TestRunRender covers the `render` subcommand, which prints the manifests the
// operator would deploy for a CR without a cluster (hack/helm-parity uses it).
// It passes the sample CR inside a multi-document file and checks that the CR
// is picked out, the real templates render, and every output document is valid
// YAML.

package operator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sigyaml "sigs.k8s.io/yaml"
)

func TestRunRender(t *testing.T) {
	// Multi-document input: the OVNK8sOperator document must be picked out.
	sample, err := os.ReadFile("../../config/samples/k8s_v1alpha1_ovnk8soperator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crPath := filepath.Join(t.TempDir(), "cr.yaml")
	multiDoc := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: unrelated\n---\n" + string(sample)
	if err := os.WriteFile(crPath, []byte(multiDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = runRender([]string{"--cr", crPath, "--templates", "../../templates"}, &out)
	if err != nil {
		t.Fatalf("runRender: %v", err)
	}

	rendered := out.String()
	for _, want := range []string{
		"# Source: global/ovn-setup.yaml",
		"# Source: ovnkubeNode/ovnkube-single-node-zone.yaml",
		"kind: DaemonSet",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}

	// Every rendered document must be parseable YAML.
	i := 0
	for doc := range strings.SplitSeq(rendered, "\n---\n") {
		i++
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var v map[string]any
		if err := sigyaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Errorf("document %d is not valid YAML: %v", i, err)
		}
	}
}
