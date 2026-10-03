// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sigyaml "sigs.k8s.io/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
)

// RunRender implements the "render" subcommand: render the operator's manifest
// templates for a CR read from a file, without contacting a cluster. Intended
// for debugging and for CI jobs that compare rendered output out of band.
//
// The webhook caBundle is injected at runtime by the operator and is left
// empty in rendered output.
func RunRender(args []string) {
	if err := runRender(args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "render: %v\n", err)
		os.Exit(1)
	}
}

func runRender(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	crPath := fs.String("cr", "", "Path to a YAML file containing the OVNK8sOperator CR. "+
		"Multi-document files are accepted; the single OVNK8sOperator document is used.")
	templatesDir := fs.String("templates", controller.TemplatesDir, "Directory containing the manifest templates.")
	outputDir := fs.String("output-dir", "", "Write manifests to <dir>/<component>/<file> instead of stdout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *crPath == "" {
		return fmt.Errorf("--cr is required")
	}

	cr, err := loadCR(*crPath)
	if err != nil {
		return err
	}

	embeddedConfig, err := defaults.LoadEmbeddedConfig()
	if err != nil {
		return fmt.Errorf("load embedded config: %w", err)
	}

	manifests, err := renderer.NewTemplateRenderer(*templatesDir, embeddedConfig).
		RenderAllTemplates(cr, renderer.RenderOptions{})
	if err != nil {
		return err
	}

	components := make([]string, 0, len(manifests))
	for name := range manifests {
		components = append(components, name)
	}
	sort.Strings(components)

	for _, component := range components {
		for _, m := range manifests[component] {
			if *outputDir != "" {
				path := filepath.Join(*outputDir, component, m.FileName)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(m.Content), 0o644); err != nil {
					return err
				}
				continue
			}
			fmt.Fprintf(stdout, "---\n# Source: %s/%s\n%s\n", component, m.FileName, m.Content)
		}
	}
	return nil
}

// loadCR reads a YAML file and returns the OVNK8sOperator document in it.
// Accepts multi-document files (e.g. `helm template` output of the operator
// chart) as long as they contain exactly one OVNK8sOperator.
func loadCR(path string) (*k8sv1alpha1.OVNK8sOperator, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var found *k8sv1alpha1.OVNK8sOperator
	for _, doc := range strings.Split("\n"+string(content), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := sigyaml.Unmarshal([]byte(doc), &probe); err != nil || probe.Kind != "OVNK8sOperator" {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%s contains more than one OVNK8sOperator document", path)
		}
		cr := &k8sv1alpha1.OVNK8sOperator{}
		if err := sigyaml.UnmarshalStrict([]byte(doc), cr); err != nil {
			return nil, fmt.Errorf("parse OVNK8sOperator from %s: %w", path, err)
		}
		found = cr
	}
	if found == nil {
		return nil, fmt.Errorf("no OVNK8sOperator document found in %s", path)
	}
	return found, nil
}
