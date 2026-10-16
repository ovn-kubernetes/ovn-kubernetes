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

// Exercise the deployed exclusion as well as the validator: a spoofed self-pod
// label must not prevent protected metadata from reaching admission at all.
func TestPodWebhookSelfExclusion(t *testing.T) {
	template, err := os.ReadFile("../../helm/ovn-kubernetes/charts/ovnkube-identity/templates/webhook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, expression, found := strings.Cut(string(template), "expression: >-")
	if !found {
		t.Fatal("missing match condition")
	}
	expression, _, _ = strings.Cut(expression, "\n    rules:")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
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
	pod := func(labels, annotations map[string]string) map[string]any {
		metadata := map[string]any{}
		if labels != nil {
			metadata["labels"] = labels
		}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata}
	}
	evaluate := func(t *testing.T, current, old any, invoke bool) {
		t.Helper()
		result, _, err := program.Eval(map[string]any{"request": map[string]any{"namespace": "ovn-kubernetes"}, "object": current, "oldObject": old})
		if err != nil {
			t.Fatal(err)
		}
		if result != types.Bool(invoke) {
			t.Fatalf("invoke = %v, want %v", result, invoke)
		}
	}
	self := map[string]string{"name": "ovnkube-identity"}
	t.Run("bootstrap self pod", func(t *testing.T) { evaluate(t, pod(self, nil), nil, false) })
	t.Run("ordinary pod", func(t *testing.T) { evaluate(t, pod(nil, nil), nil, true) })
	for _, key := range []string{"k8s.ovn.org/pod-networks", "kubevirt.io/domain", "kubevirt.io/allow-pod-bridge-network-live-migration", "kubevirt.io/migration-target-ready-timestamp"} {
		t.Run(key, func(t *testing.T) {
			forged := pod(self, map[string]string{key: ""})
			evaluate(t, forged, nil, true)
			evaluate(t, forged, forged, true)
			evaluate(t, pod(self, nil), forged, true)
		})
	}
	for _, key := range []string{"kubevirt.io", "vm.kubevirt.io/name", "kubevirt.io/nodeName"} {
		t.Run(key, func(t *testing.T) {
			forged := pod(map[string]string{"name": "ovnkube-identity", key: "virt-launcher"}, nil)
			evaluate(t, forged, nil, true)
			evaluate(t, forged, forged, true)
			evaluate(t, pod(self, nil), forged, true)
		})
	}
}
