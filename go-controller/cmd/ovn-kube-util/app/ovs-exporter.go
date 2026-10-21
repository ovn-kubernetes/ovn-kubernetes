// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"time"

	"github.com/urfave/cli/v2"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const metricsCollectionIntervalFlag = "metrics-collection-interval"

func ovsExporterMetricsCollectionInterval(ctx *cli.Context) time.Duration {
	return time.Duration(ctx.Int(metricsCollectionIntervalFlag)) * time.Second
}

var OvsExporterCommand = cli.Command{
	Name:  "ovs-exporter",
	Usage: "",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "metrics-bind-address",
			Usage: `The IP address and port for the metrics server to serve on (default ":9310")`,
		},
		&cli.IntFlag{
			Name: metricsCollectionIntervalFlag,
			Usage: fmt.Sprintf("Interval in seconds at which OVS metric values are extracted. "+
				"Zero collects on demand during each scrape. Negative uses the default (%fs).",
				config.DefaultMetricsCollectionInterval.Seconds()),
			Value: int(config.DefaultMetricsCollectionInterval / time.Second),
		},
	},
	Action: func(ctx *cli.Context) error {
		innerCtx, cancel := context.WithCancel(ctx.Context)
		defer cancel()

		bindAddress := ctx.String("metrics-bind-address")
		if bindAddress == "" {
			bindAddress = "0.0.0.0:9310"
		}

		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}

		// start the ovsdb client for ovs metrics monitoring
		ovsClient, err := libovsdb.NewOVSClient(innerCtx.Done())
		if err != nil {
			klog.Errorf("Error initializing ovs client: %v", err)
			return err
		}

		opts := metrics.MetricServerOptions{
			BindAddress:        bindAddress,
			EnableOVSMetrics:   true,
			OnFatalError:       cancel,
			OVSDBClient:        ovsClient,
			CollectionInterval: ovsExporterMetricsCollectionInterval(ctx),
		}

		_, metricsDone := metrics.StartMetricsServer(opts, innerCtx.Done())

		// run until cancelled (by OS signal or fatal error)
		<-innerCtx.Done()
		klog.Info("Shutdown signal received, stopping metrics server...")

		select {
		case <-metricsDone:
			klog.Info("Metrics server stopped gracefully")
		case <-time.After(10 * time.Second):
			klog.Warning("Timeout waiting for metrics server to stop")
		}

		return nil
	},
}
