// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
)

// recordingRunner records every command it is asked to run and always succeeds.
type recordingRunner struct {
	commands []string
	closed   int
	closeErr error
}

func (r *recordingRunner) Run(command string, args ...string) (string, error) {
	r.commands = append(r.commands, strings.Join(append([]string{command}, args...), " "))
	return "", nil
}

func (r *recordingRunner) Close() error {
	r.closed++
	return r.closeErr
}

// plainRunner has no Close, so it stands in for runners that hold nothing to
// release.
type plainRunner struct{}

func (plainRunner) Run(_ string, _ ...string) (string, error) { return "", nil }

func newTestKind(t *testing.T, runtime containerRuntime, cmdRunner api.Runner) *kind {
	t.Helper()
	return &kind{
		engine:         container.NewEngine(runtime.String(), cmdRunner),
		runtime:        runtime,
		runner:         cmdRunner,
		primaryNetwork: DefaultPrimaryNetwork,
		HostPort:       portalloc.New(1024, 65535),
	}
}

// TestPreloadImagesRunsEveryStepThroughTheRunner is the property that lets one
// provider serve both a local and a remote container runtime: no step may reach
// for the local machine behind the runner's back.
func TestPreloadImagesRunsEveryStepThroughTheRunner(t *testing.T) {
	for _, tc := range []struct {
		runtime containerRuntime
		want    []string
	}{
		{
			runtime: docker,
			want: []string{
				"docker pull ovn-daemonset:pr",
				"kind load docker-image ovn-daemonset:pr --name ovn",
			},
		},
		{
			runtime: podman,
			want: []string{
				"podman pull ovn-daemonset:pr",
				"rm -f " + imageArchivePath,
				"podman save -o " + imageArchivePath + " ovn-daemonset:pr",
				"kind load image-archive " + imageArchivePath + " --name ovn",
			},
		},
	} {
		t.Run(tc.runtime.String(), func(t *testing.T) {
			cmdRunner := &recordingRunner{}
			newTestKind(t, tc.runtime, cmdRunner).preloadImages("ovn", []string{"ovn-daemonset:pr"})
			if !reflect.DeepEqual(cmdRunner.commands, tc.want) {
				t.Fatalf("commands:\n got %q\nwant %q", cmdRunner.commands, tc.want)
			}
		})
	}
}

func TestCloseReleasesTheRunner(t *testing.T) {
	cmdRunner := &recordingRunner{closeErr: errors.New("boom")}
	k := newTestKind(t, docker, cmdRunner)
	if err := k.Close(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Close: got %v, want the runner's error", err)
	}
	if cmdRunner.closed != 1 {
		t.Fatalf("runner closed %d times, want 1", cmdRunner.closed)
	}
}

func TestCloseWithoutClosableRunner(t *testing.T) {
	if err := newTestKind(t, docker, plainRunner{}).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestParseContainerRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		env     string
		want    containerRuntime
		wantErr bool
	}{
		{name: "empty falls back to docker", want: docker},
		{name: "empty reads the environment", env: "podman", want: podman},
		{name: "explicit beats the environment", in: "docker", env: "podman", want: docker},
		{name: "case insensitive", in: "PODMAN", want: podman},
		{name: "unknown runtime", in: "containerd", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvContainerRuntime, tc.env)
			got, err := parseContainerRuntime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be rejected", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseContainerRuntime(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseContainerRuntime(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
