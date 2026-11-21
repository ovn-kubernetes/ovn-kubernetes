// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// normalize is a helper for check-helm-parity.sh. It reads a multi-document
// YAML manifest stream on stdin and, depending on the mode:
//
//	normalize — re-emits the documents with volatile content removed:
//	            the self-signed webhook cert Secret and webhook caBundle
//	            fields (random per helm render); CRDs (they have their own
//	            sync check via make verify-crds).
//	list      — emits one "kind namespace/name" line per document (minus
//	            CRDs), sorted, for object-set comparison.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	sigyaml "sigs.k8s.io/yaml"
)

const (
	modeNormalize = "normalize"
	modeList      = "list"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != modeNormalize && os.Args[1] != modeList) {
		fmt.Fprintln(os.Stderr, "usage: normalize <normalize|list> < manifests.yaml")
		os.Exit(2)
	}
	mode := os.Args[1]

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		panic(err)
	}

	var names []string
	for doc := range strings.SplitSeq("\n"+string(in), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj map[string]any
		if err := sigyaml.Unmarshal([]byte(doc), &obj); err != nil {
			fmt.Fprintf(os.Stderr, "normalize: malformed YAML document: %v\n%s\n", err, doc)
			os.Exit(1)
		}
		if obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		if kind == "" || kind == "CustomResourceDefinition" {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		namespace, _ := meta["namespace"].(string)

		// The chart self-signs the webhook cert at render time; the operator
		// injects it at runtime. Random per render — exclude entirely.
		if kind == "Secret" && name == "ovnkube-webhook-cert" {
			continue
		}

		if mode == modeList {
			names = append(names, fmt.Sprintf("%s %s/%s", kind, namespace, name))
			continue
		}

		if webhooks, ok := obj["webhooks"].([]any); ok {
			for _, w := range webhooks {
				wm, ok := w.(map[string]any)
				if !ok {
					continue
				}
				if cc, ok := wm["clientConfig"].(map[string]any); ok {
					delete(cc, "caBundle")
				}
			}
		}

		out, err := sigyaml.Marshal(obj)
		if err != nil {
			panic(err)
		}
		fmt.Printf("---\n%s", out)
	}

	if mode == modeList {
		slices.Sort(names)
		for _, n := range names {
			fmt.Println(n)
		}
	}
}
