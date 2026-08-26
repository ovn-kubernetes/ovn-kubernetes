// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Environment variables consumed by ConfigFromEnv.
const (
	// EnvHost is the SSH host (IP or resolvable name) of the machine running the
	// container runtime. Required.
	EnvHost = "OVN_TEST_SSH_HOST"
	// EnvUser is the SSH user. Required.
	EnvUser = "OVN_TEST_SSH_USER"
	// EnvPort is the SSH port. Optional, defaults to DefaultPort.
	EnvPort = "OVN_TEST_SSH_PORT"
	// EnvKey is the path to the SSH private key file used for public-key auth.
	// Required.
	EnvKey = "OVN_TEST_SSH_KEY"
	// EnvPrimaryNetwork overrides the name of the cluster's primary container
	// network. Optional, defaults to DefaultPrimaryNetwork.
	EnvPrimaryNetwork = "OVN_TEST_PRIMARY_NETWORK"
	// EnvContainerRuntime selects the container runtime (docker|podman). Optional,
	// defaults to DefaultRuntime. Reused from the kind provider convention so a
	// single variable controls the runtime for both providers.
	EnvContainerRuntime = "CONTAINER_RUNTIME"
	// EnvKnownHosts is the path to an SSH known_hosts file used for host-key
	// verification. Optional; when unset the runner falls back to ~/.ssh/known_hosts
	// if present.
	EnvKnownHosts = "OVN_TEST_SSH_KNOWN_HOSTS"
	// EnvInsecureHostKey opts out of host-key verification (test-only).
	EnvInsecureHostKey = "OVN_TEST_SSH_INSECURE_HOST_KEY"
)

// Defaults for optional configuration.
const (
	DefaultPort           = "22"
	DefaultRuntime        = "docker"
	DefaultPrimaryNetwork = "kind"
)

// Config holds everything the SSH provider needs. The e2e suite fills it from
// environment variables (ConfigFromEnv); other callers can populate the same
// struct from their own sources.
type Config struct {
	// Host is the SSH host (IP or name) of the machine running the container
	// runtime.
	Host string
	// User is the SSH user.
	User string
	// Port is the SSH port (string, to match the underlying runner API).
	Port string
	// PrivateKeyPath is the filesystem path to the SSH private key.
	PrivateKeyPath string
	// Runtime is the container runtime to drive on the remote host, "docker" or
	// "podman".
	Runtime string
	// PrimaryNetwork is the name of the cluster's primary container network
	// (returned by PrimaryNetwork). Defaults to "kind".
	PrimaryNetwork string
	// KnownHostsPath is the SSH known_hosts file for host-key verification.
	KnownHostsPath string
	// InsecureHostKey disables SSH host-key verification when true.
	InsecureHostKey bool
	// ImagePreloader, if set, makes required images available to the cluster
	// nodes before the suite runs; Provider.PreloadImages delegates to it. When
	// nil, nothing is preloaded and the images must already be available.
	ImagePreloader func(images []string) error
}

// ConfigFromEnv builds a Config from environment variables and validates it.
// Required: OVN_TEST_SSH_HOST, OVN_TEST_SSH_USER, OVN_TEST_SSH_KEY.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Host:            strings.TrimSpace(os.Getenv(EnvHost)),
		User:            strings.TrimSpace(os.Getenv(EnvUser)),
		Port:            strings.TrimSpace(os.Getenv(EnvPort)),
		PrivateKeyPath:  strings.TrimSpace(os.Getenv(EnvKey)),
		Runtime:         strings.ToLower(strings.TrimSpace(os.Getenv(EnvContainerRuntime))),
		PrimaryNetwork:  strings.TrimSpace(os.Getenv(EnvPrimaryNetwork)),
		KnownHostsPath:  strings.TrimSpace(os.Getenv(EnvKnownHosts)),
		InsecureHostKey: strings.TrimSpace(os.Getenv(EnvInsecureHostKey)) == "1",
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyDefaults normalizes fields (so direct Config consumers get the same
// treatment as ConfigFromEnv) and fills optional fields that were left empty.
func (c *Config) applyDefaults() {
	c.Host = strings.TrimSpace(c.Host)
	c.User = strings.TrimSpace(c.User)
	c.Port = strings.TrimSpace(c.Port)
	c.PrivateKeyPath = expandHome(strings.TrimSpace(c.PrivateKeyPath))
	c.Runtime = strings.ToLower(strings.TrimSpace(c.Runtime))
	c.PrimaryNetwork = strings.TrimSpace(c.PrimaryNetwork)
	if c.Port == "" {
		c.Port = DefaultPort
	}
	if c.Runtime == "" {
		c.Runtime = DefaultRuntime
	}
	if c.PrimaryNetwork == "" {
		c.PrimaryNetwork = DefaultPrimaryNetwork
	}
}

// expandHome expands a leading "~/" (or a bare "~") in path to the invoking
// user's home directory, so common forms like OVN_TEST_SSH_KEY=~/.ssh/id_ed25519
// work (the SSH runner passes the value straight to os.ReadFile, which does not
// expand "~"). Other forms (e.g. "~otheruser") are intentionally left untouched.
// If the home directory cannot be resolved, path is returned unchanged so a later
// read surfaces a clear error rather than a silently-wrong path.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[len("~/"):])
}

// checkRuntime validates the container runtime before it is used to construct
// the container engine.
func (c *Config) checkRuntime() error {
	switch c.Runtime {
	case "docker", "podman":
		return nil
	default:
		return fmt.Errorf("%s must be \"docker\" or \"podman\", got %q", EnvContainerRuntime, c.Runtime)
	}
}

// Validate checks that required fields are present and optional fields are sane.
// It aggregates all problems into a single error so callers see everything at
// once. Note: applyDefaults must have run (ConfigFromEnv and New both ensure
// this) so that Port/Runtime are populated.
func (c *Config) Validate() error {
	var problems []string
	if c.Host == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH host) is required", EnvHost))
	} else if err := validateSSHHost(c.Host); err != nil {
		problems = append(problems, err.Error())
	}
	if c.User == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH user) is required", EnvUser))
	}
	if c.PrivateKeyPath == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH private key path) is required", EnvKey))
	}
	if c.Port != "" {
		if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
			problems = append(problems, fmt.Sprintf("%s must be a valid TCP port (1-65535), got %q", EnvPort, c.Port))
		}
	}
	if err := c.checkRuntime(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid ssh provider configuration: %s", strings.Join(problems, "; "))
}

// validateSSHHost rejects values that are not a bare hostname or IP. The SSH port
// belongs in OVN_TEST_SSH_PORT / Config.Port, not embedded in the host string.
func validateSSHHost(host string) error {
	if strings.ContainsFunc(host, unicode.IsSpace) {
		return fmt.Errorf("%s must be a bare host or IP (no whitespace), got %q", EnvHost, host)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("%s must be a bare host or IP (no scheme), got %q", EnvHost, host)
	}
	if strings.HasPrefix(host, "[") {
		return fmt.Errorf("%s must be a bare host or IP (no brackets), got %q", EnvHost, host)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.Contains(host, ":") {
		if hostHasNumericPortSuffix(host) {
			return fmt.Errorf("%s must not include a port (use %s), got %q", EnvHost, EnvPort, host)
		}
		return fmt.Errorf("%s must be a bare host or IP, got %q", EnvHost, host)
	}
	return nil
}

func hostHasNumericPortSuffix(host string) bool {
	if strings.Count(host, ":") != 1 {
		return false
	}
	_, portPart, found := strings.Cut(host, ":")
	if !found || portPart == "" {
		return true
	}
	p, err := strconv.Atoi(portPart)
	return err == nil && p >= 1 && p <= 65535
}

// IsLocalHost reports whether host refers to the local machine's loopback.
// Used by the e2e suite to decide when KinD image preload is safe with the SSH
// provider (preload must target the same daemon the SSH session uses).
func IsLocalHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
