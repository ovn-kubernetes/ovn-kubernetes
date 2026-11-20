// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"os/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// Run implements api.Runner interface to run commands locally.
type directRunner struct{}

func NewDirectRunner() api.Runner {
	return &directRunner{}
}

// Close satisfies api.Runner. Running commands locally opens nothing that
// outlives a single command, so there is nothing to release.
func (r *directRunner) Close() error {
	return nil
}

func (r *directRunner) Run(command string, args ...string) (string, error) {
	out, err := exec.Command(command, args...).CombinedOutput()
	result := string(out)
	if err != nil {
		return result, fmt.Errorf("command %s %v failed: %w\noutput: %s", command, args, err, result)
	}
	return result, nil
}
