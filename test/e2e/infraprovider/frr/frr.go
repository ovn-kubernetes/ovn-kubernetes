// Package frr provides shared FRR (Free Range Routing) configuration utilities
// for both e2e tests and the bgp-setup tool.
package frr

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"text/template"

	utilnet "k8s.io/utils/net"
)

// Image is the FRR container image used for BGP testing
const Image = "quay.io/frrouting/frr:10.4.1"

// Router contains BGP router configuration for FRR templates
type Router struct {
	VRF           string
	NeighborsIPv4 []string
	NeighborsIPv6 []string
	NetworksIPv4  []string
	NetworksIPv6  []string
}

// FRRKubernetesConfig contains the full FRR configuration data for templates
type FRRKubernetesConfig struct {
	// Name and Labels are used for FRRConfiguration metadata (frr-k8s)
	Name    string
	Labels  map[string]string
	Routers []Router
}

// TemplateSource provides access to FRR configuration templates.
// This interface allows using either embedded templates (for e2e tests)
// or filesystem templates (for the bgp-setup tool).
type TemplateSource interface {
	// ParseFRRTemplates parses the FRR daemon configuration templates
	ParseFRRTemplates() (*template.Template, error)
	// ParseFRRK8sTemplates parses the FRR-k8s CRD templates
	ParseFRRK8sTemplates() (*template.Template, error)
}

// EmbeddedSource reads templates from an embedded filesystem
type EmbeddedSource struct {
	fs      embed.FS
	baseDir string
}

// NewEmbeddedSource creates a template source from an embedded filesystem.
// baseDir is the path within the embed.FS where the frr/ and frr-k8s/ directories are located.
func NewEmbeddedSource(efs embed.FS, baseDir string) *EmbeddedSource {
	return &EmbeddedSource{fs: efs, baseDir: baseDir}
}

func (e *EmbeddedSource) ParseFRRTemplates() (*template.Template, error) {
	return template.ParseFS(e.fs, path.Join(e.baseDir, "frr", "*.tmpl"))
}

func (e *EmbeddedSource) ParseFRRK8sTemplates() (*template.Template, error) {
	return template.ParseFS(e.fs, path.Join(e.baseDir, "frr-k8s", "*.tmpl"))
}

// FilesystemSource reads templates from the local filesystem
type FilesystemSource struct {
	baseDir string
}

// NewFilesystemSource creates a template source from a filesystem directory.
// baseDir should contain frr/ and frr-k8s/ subdirectories with *.tmpl files.
func NewFilesystemSource(baseDir string) *FilesystemSource {
	return &FilesystemSource{baseDir: baseDir}
}

func (f *FilesystemSource) ParseFRRTemplates() (*template.Template, error) {
	pattern := filepath.Join(f.baseDir, "frr", "*.tmpl")
	return template.ParseGlob(pattern)
}

func (f *FilesystemSource) ParseFRRK8sTemplates() (*template.Template, error) {
	pattern := filepath.Join(f.baseDir, "frr-k8s", "*.tmpl")
	return template.ParseGlob(pattern)
}

// WriteConfigToDir generates FRR daemon configuration files (frr.conf and daemons)
// in the specified output directory.
// neighborIPs are the BGP peer IP addresses (mixed IPv4 and IPv6).
// advertiseNetworks are the networks to advertise via BGP.
func WriteConfigToDir(source TemplateSource, outputDir string, neighborIPs, advertiseNetworks []string) error {
	templates, err := source.ParseFRRTemplates()
	if err != nil {
		return fmt.Errorf("failed to parse FRR templates: %w", err)
	}

	// Split IPs by family
	neighborsIPv4, neighborsIPv6 := splitIPStringsByIPFamily(neighborIPs)
	networksIPv4, networksIPv6 := splitCIDRStringsByIPFamily(advertiseNetworks)

	conf := FRRKubernetesConfig{
		Routers: []Router{
			{
				NeighborsIPv4: neighborsIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv4:  networksIPv4,
				NetworksIPv6:  networksIPv6,
			},
		},
	}

	if err := executeFileTemplate(templates, outputDir, "frr.conf", conf); err != nil {
		return fmt.Errorf("failed to execute frr.conf template: %w", err)
	}

	if err := executeFileTemplate(templates, outputDir, "daemons", nil); err != nil {
		return fmt.Errorf("failed to execute daemons template: %w", err)
	}

	return nil
}

// GenerateFRRDaemonConfig generates FRR daemon configuration files in a temporary directory.
// Returns the path to the temporary directory containing the configuration files.
// The caller is responsible for cleaning up the directory.
func GenerateFRRDaemonConfig(source TemplateSource, neighborIPs, advertiseNetworks []string) (string, error) {
	dir, err := os.MkdirTemp("", "frrconf-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	if err := WriteConfigToDir(source, dir, neighborIPs, advertiseNetworks); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

// GenerateFRRK8sConfig generates an FRRConfiguration CRD YAML file for frr-k8s.
// networkName is used as the resource name and VRF name.
// Returns the path to a temporary directory containing the frrconf.yaml file.
// The caller is responsible for cleaning up the directory.
func GenerateFRRK8sConfig(source TemplateSource, networkName string, labels map[string]string, neighborIPs, receiveNetworks []string) (string, error) {
	return GenerateFRRK8sConfigWithVRF(source, networkName, networkName, labels, neighborIPs, receiveNetworks)
}

// GenerateFRRK8sConfigWithVRF generates an FRRConfiguration CRD YAML file for frr-k8s
// with an explicit VRF name (which can be different from networkName or empty for default network).
// Returns the path to a temporary directory containing the frrconf.yaml file.
// The caller is responsible for cleaning up the directory.
func GenerateFRRK8sConfigWithVRF(source TemplateSource, networkName, vrf string, labels map[string]string, neighborIPs, receiveNetworks []string) (string, error) {
	templates, err := source.ParseFRRK8sTemplates()
	if err != nil {
		return "", fmt.Errorf("failed to parse FRR-k8s templates: %w", err)
	}

	dir, err := os.MkdirTemp("", "frrk8sconf-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Split by IP family
	neighborsIPv4, neighborsIPv6 := splitIPStringsByIPFamily(neighborIPs)
	receivesIPv4, receivesIPv6 := splitCIDRStringsByIPFamily(receiveNetworks)

	conf := FRRKubernetesConfig{
		Name:   networkName,
		Labels: labels,
		Routers: []Router{
			{
				VRF:           vrf,
				NeighborsIPv4: neighborsIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv4:  receivesIPv4,
				NetworksIPv6:  receivesIPv6,
			},
		},
	}

	if err := executeFileTemplate(templates, dir, "frrconf.yaml", conf); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("failed to execute frrconf.yaml template: %w", err)
	}

	return dir, nil
}

// Helper functions

func splitCIDRStringsByIPFamily(cidrs []string) (ipv4 []string, ipv6 []string) {
	for _, cidr := range cidrs {
		switch {
		case utilnet.IsIPv4CIDRString(cidr):
			ipv4 = append(ipv4, cidr)
		case utilnet.IsIPv6CIDRString(cidr):
			ipv6 = append(ipv6, cidr)
		}
	}
	return
}

func splitIPStringsByIPFamily(ips []string) (ipv4 []string, ipv6 []string) {
	for _, ip := range ips {
		switch {
		case utilnet.IsIPv4String(ip):
			ipv4 = append(ipv4, ip)
		case utilnet.IsIPv6String(ip):
			ipv6 = append(ipv6, ip)
		}
	}
	return
}

func executeFileTemplate(templates *template.Template, directory, name string, data any) error {
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return templates.ExecuteTemplate(f, name, data)
}

// SubFS is a helper to create a sub-filesystem from an embed.FS.
// This is useful when the embed.FS has a prefix that needs to be stripped.
func SubFS(efs embed.FS, dir string) (fs.FS, error) {
	return fs.Sub(efs, dir)
}
