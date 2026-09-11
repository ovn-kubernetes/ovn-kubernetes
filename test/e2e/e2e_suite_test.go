// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/diagnostics"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/ipalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/label"

	deploymentkind "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/configs/kind"
	deploymentkube "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/configs/kube"
	infraproviderkind "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/providers/kind"
	infraproviderkube "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/providers/kube"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2econfig "k8s.io/kubernetes/test/e2e/framework/config"
)

// https://github.com/kubernetes/kubernetes/blob/v1.16.4/test/e2e/e2e_test.go#L62

const infraProviderEnvVar = "OVN_TEST_INFRA_PROVIDER"

// handleFlags sets up all flags and parses the command line.
func handleFlags() {
	e2econfig.CopyFlags(e2econfig.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	diagnostics.RegisterFlags(flag.CommandLine)
	flag.StringVar(&reportPath, "report-path", "/tmp/kind/logs", "the path to be used to dump test failure information")
	flag.Parse()
}

var _ = ginkgo.BeforeSuite(func() {
	// Make sure the framework's kubeconfig is set.
	gomega.Expect(framework.TestContext.KubeConfig).NotTo(gomega.Equal(""), fmt.Sprintf("%s env var not set", clientcmd.RecommendedConfigPathEnvVar))

	// Preload e2e test images into the cluster to avoid runtime pull
	// failures and timeouts during test execution.
	infraprovider.Get().PreloadImages(images.Required())

	_, err := framework.LoadClientset()
	framework.ExpectNoError(err)
	config, err := framework.LoadConfig()
	framework.ExpectNoError(err)
	client, err := clientset.NewForConfig(config)
	framework.ExpectNoError(err, "k8 clientset is required to list nodes")
	if os.Getenv(uplinkDPUGatewayNetworkEnv) == "" {
		err = ipalloc.InitPrimaryIPAllocator(client.CoreV1().Nodes())
		// Under KinD no derivable range is a fault in a cluster the suite built, and still fails the run.
		if ipalloc.IsNoRangeError(err) && infraprovider.Get().Name() == infraproviderkube.ProviderName {
			framework.Logf("No addresses can be allocated on this cluster, specs that need one will skip: %v", err)
		} else {
			framework.ExpectNoError(err, "failed to initialize node primary IP allocator")
		}
	} else {
		framework.Logf("Skipping primary IP allocator initialization for DPU Uplink e2e")
	}
})

// required due to go1.13 issue: https://github.com/onsi/ginkgo/issues/602
func TestMain(m *testing.M) {
	// Register test flags, then parse flags.
	handleFlags()
	ProcessTestContextAndSetupLogging()

	switch provider := os.Getenv(infraProviderEnvVar); provider {
	case "", infraproviderkind.ProviderName:
		infraprovider.Set(infraproviderkind.New())
		deploymentconfig.Set(deploymentkind.New())
	case infraproviderkube.ProviderName:
		infraprovider.Set(infraproviderkube.New())
		deploymentconfig.Set(deploymentkube.New())
	default:
		klog.Fatalf("unsupported %s=%q, want %q or %q", infraProviderEnvVar, provider,
			infraproviderkind.ProviderName, infraproviderkube.ProviderName)
	}

	os.Exit(m.Run())
}

func TestE2E(t *testing.T) {
	if testing.Short() {
		return
	}
	if framework.TestContext.ReportDir != "" {
		if err := os.MkdirAll(framework.TestContext.ReportDir, 0755); err != nil {
			klog.Errorf("Failed creating report directory: %v", err)
		}
	}
	gomega.RegisterFailHandler(framework.Fail)
	ginkgo.RunSpecs(t, "E2E Suite", label.ComponentName())
}
