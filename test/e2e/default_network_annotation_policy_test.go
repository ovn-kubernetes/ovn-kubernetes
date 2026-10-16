// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

// TestDefaultNetworkAnnotationPolicy checks that missing annotation maps do not
// reject unrelated updates while the selected default network stays immutable.
func TestDefaultNetworkAnnotationPolicy(t *testing.T) {
	template, err := os.ReadFile("../../helm/ovn-kubernetes/charts/ovnkube-control-plane/templates/rbac-ovnkube-cluster-manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, policy, found := strings.Cut(string(template), "name: default-network-annotation\n")
	if !found {
		t.Fatal("missing default-network annotation policy")
	}
	_, expression, found := strings.Cut(policy, "- expression: >\n")
	if !found {
		t.Fatal("missing validation expression")
	}
	expression, _, _ = strings.Cut(expression, "\n      message:")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(strings.TrimSpace(expression))
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	pod := func(annotations map[string]string) map[string]any {
		metadata := map[string]any{}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata}
	}
	const key = "v1.multus-cni.io/default-network"
	for _, tc := range []struct {
		name     string
		old, new map[string]string
		allowed  bool
	}{
		{name: "both maps absent", allowed: true},
		{name: "empty maps", old: map[string]string{}, new: map[string]string{}, allowed: true},
		{name: "add unrelated annotation", new: map[string]string{"example.com/note": "value"}, allowed: true},
		{name: "remove unrelated annotation", old: map[string]string{"example.com/note": "value"}, allowed: true},
		{name: "retain network", old: map[string]string{key: "blue"}, new: map[string]string{key: "blue", "example.com/note": "value"}, allowed: true},
		{name: "add network", new: map[string]string{key: "blue"}},
		{name: "add empty network", new: map[string]string{key: ""}},
		{name: "change network", old: map[string]string{key: "blue"}, new: map[string]string{key: "red"}},
		{name: "remove network", old: map[string]string{key: "blue"}, new: map[string]string{"example.com/note": "value"}},
		{name: "remove entire map", old: map[string]string{key: "blue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{"object": pod(tc.new), "oldObject": pod(tc.old)})
			if err != nil {
				t.Fatal(err)
			}
			if result != types.Bool(tc.allowed) {
				t.Fatalf("allowed = %v, want %v", result, tc.allowed)
			}
		})
	}
}
