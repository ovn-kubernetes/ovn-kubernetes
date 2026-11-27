// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

// envtestAvailable is true when the suite started a real apiserver via envtest.
// It is false when KUBEBUILDER_ASSETS is unset (e.g. a direct `go test` run).
// Integration specs that need a live apiserver skip themselves when this is
// false; fake-client and pure-logic specs run regardless. Both the shared
// `make check` and the operator's `make test` provision envtest.
var envtestAvailable bool

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(GinkgoLogr)

	ctx, cancel = context.WithCancel(context.TODO())

	// envtest binaries are discovered via KUBEBUILDER_ASSETS. When it is unset
	// (e.g. a direct `go test` run), skip starting the
	// control plane rather than failing; integration specs guard on
	// envtestAvailable and skip themselves.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		logf.Log.Info("KUBEBUILDER_ASSETS not set; skipping envtest setup (integration specs will be skipped)")
		return
	}

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		// CRDs are kustomized under the operator's config/crd/bases/. From this
		// file at operator/internal/controller/, that's two
		// levels up to the operator project root.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,

		// envtest binary assets are discovered via the KUBEBUILDER_ASSETS env
		// var, which `make test` sets from setup-envtest. To run this suite
		// directly (`go test`), export KUBEBUILDER_ASSETS yourself, e.g.:
		//   export KUBEBUILDER_ASSETS=$(setup-envtest use 1.31.0 -p path)
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = k8sv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	envtestAvailable = true
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	if testEnv != nil {
		By("tearing down the test environment")
		Expect(testEnv.Stop()).To(Succeed())
	}
})
