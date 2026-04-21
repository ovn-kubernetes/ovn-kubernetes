// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"fmt"
	"os"
	"syscall"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var NewManager = ctrl.NewManager

// WatchProfileAndSignalOnChange sets up a controller-runtime Manager to watch for TLS profile changes and triggers a signal
// when one occurs.
func WatchProfileAndSignalOnChange(ctx context.Context, restCfg *rest.Config, config Profile, signal syscall.Signal) error {
	// Don't watch for profile changes if we're not running on OpenShift
	if config.ProfileSpec == nil {
		klog.Info("TLS profile watcher disabled (not running on OpenShift)")
		return nil
	}

	tlsWatcherMgr, err := NewManager(restCfg, ctrl.Options{
		Scheme: NewScheme(),
		Metrics: server.Options{
			BindAddress: "0", // Disable metrics for this manager
		},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create TLS watcher manager: %w", err)
	}

	sendSignal := func() {
		if err := syscall.Kill(os.Getpid(), signal); err != nil {
			klog.Errorf("Failed to send signal: %v", err)
		}
	}

	// Create SecurityProfileWatcher that sends the signal.
	tlsProfileWatcher := &openshifttls.SecurityProfileWatcher{
		Client:                    tlsWatcherMgr.GetClient(),
		InitialTLSProfileSpec:     *config.ProfileSpec,
		InitialTLSAdherencePolicy: config.Adherence,
		OnProfileChange: func(_ context.Context, oldProfile, newProfile configv1.TLSProfileSpec) {
			klog.Infof(
				"TLS security profile changed, sending signal \"%v\" to self. Old: MinVersion=%s Ciphers=%d, New: MinVersion=%s Ciphers=%d",
				signal, oldProfile.MinTLSVersion, len(oldProfile.Ciphers), newProfile.MinTLSVersion, len(newProfile.Ciphers))
			sendSignal()
		},
		OnAdherencePolicyChange: func(_ context.Context, oldTLSAdherencePolicy, newTLSAdherencePolicy configv1.TLSAdherencePolicy) {
			klog.Infof("TLS Adherence policy changed, sending signal \"%v\" to self. Old: %s, New: %s",
				signal, oldTLSAdherencePolicy, newTLSAdherencePolicy)
			sendSignal()
		},
	}

	if err := tlsProfileWatcher.SetupWithManager(tlsWatcherMgr); err != nil {
		return fmt.Errorf("failed to setup TLS profile watcher: %w", err)
	}

	// Start the TLS watcher manager in the background
	go func() {
		klog.Info("Starting TLS security profile watcher")
		if err := tlsWatcherMgr.Start(ctx); err != nil {
			klog.Errorf("TLS watcher manager stopped with error: %v", err)
		}
	}()

	return nil
}
