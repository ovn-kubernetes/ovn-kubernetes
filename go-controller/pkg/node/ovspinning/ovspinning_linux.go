//go:build linux
// +build linux

package ovspinning

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"k8s.io/klog/v2"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/utils/cpuset"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// These variables are meant to be used in unit tests
var tickDuration time.Duration = 1 * time.Second
var getOvsVSwitchdPIDFn func() (string, error) = util.GetOvsVSwitchdPID
var getOvsDBServerPIDFn func() (string, error) = util.GetOvsDBServerPID
var featureEnablerFile string = "/etc/openvswitch/enable_dynamic_cpu_affinity"

// Run monitors OVS daemon's processes (ovs-vswitchd and ovsdb-server) and sets their CPU affinity
// masks to that of the current process.
// This feature is enabled by the presence of a non-empty file in the path `/etc/openvswitch/enable_dynamic_cpu_affinity`
// we're passing the podResCli from the caller, so we could support unit-tests
func Run(ctx context.Context, stopCh <-chan struct{}, podResCli podresourcesapi.PodResourcesListerClient) {

	// The file must be present at startup to enable the feature
	isFeatureEnabled, err := isFileNotEmpty(featureEnablerFile)
	if err != nil {
		klog.Warningf("Can't start OVS CPU affinity pinning: %v", err)
		return
	}

	if !isFeatureEnabled {
		klog.Info("OVS CPU affinity pinning disabled")
		return
	}

	klog.Infof("Starting OVS daemon CPU pinning")
	defer klog.Infof("Stopping OVS daemon CPU pinning")

	var fsnotifyEvents chan fsnotify.Event
	var fsnotifyErrors chan error

	// Watch the parent folder, as it's the only way to get events when the file is deleted and recreated.
	fileWatcher, err := createFileWatcherFor(filepath.Dir(featureEnablerFile))
	if err != nil {
		klog.Warningf("Can't create a watcher for %s. Pinning will not stop by deleting it: %v", featureEnablerFile, err)
		fsnotifyEvents = make(chan fsnotify.Event)
		fsnotifyErrors = make(chan error)
	} else {
		fsnotifyEvents = fileWatcher.Events
		fsnotifyErrors = fileWatcher.Errors
		defer fileWatcher.Close()
	}

	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-fsnotifyEvents:
			if !ok {
				continue
			}

			// Since we are watching the entire folder, skip all the events not related to the enabler file
			if event.Name != featureEnablerFile {
				continue
			}

			isFeatureEnabled, err = isFileNotEmpty(featureEnablerFile)
			if err != nil {
				klog.Warningf("Error while reading [%s]: %v", featureEnablerFile, err)
				continue
			}

			if isFeatureEnabled {
				klog.Infof("OVS daemon CPU pinning feature enabled")
			} else {
				klog.Infof("OVS daemon CPU pinning feature NOT enabled")
			}

		case err, ok := <-fsnotifyErrors:
			if ok {
				klog.Errorf("Error watching for file [%s] changes: %s", featureEnablerFile, err)
			}

		case <-stopCh:
			return

		case <-ticker.C:
			if !isFeatureEnabled {
				continue
			}
			cpus, err := getNonPinnedCPUs(ctx, podResCli)
			if err != nil {
				klog.Warningf("Error while trying to get system non pinned CPUs: %v", err)
			}
			err = setOvsVSwitchdCPUAffinity(&cpus)
			if err != nil {
				klog.Warningf("Error while aligning ovs-vswitchd CPUs to current process: %v", err)
			}

			err = setOvsDBServerCPUAffinity(&cpus)
			if err != nil {
				klog.Warningf("Error while aligning ovsdb-server CPUs to current process: %v", err)
			}
		}
	}
}

func createFileWatcherFor(filename string) (*fsnotify.Watcher, error) {
	fileWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create filesystem watcher: %w", err)
	}

	err = fileWatcher.Add(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to watch [%s] file: %w", filename, err)
	}

	return fileWatcher, nil
}

func isFileNotEmpty(filename string) (bool, error) {
	f, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("can't get file information [%s]: %w", filename, err)
	}

	// get the size
	return f.Size() > 0, nil
}

func setOvsVSwitchdCPUAffinity(set *cpuset.CPUSet) error {

	ovsVSwitchdPID, err := getOvsVSwitchdPIDFn()
	if err != nil {
		return fmt.Errorf("can't retrieve ovs-vswitchd PID: %w", err)
	}

	klog.V(5).Infof("Managing ovs-vswitchd[%s] daemon CPU affinity", ovsVSwitchdPID)
	return setProcessCPUAffinity(ovsVSwitchdPID, set)
}

func setOvsDBServerCPUAffinity(set *cpuset.CPUSet) error {

	ovsDBserverPID, err := getOvsDBServerPIDFn()
	if err != nil {
		return fmt.Errorf("can't retrieve ovsdb-server PID: %w", err)
	}

	klog.V(5).Infof("Managing ovsdb-server[%s] daemon CPU affinity", ovsDBserverPID)
	return setProcessCPUAffinity(ovsDBserverPID, set)
}

// setProcessCPUAffinity sets the CPU affinity of a target process and all its threads
// to the specified CPU set. If the provided CPU set is empty, it falls back to using
// the current process's CPU affinity as the desired affinity.
//
// The function operates at the thread level, iterating through all threads (tasks)
// of the target process and setting their individual CPU affinities. This ensures
// that both the main process and any spawned threads are properly pinned to the
// specified CPUs.
//
// Parameters:
//   - targetPIDStr: string representation of the target process ID
//   - set: pointer to the desired CPU set; if empty, current process affinity is used
//
// Returns:
//   - error: any error encountered during PID conversion, affinity retrieval, or setting
//
// The function skips setting affinity if the target process already has the desired
// CPU affinity. Individual thread affinity setting failures are logged as warnings
// but don't stop the overall operation.
func setProcessCPUAffinity(targetPIDStr string, set *cpuset.CPUSet) error {

	targetPID, err := strconv.Atoi(targetPIDStr)
	if err != nil {
		return fmt.Errorf("can't convert PID[%s] to integer: %w", targetPIDStr, err)
	}

	desiredProcessCPUs := convertCPUSet(set)
	if set.IsEmpty() {
		selfPID := os.Getpid()
		klog.InfoS("Given CPU set is empty, setting self CPU affinity", "selfPID", selfPID, "targetPID", targetPID)
		err = unix.SchedGetaffinity(selfPID, &desiredProcessCPUs)
		if err != nil {
			return fmt.Errorf("can't get own CPU affinity")
		}
	}

	var targetProcessCPUs unix.CPUSet
	err = unix.SchedGetaffinity(targetPID, &targetProcessCPUs)
	if err != nil {
		return fmt.Errorf("can't get process (PID:%d) CPU affinity: %w", targetPID, err)
	}

	if desiredProcessCPUs == targetProcessCPUs {
		klog.V(5).Infof("Process[%d] CPU affinity already match desired process's affinity %s", targetPID, printCPUSet(desiredProcessCPUs))
		return nil
	}

	taskIDs, err := getThreadsOfProcess(targetPID)
	if err != nil {
		return fmt.Errorf("can't get tasks of PID(%d):%w", targetPID, err)
	}

	klog.Infof("Setting CPU affinity of PID(%d) (ntasks=%d) to %s, was %s", targetPID, len(taskIDs), printCPUSet(desiredProcessCPUs), printCPUSet(targetProcessCPUs))
	for _, taskID := range taskIDs {
		err = unix.SchedSetaffinity(taskID, &desiredProcessCPUs)
		if err != nil {
			// The task may have been stopped, don't break the loop and continue setting CPU affinity on other tasks.
			klog.Warningf("Error while setting CPU affinity of task(%d) PID(%d) to %s: %v", taskID, targetPID, printCPUSet(desiredProcessCPUs), err)
		}
	}

	return nil
}

// printCPUSet takes a unix.CPUSet and returns a string representation in canonical linux CPU list format.
// e.g. 0-5,8,10,12-3
//
// See http://man7.org/linux/man-pages/man7/cpuset.7.html#FORMATS
func printCPUSet(cpus unix.CPUSet) string {

	type rng struct {
		start int
		end   int
	}

	// Start with a fake range to avoid going out of range while looping
	ranges := []rng{{-2, -2}}

	// There is no public API to know the length of unix.CPUSet, so this counter is the
	// stopping condition for the loop
	remainingSetsCpus := cpus.Count()

	for i := 0; remainingSetsCpus > 0; i++ {
		if !cpus.IsSet(i) {
			continue
		}

		remainingSetsCpus--

		lastRange := ranges[len(ranges)-1]
		if lastRange.end == i-1 {
			ranges[len(ranges)-1].end++
		} else {
			ranges = append(ranges, rng{start: i, end: i})
		}
	}

	var result bytes.Buffer
	// discard the fake range with [1:]
	for _, r := range ranges[1:] {
		if r.start == r.end {
			result.WriteString(strconv.Itoa(r.start))
		} else {
			result.WriteString(fmt.Sprintf("%d-%d", r.start, r.end))
		}
		result.WriteString(",")
	}
	return strings.TrimRight(result.String(), ",")
}

// getThreadsOfProcess returns the list of thread IDs of the given process
func getThreadsOfProcess(pid int) ([]int, error) {
	taskFolders, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, fmt.Errorf("unable to find %d tasks: %v", pid, err)
	}

	ret := []int{}
	for _, taskFolder := range taskFolders {
		taskID, err := strconv.Atoi(taskFolder.Name())
		if err != nil {
			return nil, fmt.Errorf("unable to get task ID of %d: %s, %v", pid, taskFolder.Name(), err)
		}

		ret = append(ret, taskID)
	}

	return ret, nil
}

func convertCPUSet(k8sSet *cpuset.CPUSet) unix.CPUSet {
	var uSet unix.CPUSet
	for _, cpu := range k8sSet.List() {
		uSet.Set(cpu)
	}
	return uSet
}

// getNonPinnedCPUs calculates and returns all allocatable CPUs on the node which are not
// exclusively pinned to any container. IOW it returns the CPUs that are dedicated for
// Burstable and BestEffort QoS containers
func getNonPinnedCPUs(ctx context.Context, podResCli podresourcesapi.PodResourcesListerClient) (cpuset.CPUSet, error) {
	// Get allocatable CPUs
	allocatableResp, err := podResCli.GetAllocatableResources(ctx, &podresourcesapi.AllocatableResourcesRequest{})
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("GetAllocatableResources failed: %w", err)
	}
	allocatableCPUs := cpuset.New(convertInt64ToInt(allocatableResp.CpuIds)...)

	// List pod resources and collect used CPUs
	listCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	listResp, err := podResCli.List(listCtx, &podresourcesapi.ListPodResourcesRequest{})
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("ListPodResources failed: %w", err)
	}

	usedCPUs := cpuset.New()
	for _, pod := range listResp.PodResources {
		for _, container := range pod.Containers {
			usedCPUs = usedCPUs.Union(cpuset.New(convertInt64ToInt(container.CpuIds)...))
		}
	}

	// Calculate the difference
	availableCPUs := allocatableCPUs.Difference(usedCPUs)
	return availableCPUs, nil
}

func convertInt64ToInt(int64s []int64) []int {
	ints := make([]int, len(int64s))
	for i, v := range int64s {
		ints[i] = int(v)
	}
	return ints
}
