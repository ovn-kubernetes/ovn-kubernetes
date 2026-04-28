// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/scheme"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovnwebhook"
)

var logger = klog.NewKlogr()

// Config contains configuration for the webhook server
type Config struct {
	EnableInterconnect      bool
	EnableHybridOverlay     bool
	ExtraAllowedUsers       []string
	CSRAcceptanceConditions []csrapprover.CSRAcceptanceCondition
	PodAdmissionConditions  []ovnwebhook.PodAdmissionConditionOption
	CertDir                 string
	Host                    string
	Port                    int
}

// Run starts the webhook server
func Run(ctx context.Context, restCfg *rest.Config, config Config) error {
	// We cannot use the default implementation of the webhook server because we need to enable SO_REUSEPORT
	// on the socket to allow for two instances running at the same time (required during upgrades).
	// The webhook server is set up and started in a very similar way to the default one:
	// https://github.com/ovn-kubernetes/ovn-kubernetes/blob/7c0838bb46d6de202f509abe47609c8da09311b2/go-controller/vendor/sigs.k8s.io/controller-runtime/pkg/webhook/server.go#L212

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("error creating clientset: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	webhookMux := http.NewServeMux()

	nodeWebhook := admission.WithValidator(
		scheme.Scheme,
		ovnwebhook.NewNodeAdmissionWebhook(config.EnableInterconnect, config.EnableHybridOverlay, config.ExtraAllowedUsers...),
	).WithRecoverPanic(true)

	nodeHandler, err := admission.StandaloneWebhook(
		nodeWebhook,
		admission.StandaloneOptions{
			Logger:      logger.WithName("node.network-identity"),
			MetricsPath: "node.network-identity",
		},
	)
	if err != nil {
		return fmt.Errorf("failed to setup the node admission webhook: %w", err)
	}
	webhookMux.Handle("/node", nodeHandler)

	// in non-ic ovnkube-node without additional conditions does not have the permissions to update pods
	if config.EnableInterconnect || len(config.CSRAcceptanceConditions) > 1 {
		informerFactory := informers.NewSharedInformerFactory(client, 10*time.Minute)
		nodeInformer := informerFactory.Core().V1().Nodes().Informer()
		informerFactory.Start(stopCh)
		klog.Infof("Waiting for caches to sync")
		cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced)

		nodeLister := listers.NewNodeLister(nodeInformer.GetIndexer())
		podWebhook := admission.WithValidator(
			scheme.Scheme,
			ovnwebhook.NewPodAdmissionWebhook(nodeLister, config.PodAdmissionConditions, config.ExtraAllowedUsers...),
		).WithRecoverPanic(true)
		podHandler, err := admission.StandaloneWebhook(
			podWebhook,
			admission.StandaloneOptions{
				Logger:      logger.WithName("pod.network-identity"),
				MetricsPath: "pod.network-identity",
			},
		)
		if err != nil {
			return fmt.Errorf("failed to setup the pod admission webhook: %w", err)
		}
		webhookMux.Handle("/pod", podHandler)
	}

	cfg := &tls.Config{
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS10,
	}

	certPath := filepath.Join(config.CertDir, "tls.crt")
	keyPath := filepath.Join(config.CertDir, "tls.key")
	certWatcher, err := certwatcher.New(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("failed to setup certwatcher: %v", err)
	}
	cfg.GetCertificate = certWatcher.GetCertificate

	go func() {
		if err := certWatcher.Start(ctx); err != nil {
			klog.Fatalf("Certificate watcher failed to start: %v", err)
		}
	}()
	srv := &http.Server{
		Handler:           webhookMux,
		IdleTimeout:       90 * time.Second,
		ReadHeaderTimeout: 32 * time.Second,
	}

	l := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// Enable SO_REUSEPORT
				err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if err != nil {
					klog.Fatalf("Failed to set SO_REUSEPORT: %v", err)
				}
			})
		},
	}

	innerListener, err := l.Listen(ctx, "tcp", net.JoinHostPort(config.Host, strconv.Itoa(config.Port)))
	if err != nil {
		return fmt.Errorf("failed to create the listener: %v", err)
	}
	listener := tls.NewListener(innerListener, cfg)

	idleWebhookConnectionsClosed := make(chan struct{})
	defer func() {
		klog.Infof("Waiting for the webhook server to gracefully close")
		<-idleWebhookConnectionsClosed
	}()
	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			klog.Errorf("Failed shutting down the HTTP server: %v", err)
		}
		close(idleWebhookConnectionsClosed)
	}()

	klog.Infof("Starting the webhook server")
	return srv.Serve(listener)
}
