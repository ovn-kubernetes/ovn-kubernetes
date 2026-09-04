// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package egressip contains CRD integration tests for the EgressIP API.
// Tests run against a real kube-apiserver + etcd started by envtest so that
// admission defaulting, validation, and (future) CEL rules are exercised
// exactly as they would be in a live cluster.
//
// Run with:
//
//	make test-crd   (from the test/ directory)
package egressip

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	egressipscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/scheme"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       = context.Background()
)

// crdDir returns the absolute path to the committed CRD manifests directory.
// It is computed relative to this source file so the test works regardless of
// the working directory at invocation time.
func crdDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	// thisFile: .../test/crd-integration/egressip/suite_test.go
	// CRDs:     .../helm/ovn-kubernetes/crds/
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "helm", "ovn-kubernetes", "crds")
}

func TestEgressIPCRDIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EgressIP CRD Integration Suite")
}

var _ = BeforeSuite(func() {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{crdDir()},
		// Fail fast if the CRD YAML is absent rather than producing a
		// confusing "resource not found" error later.
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: egressipscheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	Expect(testEnv.Stop()).To(Succeed())
})
