/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2020 Red Hat, Inc.
 *
 */

package kubevirt

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

type Client struct {
	path string
}

func NewClient(cliDir string) (*Client, error) {
	// Ensure the virtctl directory exists.
	if err := os.MkdirAll(cliDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create virtctl directory: %w", err)
	}

	// Ensure the virtctl executable is present.
	if err := ensureVirtctl(cliDir); err != nil {
		return nil, fmt.Errorf("failed to ensure virtctl: %w", err)
	}

	return &Client{path: filepath.Join(cliDir, "virtctl")}, nil
}

func (virtctl *Client) RestartVirtualMachine(vmi *kubevirtv1.VirtualMachineInstance) (string, error) {
	output, err := exec.Command(virtctl.path, "restart", "-n", vmi.Namespace, vmi.Name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to restart VM: %w", err)
	}
	return string(output), nil
}

func ensureVirtctl(cliDir string) error {
	// Check if the "virtctl" executable exists in the specified path.
	// If it does not exist, call the installVirtctl function.
	if _, err := os.Stat(filepath.Join(cliDir, "virtctl")); os.IsNotExist(err) {
		return installVirtctl(cliDir)
	} else if err != nil {
		return fmt.Errorf("error checking virtctl executable: %w", err)
	}
	return nil
}

func installVirtctl(cliDir string) error {
	// Fetch the latest stable version of KubeVirt from the stable.txt file.
	stableResp, err := http.Get("https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt")
	if err != nil {
		return fmt.Errorf("failed to fetch stable version: %w", err)
	}
	defer stableResp.Body.Close()

	// Check if the HTTP response status is OK.
	if stableResp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch stable version: received status code %d", stableResp.StatusCode)
	}

	// Read the version from the response body.
	versionBytes, err := io.ReadAll(stableResp.Body)
	if err != nil {
		return fmt.Errorf("failed to read stable version: %w", err)
	}
	version := strings.TrimSpace(string(versionBytes))

	// Construct the download URL for the virtctl binary.
	virtctlURL := fmt.Sprintf("https://github.com/kubevirt/kubevirt/releases/download/%s/virtctl-%s-linux-amd64", version, version)

	// Download the virtctl binary.
	virtctlResp, err := http.Get(virtctlURL)
	if err != nil {
		return fmt.Errorf("failed to download virtctl: %2", err)
	}
	defer virtctlResp.Body.Close()

	// Check if the HTTP response status is OK.
	if virtctlResp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download virtctl: received status code %d", virtctlResp.StatusCode)
	}

	// Save the binary to the specified directory.
	cliPath := filepath.Join(cliDir, "virtctl")
	outFile, err := os.Create(cliPath)
	if err != nil {
		return fmt.Errorf("failed to create virtctl file at %s: %w", cliPath, err)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, virtctlResp.Body)
	if err != nil {
		return fmt.Errorf("failed to save virtctl binary to %s: %w", cliPath, err)
	}

	// Make the binary executable.
	if err := os.Chmod(cliPath, 0755); err != nil {
		return fmt.Errorf("failed to make virtctl executable at %s: %w", cliPath, err)
	}

	return nil
}
