// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv gives each test a known environment, whatever the developer shell has
// exported.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{EnvHost, EnvUser, EnvPort, EnvKey, EnvPrimaryNetwork, EnvContainerRuntime,
		EnvKnownHosts, EnvInsecureHostKey} {
		t.Setenv(key, "")
	}
}

func TestConfigFromEnvAppliesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvHost, "runtime.example.com")
	t.Setenv(EnvUser, "tester")
	t.Setenv(EnvKey, "/home/tester/.ssh/id_ed25519")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Port != DefaultPort || cfg.Runtime != DefaultRuntime || cfg.PrimaryNetwork != DefaultPrimaryNetwork {
		t.Fatalf("defaults not applied: port %q, runtime %q, primary network %q",
			cfg.Port, cfg.Runtime, cfg.PrimaryNetwork)
	}
	if cfg.InsecureHostKey {
		t.Fatal("host key verification must be on unless explicitly disabled")
	}
}

func TestConfigFromEnvReadsOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvHost, "10.0.0.5")
	t.Setenv(EnvUser, "tester")
	t.Setenv(EnvKey, "/keys/id_ed25519")
	t.Setenv(EnvPort, "2222")
	t.Setenv(EnvContainerRuntime, "PODMAN")
	t.Setenv(EnvPrimaryNetwork, "ovn")
	t.Setenv(EnvKnownHosts, "/keys/known_hosts")
	t.Setenv(EnvInsecureHostKey, "1")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Port != "2222" || cfg.Runtime != "podman" || cfg.PrimaryNetwork != "ovn" {
		t.Fatalf("overrides not read: %+v", cfg)
	}
	if cfg.KnownHostsPath != "/keys/known_hosts" || !cfg.InsecureHostKey {
		t.Fatalf("host key options not read: %+v", cfg)
	}
}

func TestConfigFromEnvReportsEveryMissingRequirement(t *testing.T) {
	clearEnv(t)

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected an error when no required variable is set")
	}
	for _, want := range []string{EnvHost, EnvUser, EnvKey} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestConfigFromEnvExpandsKeyPathHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	clearEnv(t)
	t.Setenv(EnvHost, "10.0.0.5")
	t.Setenv(EnvUser, "tester")
	t.Setenv(EnvKey, "~/.ssh/id_ed25519")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if want := filepath.Join(home, ".ssh", "id_ed25519"); cfg.PrivateKeyPath != want {
		t.Fatalf("private key path: got %q, want %q", cfg.PrivateKeyPath, want)
	}
}

func TestConfigValidateRejectsBadValues(t *testing.T) {
	base := Config{Host: "10.0.0.5", User: "tester", PrivateKeyPath: "/keys/id_ed25519"}
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"host with port", func(c *Config) { c.Host = "10.0.0.5:2222" }, EnvPort},
		{"host with scheme", func(c *Config) { c.Host = "ssh://10.0.0.5" }, "no scheme"},
		{"bracketed host", func(c *Config) { c.Host = "[fd00::1]" }, "no brackets"},
		{"port out of range", func(c *Config) { c.Port = "70000" }, EnvPort},
		{"non numeric port", func(c *Config) { c.Port = "ssh" }, EnvPort},
		{"unknown runtime", func(c *Config) { c.Runtime = "containerd" }, EnvContainerRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			cfg.applyDefaults()
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %+v to be rejected", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfigValidateAcceptsIPv6Host(t *testing.T) {
	cfg := Config{Host: "fd00::1", User: "tester", PrivateKeyPath: "/keys/id_ed25519"}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestIsLocalHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{" LocalHost ", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", false},
		{"host.example", false},
	} {
		if got := IsLocalHost(tc.host); got != tc.want {
			t.Fatalf("IsLocalHost(%q): got %t, want %t", tc.host, got, tc.want)
		}
	}
}
