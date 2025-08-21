//go:build linux
// +build linux

package ovspinning

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

func TestAlignCPUAffinity(t *testing.T) {
	ovsDBPid, ovsDBStop := mockOvsdbProcess(t)
	defer ovsDBStop()

	ovsVSwitchdPid, ovsVSwitchdStop := mockOvsVSwitchdProcess(t)
	defer ovsVSwitchdStop()

	defer setTickDuration(20 * time.Millisecond)()
	defer mockFeatureEnableFile(t, "1")()

	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	defer func() {
		close(stopCh)
		wg.Wait()
	}()

	wg.Add(1)
	go func() {
		// Be sure the system under test goroutine is finished before cleaning
		defer wg.Done()
		Run(context.TODO(), stopCh)
	}()

	var initialCPUset unix.CPUSet
	err := unix.SchedGetaffinity(os.Getpid(), &initialCPUset)
	require.NoError(t, err)

	defer func() {
		// Restore any previous CPU affinity value it was in place before the test
		err = unix.SchedSetaffinity(os.Getpid(), &initialCPUset)
		assert.NoError(t, err)
	}()

	assert.Greater(t, runtime.NumCPU(), 1)

	for i := 0; i < runtime.NumCPU(); i++ {
		var tmpCPUset unix.CPUSet
		tmpCPUset.Set(i)
		err = unix.SchedSetaffinity(os.Getpid(), &tmpCPUset)
		require.NoError(t, err)

		klog.Infof("Test CPU Affinity %x", tmpCPUset)

		assertPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
		assertPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)
	}

	// Disable the feature by making the enabler file empty
	err = os.WriteFile(featureEnablerFile, []byte(""), 0)
	require.NoError(t, err)

	var tmpCPUset unix.CPUSet
	tmpCPUset.Set(0)
	err = unix.SchedSetaffinity(os.Getpid(), &tmpCPUset)
	require.NoError(t, err)

	assertNeverPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
	assertNeverPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)

	// Enable the feature back by putting contents in the enabler file
	err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
	require.NoError(t, err)

	assertPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
	assertPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)

	// Disable the feature by deleting the enabler file
	klog.Infof("Remove the enabler file to disable the feature")
	err = os.Remove(featureEnablerFile)
	require.NoError(t, err)

	tmpCPUset.Set(1)
	err = unix.SchedSetaffinity(os.Getpid(), &tmpCPUset)
	require.NoError(t, err)

	assertNeverPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
	assertNeverPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)

	// Re-enable the feature back by recreating the enabler file
	klog.Infof("Re-enable the feature")
	err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
	require.NoError(t, err)

	assertPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
	assertPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)
}

func TestIsFileNotEmpty(t *testing.T) {
	defer mockFeatureEnableFile(t, "")()

	result, err := isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.False(t, result)

	err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
	require.NoError(t, err)
	result, err = isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.True(t, result)

	os.Remove(featureEnablerFile)
	result, err = isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestPrintCPUSetAll(t *testing.T) {
	var x unix.CPUSet
	for i := 0; i < 16; i++ {
		x.Set(i)
	}

	assert.Equal(t,
		"0-15",
		printCPUSet(x),
	)

	assert.Empty(t,
		printCPUSet(unix.CPUSet{}),
	)
}

func TestPrintCPUSetRanges(t *testing.T) {
	var x unix.CPUSet

	x.Set(2)
	x.Set(3)
	x.Set(6)
	x.Set(7)
	x.Set(8)
	x.Set(14)

	assert.Equal(t,
		"2-3,6-8,14",
		printCPUSet(x),
	)
}

func TestGetReservedCPUs(t *testing.T) {
	tests := []struct {
		name            string
		yamlContent     string
		expectError     bool
		expectedCPUSet  string
		expectedIsEmpty bool
	}{
		{
			name: "valid config",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: "0-1,3"
`,
			expectError:    false,
			expectedCPUSet: "0-1,3",
		},
		{
			name: "empty reservedSystemCPUs",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
`,
			expectError:     false,
			expectedIsEmpty: true,
		},
		{
			name: "invalid cpuset string",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: "not-a-valid-range"
`,
			expectError: true,
		},
		{
			name: "invalid YAML format",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: [0,1
`,
			expectError: true,
		},
		{
			name:        "file not found",
			yamlContent: "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.name == "file not found" {
				path = "/nonexistent/path.yaml"
			} else {
				path = writeTempFile(t, tt.yamlContent)
			}

			cset, err := getReservedCPUs(path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.expectedIsEmpty && !cset.IsEmpty() {
				t.Errorf("expected empty cpuset, got %s", cset.String())
			} else if tt.expectedCPUSet != "" {
				expected, _ := cpuset.Parse(tt.expectedCPUSet)
				if !cset.Equals(expected) {
					t.Errorf("expected cpuset %s, got %s", expected.String(), cset.String())
				}
			}
		})
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "kubelet-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

func mockOvsdbProcess(t *testing.T) (int, func()) {
	t.Helper()
	ctx, stopCmd := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "sleep", "10")

	err := cmd.Start()
	assert.NoError(t, err)

	previousGetter := getOvsDBServerPIDFn
	getOvsDBServerPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", cmd.Process.Pid), nil
	}

	return cmd.Process.Pid, func() {
		stopCmd()
		getOvsDBServerPIDFn = previousGetter
	}
}

func mockOvsVSwitchdProcess(t *testing.T) (int, func()) {
	t.Helper()
	ctx, stopCmd := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "go", "run", "testdata/fake_thread_process.go")
	err := cmd.Start()
	require.NoError(t, err)

	previousGetter := getOvsVSwitchdPIDFn
	getOvsVSwitchdPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", cmd.Process.Pid), nil
	}

	// Ensure the fake process has some thread
	assert.Eventually(t, func() bool {
		tasks, err := getThreadsOfProcess(cmd.Process.Pid)
		assert.NoError(t, err)
		return len(tasks) > 1
	}, time.Second, 100*time.Millisecond, "ovs-vswitchd fake process does not have enough threads")

	return cmd.Process.Pid, func() {
		stopCmd()
		getOvsVSwitchdPIDFn = previousGetter
	}
}

func setTickDuration(d time.Duration) func() {
	previousValue := tickDuration
	tickDuration = d

	return func() {
		tickDuration = previousValue
	}
}

func mockFeatureEnableFile(t *testing.T, data string) func() {
	t.Helper()
	f, err := os.CreateTemp("", "enable_dynamic_cpu_affinity")
	require.NoError(t, err)

	previousValue := featureEnablerFile
	featureEnablerFile = f.Name()

	err = os.WriteFile(featureEnablerFile, []byte(data), 0)
	assert.NoError(t, err)

	return func() {
		featureEnablerFile = previousValue
		os.Remove(f.Name())
	}
}

func assertPIDHasSchedAffinity(t *testing.T, pid int, expectedCPUSet unix.CPUSet) {
	t.Helper()
	var actual unix.CPUSet
	assert.Eventually(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)

		return actual == expectedCPUSet
	}, time.Second, 10*time.Millisecond, "pid[%d] Expected CPUSet %0x != Actual CPUSet %0x", pid, expectedCPUSet, actual)

	tasks, err := getThreadsOfProcess(pid)
	require.NoError(t, err)

	for _, task := range tasks {
		assert.Eventually(t, func() bool {
			err := unix.SchedGetaffinity(task, &actual)
			assert.NoError(t, err)

			return actual == expectedCPUSet
		}, time.Second, 10*time.Millisecond, "task[%d] of process[%d]  Expected CPUSet %0x != Actual CPUSet %0x", task, pid, expectedCPUSet, actual)
	}
}

func assertNeverPIDHasSchedAffinity(t *testing.T, pid int, targetCPUSet unix.CPUSet) {
	t.Helper()
	var actual unix.CPUSet
	assert.Never(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)

		return actual == targetCPUSet
	}, time.Second, 10*time.Millisecond, "pid[%d]  == Actual CPUSet %0x expected to be different than %0x", pid, actual, targetCPUSet)
}
