// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dra

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/stub"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

const (
	driverName         = "k8s.ovn.org"
	maxAttempts        = 10
	stabilityThreshold = 5 * time.Minute
)

// AllocatedDevice represents a network device that has been allocated to a pod.
type AllocatedDevice struct {
	Name       string
	Attributes map[string]string
	PoolName   string
	Request    string
}

// SharedState is shared between DRA hooks and NRI hooks.
type SharedState struct {
	PodDeviceConfig map[types.UID][]AllocatedDevice
	PreparedData    map[types.UID]interface{}
}

// NetworkDriver manages lifecycle of DRA and NRI plugins for one node.
type NetworkDriver struct {
	driverName string
	nodeName   string
	kubeClient kubernetes.Interface
	draPlugin  *kubeletplugin.Helper
	nriPlugin  stub.Stub

	mu          sync.Mutex
	sharedState *SharedState
}

// New creates a DRA controller for a node.
func New(nodeName string, kubeClient kubernetes.Interface) (*NetworkDriver, error) {
	return &NetworkDriver{
		driverName: driverName,
		nodeName:   nodeName,
		kubeClient: kubeClient,
		sharedState: &SharedState{
			PodDeviceConfig: make(map[types.UID][]AllocatedDevice),
			PreparedData:    make(map[types.UID]interface{}),
		},
	}, nil
}

// Start initializes and runs DRA and NRI plugins.
func (k *NetworkDriver) Start(ctx context.Context) error {
	driverPluginPath := filepath.Join(kubeletplugin.KubeletPluginsDir, k.driverName)
	if err := os.MkdirAll(driverPluginPath, 0o750); err != nil {
		return fmt.Errorf("failed to create plugin path %s: %w", driverPluginPath, err)
	}

	kubeletOptions := []kubeletplugin.Option{
		kubeletplugin.DriverName(k.driverName),
		kubeletplugin.NodeName(k.nodeName),
		kubeletplugin.KubeClient(k.kubeClient),
	}
	draHelper, err := kubeletplugin.Start(ctx, k, kubeletOptions...)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	k.draPlugin = draHelper

	if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
		status := k.draPlugin.RegistrationStatus()
		return status != nil && status.PluginRegistered, nil
	}); err != nil {
		return err
	}

	nriOptions := []stub.Option{
		stub.WithPluginName(k.driverName),
		stub.WithPluginIdx("10"),
		stub.WithOnClose(func() { klog.Infof("%s NRI plugin closed", k.driverName) }),
	}
	nriStub, err := stub.New(k, nriOptions...)
	if err != nil {
		return fmt.Errorf("failed to create NRI plugin stub: %w", err)
	}
	k.nriPlugin = nriStub

	go k.runNRIPlugin(ctx)
	go k.publishResources(ctx)

	return nil
}

// Stop gracefully stops DRA and NRI plugins.
func (k *NetworkDriver) Stop() {
	if k.nriPlugin != nil {
		k.nriPlugin.Stop()
	}
	if k.draPlugin != nil {
		k.draPlugin.Stop()
	}
}

func (k *NetworkDriver) runNRIPlugin(ctx context.Context) {
	attempt := 0
	for attempt < maxAttempts {
		startTime := time.Now()
		if err := k.nriPlugin.Run(ctx); err != nil {
			klog.Errorf("NRI plugin failed: %v", err)
		}

		if time.Since(startTime) > stabilityThreshold {
			attempt = 0
		} else {
			attempt++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			klog.Infof("Restarting NRI plugin (attempt %d/%d)", attempt, maxAttempts)
		}
	}
	klog.Fatalf("NRI plugin failed to restart after %d attempts", maxAttempts)
}
