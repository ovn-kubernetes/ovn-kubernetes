package e2e

import (
	"errors"
	"os"
	"path"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	"github.com/onsi/gomega"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
)

// ProcessTestContextAndSetupLogging copied up k8 e2e test framework pkg because we need to remove the label check.
func ProcessTestContextAndSetupLogging() {
	t := &framework.TestContext
	// default copied from k8 e2e test framework pkg
	// Reconfigure gomega defaults. The poll interval should be suitable
	// for most tests. The timeouts are more subjective and tests may want
	// to override them, but these defaults are still better for E2E than the
	// ones from Gomega (1s timeout, 10ms interval).
	var defaultTimeouts = framework.TimeoutContext{
		Poll:                      2 * time.Second, // from the former e2e/framework/pod poll interval
		PodStart:                  5 * time.Minute,
		PodStartShort:             2 * time.Minute,
		PodStartSlow:              15 * time.Minute,
		PodDelete:                 5 * time.Minute,
		ClaimProvision:            5 * time.Minute,
		ClaimProvisionShort:       1 * time.Minute,
		DataSourceProvision:       5 * time.Minute,
		ClaimBound:                3 * time.Minute,
		PVReclaim:                 3 * time.Minute,
		PVBound:                   3 * time.Minute,
		PVCreate:                  3 * time.Minute,
		PVDelete:                  5 * time.Minute,
		PVDeleteSlow:              20 * time.Minute,
		SnapshotCreate:            5 * time.Minute,
		SnapshotDelete:            5 * time.Minute,
		SnapshotControllerMetrics: 5 * time.Minute,
		SystemPodsStartup:         10 * time.Minute,
		NodeSchedulable:           30 * time.Minute,
		SystemDaemonsetStartup:    5 * time.Minute,
		NodeNotReady:              3 * time.Minute,
	}
	gomega.SetDefaultEventuallyPollingInterval(defaultTimeouts.Poll)
	gomega.SetDefaultConsistentlyPollingInterval(defaultTimeouts.Poll)
	gomega.SetDefaultEventuallyTimeout(defaultTimeouts.PodStart)
	gomega.SetDefaultConsistentlyDuration(defaultTimeouts.PodStartShort)

	// Allow 1% of nodes to be unready (statistically) - relevant for large clusters.
	if t.AllowedNotReadyNodes == 0 {
		t.AllowedNotReadyNodes = t.CloudConfig.NumNodes / 100
	}

	// Make sure that all test runs have a valid TestContext.CloudConfig.Provider.
	// TODO: whether and how long this code is needed is getting discussed
	// in https://github.com/kubernetes/kubernetes/issues/70194.
	if t.Provider == "" {
		t.Provider = "skeleton"
	}

	var err error
	t.CloudConfig.Provider, err = framework.SetupProviderConfig(t.Provider)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) {
			klog.Errorf("Unknown provider %q. ", t.Provider)
		} else {
			klog.Errorf("Failed to setup provider config for %q: %v", t.Provider, err)
		}
		os.Exit(1)
	}

	if t.ReportDir != "" {
		// Create the directory before running the suite. If
		// --report-dir is not unusable, we should report
		// that as soon as possible. This will be done by each worker
		// in parallel, so we will get "exists" error in most of them.
		if err := os.MkdirAll(t.ReportDir, 0777); err != nil && !os.IsExist(err) {
			klog.Errorf("Create report dir: %v", err)
			os.Exit(1)
		}
		ginkgoDir := path.Join(t.ReportDir, "ginkgo")
		if t.ReportCompleteGinkgo || t.ReportCompleteJUnit {
			if err := os.MkdirAll(ginkgoDir, 0777); err != nil && !os.IsExist(err) {
				klog.Errorf("Create <report-dir>/ginkgo: %v", err)
				os.Exit(1)
			}
		}

		if t.ReportCompleteGinkgo {
			ginkgo.ReportAfterSuite("Ginkgo JSON report", func(report ginkgo.Report) {
				gomega.Expect(reporters.GenerateJSONReport(report, path.Join(ginkgoDir, "report.json"))).NotTo(gomega.HaveOccurred())
			})
			ginkgo.ReportAfterSuite("JUnit XML report", func(report ginkgo.Report) {
				gomega.Expect(reporters.GenerateJUnitReport(report, path.Join(ginkgoDir, "report.xml"))).NotTo(gomega.HaveOccurred())
			})
		}

		ginkgo.ReportAfterSuite("OVN-Kubernetes e2e JUnit report", func(report ginkgo.Report) {
			// With Ginkgo v1, we used to write one file per
			// parallel node. Now Ginkgo v2 automatically merges
			// all results into a report for us. The 01 suffix is
			// kept in case that users expect files to be called
			// "junit_<prefix><number>.xml".
			junitReport := path.Join(t.ReportDir, "junit_"+t.ReportPrefix+"01.xml")

			// writeJUnitReport generates a JUnit file in the e2e
			// report directory that is shorter than the one
			// normally written by `ginkgo --junit-report`. This is
			// needed because the full report can become too large
			// for tools like Spyglass
			// (https://github.com/kubernetes/kubernetes/issues/111510).
			gomega.Expect(writeJUnitReport(report, junitReport)).NotTo(gomega.HaveOccurred())
		})
	}
}

// writeJUnitReport generates a JUnit file that is shorter than the one
// normally written by `ginkgo --junit-report`. This is needed because the full
// report can become too large for tools like Spyglass
// (https://github.com/kubernetes/kubernetes/issues/111510).
func writeJUnitReport(report ginkgo.Report, filename string) error {
	config := reporters.JunitReportConfig{
		// Remove details for specs where we don't care.
		OmitTimelinesForSpecState: ginkgotypes.SpecStatePassed | ginkgotypes.SpecStateSkipped,

		// Don't write <failure message="summary">. The same text is
		// also in the full text for the failure. If we were to write
		// both, then tools like kettle and spyglass would concatenate
		// the two strings and thus show duplicated information.
		OmitFailureMessageAttr: true,

		// All labels are also part of the spec texts in inline [] tags,
		// so we don't need to write them separately.
		OmitSpecLabels: true,
	}

	return reporters.GenerateJUnitReportWithConfig(report, filename, config)
}
