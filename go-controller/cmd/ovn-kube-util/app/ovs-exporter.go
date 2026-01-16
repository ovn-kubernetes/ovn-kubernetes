package app

import (
	"sync"
	"time"

	"github.com/urfave/cli/v2"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var OvsExporterCommand = cli.Command{
	Name:  "ovs-exporter",
	Usage: "",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "metrics-bind-address",
			Usage: `The IP address and port for the metrics server to serve on (default ":9310")`,
		},
	},
	Action: func(ctx *cli.Context) error {
		stopChan := make(chan struct{})
		bindAddress := ctx.String("metrics-bind-address")
		if bindAddress == "" {
			bindAddress = "0.0.0.0:9310"
		}

		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}

		// start the ovsdb client for ovs metrics monitoring
		ovsClient, err := libovsdb.NewOVSClient(stopChan)
		if err != nil {
			klog.Errorf("Error initializing ovs client: %v", err)
		}

		wg := &sync.WaitGroup{}

		opts := metrics.MetricServerOptions{
			BindAddress:      bindAddress,
			EnableOVSMetrics: true,
		}

		if _, err := metrics.StartOVNMetricsServer(opts, ovsClient, nil, stopChan, wg); err != nil {
			return err
		}

		// run until cancelled
		<-ctx.Context.Done()
		klog.Info("Shutdown signal received, stopping metrics server...")
		close(stopChan)

		// Wait for all goroutines to finish with a timeout
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			klog.Info("Metrics server stopped gracefully")
		case <-time.After(10 * time.Second):
			klog.Warning("Timeout waiting for metrics server to stop")
		}

		return nil
	},
}
