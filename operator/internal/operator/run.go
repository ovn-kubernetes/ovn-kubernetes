// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(k8sv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// buildCacheOptions restricts the manager's cache for OVNK8sOperator to the
// operator namespace while leaving all other kinds cluster-wide.
func buildCacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&k8sv1alpha1.OVNK8sOperator{}: {
				Namespaces: map[string]cache.Config{
					k8sv1alpha1.OperatorNamespace: {},
				},
			},
		},
	}
}

func newUncachedClient(config *rest.Config, options client.Options) (client.Client, error) {
	return client.New(config, uncachedClientOptions(options))
}

func uncachedClientOptions(options client.Options) client.Options {
	options.Cache = nil
	return options
}

func Run() {
	var enableLeaderElection bool
	var probeAddr string
	var kubeAPIServer string
	var validationTimeout time.Duration
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8181", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&kubeAPIServer, "kube-api-server", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig.")
	flag.DurationVar(&validationTimeout, "validation-timeout", 5*time.Minute, "The timeout for component validation operations.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Get Kubernetes configuration with optional overrides
	config, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	// Override API server host if specified
	if kubeAPIServer != "" {
		config.Host = kubeAPIServer
		setupLog.Info("Using explicit API server address", "host", kubeAPIServer)
	} else if envAPIServer := os.Getenv("KUBE_API_SERVER"); envAPIServer != "" {
		config.Host = envAPIServer
		setupLog.Info("Using API server address from environment", "host", envAPIServer)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:        scheme,
		Cache:         buildCacheOptions(),
		NewClient:     newUncachedClient,
		WebhookServer: nil, // No webhooks are used
		// Metrics server is not used now, so disable it. The operator runs with
		// hostNetwork, so the default :8080 would otherwise bind on the node and
		// collide with host-network workloads that also use :8080.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "bab1fa4e.ovn.org",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.OVNK8sOperatorReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		ValidationTimeout: validationTimeout,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OVNK8sOperator")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
