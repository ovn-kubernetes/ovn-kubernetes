package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controllermanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	ovnnode "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	// CustomAppHelpTemplate helps in grouping options to ovnkube
	CustomAppHelpTemplate = `NAME:
   {{.Name}} - {{.Usage}}

USAGE:
   {{.HelpName}} [global options]

VERSION:
   {{.Version}}{{if .Description}}

DESCRIPTION:
   {{.Description}}{{end}}

COMMANDS:{{range .VisibleCategories}}{{if .Name}}

   {{.Name}}:{{end}}{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}

GLOBAL OPTIONS:
{{range $title, $category := getFlagsByCategory}}
   {{upper $title}}:
	 {{$maxLen := maxOptionLength $category}}{{$categoryIndent := makeIndentForLength $maxLen}}
   {{range $index, $option := $category}}{{if $index}}

   {{end}}--{{$option.Name}}{{if flagTakesValue $option}} value{{end}}{{$currentLen := len $option.Name}}{{if flagTakesValue $option}}{{$currentLen = add $currentLen 6}}{{end}}{{printf "%*s" (sub $maxLen $currentLen) ""}}  {{wrapWithIndent (printf "%s%s%s" $option.Usage (formatDefaultValue (getDefaultValue $option)) (formatEnvVars (getEnvVars $option))) $categoryIndent}}{{end}}
   {{end}}`
)

func getFlagsByCategory() map[string][]cli.Flag {
	m := map[string][]cli.Flag{}
	m["Generic Options"] = config.CommonFlags
	m["CNI Options"] = config.CNIFlags
	m["Feature Flags"] = config.OVNK8sFeatureFlags
	m["K8s-related Options"] = config.K8sFlags
	m["OVN Northbound DB Options"] = config.OvnNBFlags
	m["OVN Southbound DB Options"] = config.OvnSBFlags
	m["OVN Gateway Options"] = config.OVNGatewayFlags
	m["Cluster Manager Options"] = config.ClusterManagerFlags
	m["Cluster Manager HA Options"] = config.ClusterMgrHAFlags
	m["Master HA Options"] = config.MasterHAFlags
	m["OVN Kube Node Options"] = config.OvnKubeNodeFlags
	m["Monitoring Options"] = config.MonitoringFlags
	m["IPFIX Flow Tracing Options"] = config.IPFIXFlags
	m["Metrics Options"] = config.MetricsFlags
	m["Hybrid Overlay Options"] = config.HybridOverlayFlags

	return m
}

// getTerminalWidth returns the width of the terminal, defaulting to 80 if unable to determine
func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80 // default width
	}
	return width
}

// wrapWithIndent wraps text to fit within specified width, adding indent to continuation lines
func wrapWithIndent(text string, width int, indent string) string {
	if len(text) <= width {
		return text
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}

	var result strings.Builder
	currentLine := words[0]

	for _, word := range words[1:] {
		// Check if adding the next word would exceed the width
		if len(currentLine)+1+len(word) <= width {
			currentLine += " " + word
		} else {
			// Write current line and start a new one with indent
			result.WriteString(currentLine)
			result.WriteString("\n")
			result.WriteString(indent)
			currentLine = word
		}
	}

	// Write the final line
	result.WriteString(currentLine)
	return result.String()
}

// borrowed from cli packages' printHelpCustom()
func printOvnKubeHelp(out io.Writer, templ string, data interface{}, customFunc map[string]interface{}) {
	termWidth := getTerminalWidth()
	funcMap := template.FuncMap{
		"join":               strings.Join,
		"upper":              strings.ToUpper,
		"getFlagsByCategory": getFlagsByCategory,
		"wrapWithIndent": func(text string, indent string) string {
			// Reserve space for option name and spacing, use remaining width for description
			availableWidth := termWidth - len(indent) - 1
			if availableWidth < 40 {
				availableWidth = 40 // minimum reasonable width
			}
			return wrapWithIndent(text, availableWidth, indent)
		},
		"makeIndentForLength": func(optionNameLength int) string {
			// Create indent string to align continuation lines with description text
			// Format is "   --<option-name>  " so we need to match that length
			baseIndent := "   --" + strings.Repeat(" ", optionNameLength) + "  "
			return strings.Repeat(" ", len(baseIndent))
		},
		"maxOptionLength": func(category []cli.Flag) int {
			maxLen := 0
			for _, flag := range category {
				nameLen := len(flag.Names()[0])
				// Add 6 characters for " value" if flag takes a value
				if _, isBoolFlag := flag.(*cli.BoolFlag); !isBoolFlag {
					nameLen += 6
				}
				if nameLen > maxLen {
					maxLen = nameLen
				}
			}
			return maxLen
		},
		"sub": func(a, b int) int {
			return a - b
		},
		"add": func(a, b int) int {
			return a + b
		},
		"getEnvVars": func(flag cli.Flag) []string {
			// Use reflection to get the EnvVars field from different flag types
			val := reflect.ValueOf(flag)
			if val.Kind() == reflect.Ptr {
				val = val.Elem()
			}
			envVarsField := val.FieldByName("EnvVars")
			if !envVarsField.IsValid() {
				return nil
			}
			if envVarsField.Kind() == reflect.Slice && envVarsField.Type().Elem().Kind() == reflect.String {
				result := make([]string, envVarsField.Len())
				for i := 0; i < envVarsField.Len(); i++ {
					result[i] = envVarsField.Index(i).String()
				}
				return result
			}
			return nil
		},
		"formatEnvVars": func(envVars []string) string {
			if len(envVars) == 0 {
				return ""
			}
			// Add $ prefix to each environment variable
			prefixedVars := make([]string, len(envVars))
			for i, envVar := range envVars {
				prefixedVars[i] = "$" + envVar
			}

			// Use non-breaking spaces within the env section to prevent splitting
			envString := strings.Join(prefixedVars, ", ")
			return " (env: " + envString + ")"
		},
		"flagTakesValue": func(flag cli.Flag) bool {
			// Check if the flag type takes a value (not a BoolFlag)
			switch flag.(type) {
			case *cli.BoolFlag:
				return false
			default:
				return true
			}
		},
		"getDefaultValue": func(flag cli.Flag) string {
			// Use reflection to get the default value from different flag types
			val := reflect.ValueOf(flag)
			if val.Kind() == reflect.Ptr {
				val = val.Elem()
			}

			// Try to get the Value field which contains the default
			valueField := val.FieldByName("Value")
			if valueField.IsValid() {
				switch flag.(type) {
				case *cli.StringFlag:
					if str := valueField.String(); str != "" {
						return str
					}
				case *cli.IntFlag:
					if intVal := valueField.Int(); intVal != 0 {
						return fmt.Sprintf("%d", intVal)
					}
				case *cli.Float64Flag:
					if floatVal := valueField.Float(); floatVal != 0 {
						return fmt.Sprintf("%g", floatVal)
					}
				case *cli.BoolFlag:
					if boolVal := valueField.Bool(); boolVal {
						return "true"
					}
				case *cli.DurationFlag:
					if durVal := valueField.Interface(); durVal != time.Duration(0) {
						return fmt.Sprintf("%v", durVal)
					}
				}
			}
			return ""
		},
		"formatDefaultValue": func(defaultVal string) string {
			if defaultVal == "" {
				return ""
			}
			return fmt.Sprintf(" (default: %s)", defaultVal)
		},
	}
	for key, value := range customFunc {
		funcMap[key] = value
	}

	w := tabwriter.NewWriter(out, 1, 8, 2, ' ', 0)
	t := template.Must(template.New("help").Funcs(funcMap).Parse(templ))
	err := t.Execute(w, data)
	if err == nil {
		_ = w.Flush()
	}
}

func main() {
	cli.HelpPrinterCustom = printOvnKubeHelp
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = config.Version
	c.CustomAppHelpTemplate = CustomAppHelpTemplate
	c.Flags = config.GetFlags(nil)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	c.Action = func(ctx *cli.Context) error {
		return startOvnKube(ctx, cancel)
	}

	// trap SIGHUP, SIGINT, SIGTERM, SIGQUIT and
	// cancel the context
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer func() {
		signal.Stop(exitCh)
		cancel()
	}()
	go func() {
		select {
		case s := <-exitCh:
			klog.Infof("Received signal %s. Shutting down", s)
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := c.RunContext(ctx, os.Args); err != nil {
		klog.Exit(err)
	}
}

func delPidfile(pidfile string) {
	if pidfile != "" {
		if _, err := os.Stat(pidfile); err == nil {
			if err := os.Remove(pidfile); err != nil {
				klog.Errorf("%s delete failed: %v", pidfile, err)
			}
		}
	}
}

func setupPIDFile(pidfile string) error {
	// need to test if already there
	_, err := os.Stat(pidfile)

	// Create if it doesn't exist, else exit with error
	if os.IsNotExist(err) {
		if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
			klog.Errorf("Failed to write pidfile %s (%v). Ignoring..", pidfile, err)
		}
	} else {
		// get the pid and see if it exists
		pid, err := os.ReadFile(pidfile)
		if err != nil {
			return fmt.Errorf("pidfile %s exists but can't be read: %v", pidfile, err)
		}
		_, err1 := os.Stat("/proc/" + string(pid[:]) + "/cmdline")
		if os.IsNotExist(err1) {
			// Left over pid from dead process
			if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
				klog.Errorf("Failed to write pidfile %s (%v). Ignoring..", pidfile, err)
			}
		} else {
			return fmt.Errorf("pidfile %s exists and ovnkube is running", pidfile)
		}
	}

	return nil
}

// ovnkubeRunMode object stores the run mode of the ovnkube
type ovnkubeRunMode struct {
	ovnkubeController bool // ovnkube controller (--init-ovnkube-controller or --init-master) is enabled
	clusterManager    bool // cluster manager (--init-cluster-manager or --init-master) is enabled
	node              bool // node (--init-node) is enabled
	cleanupNode       bool // cleanup (--cleanup-node) is enabled

	// Along with the run mode, an identity is provided that uniquely identifies
	// this instance vs other instances that might be running in the cluster.
	// The identity is usually the node name. It's used for leader election
	// among other things.
	identity string
}

// determineOvnkubeRunMode determines the run modes of ovnkube
// based on the init flags set.  It is possible to run ovnkube in
// multiple modes.  Allowed multiple modes are:
//   - master (ovnkube controller + cluster manager) + node
//   - ovnkube controller + cluster manager
//   - ovnkube controller + node
func determineOvnkubeRunMode(ctx *cli.Context) (*ovnkubeRunMode, error) {
	mode := &ovnkubeRunMode{}

	master := ctx.String("init-master")
	cm := ctx.String("init-cluster-manager")
	ovnkController := ctx.String("init-ovnkube-controller")
	node := ctx.String("init-node")
	cleanup := ctx.String("cleanup-node")

	if master != "" {
		// If init-master is set, then both ovnkube controller and cluster manager
		// are enabled
		mode.ovnkubeController = true
		mode.clusterManager = true
	}

	if cm != "" {
		mode.clusterManager = true
	}

	if ovnkController != "" {
		mode.ovnkubeController = true
	}

	if node != "" {
		mode.node = true
	}

	if cleanup != "" {
		mode.cleanupNode = true
	}

	if mode.cleanupNode && (mode.clusterManager || mode.ovnkubeController || mode.node) {
		return nil, fmt.Errorf("cannot run cleanup-node mode along with any other mode")
	}

	if !mode.clusterManager && !mode.ovnkubeController && !mode.node && !mode.cleanupNode {
		return nil, fmt.Errorf("need to specify a mode for ovnkube")
	}

	if !mode.ovnkubeController && mode.clusterManager && mode.node {
		return nil, fmt.Errorf("cannot run in both cluster manager and node mode")
	}

	identities := sets.NewString(master, cm, ovnkController, node, cleanup)
	identities.Delete("")
	if identities.Len() != 1 {
		return nil, fmt.Errorf("provided no identity or different identities for different modes")
	}

	mode.identity, _ = identities.PopAny()

	return mode, nil
}

func startOvnKube(ctx *cli.Context, cancel context.CancelFunc) error {
	pidfile := ctx.String("pidfile")
	if pidfile != "" {
		defer delPidfile(pidfile)
		if err := setupPIDFile(pidfile); err != nil {
			return err
		}
	}

	exec := kexec.New()
	_, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}

	if err = util.SetExec(exec); err != nil {
		return fmt.Errorf("failed to initialize exec helper: %v", err)
	}

	ovnKubeStartWg := &sync.WaitGroup{}
	defer func() {
		// make sure everything stops and wait
		cancel()
		ovnKubeStartWg.Wait()
	}()

	if config.Kubernetes.BootstrapKubeconfig != "" {
		// In the case of dpus K8S_NODE will be set to dpu host's name
		var csrNodeName string
		if config.OvnKubeNode.Mode == types.NodeModeDPU {
			csrNodeName = os.Getenv("K8S_NODE_DPU")
		} else {
			csrNodeName = os.Getenv("K8S_NODE")
		}
		if err := util.StartNodeCertificateManager(ctx.Context, ovnKubeStartWg, csrNodeName, &config.Kubernetes); err != nil {
			return fmt.Errorf("failed to start the node certificate manager: %w", err)
		}
	}
	ovnClientset, err := util.NewOVNClientset(&config.Kubernetes)
	if err != nil {
		return err
	}

	runMode, err := determineOvnkubeRunMode(ctx)
	if err != nil {
		return err
	}

	eventRecorder := util.EventRecorder(ovnClientset.KubeClient)

	// Start metric server for master and node. Expose the metrics HTTP endpoint if configured.
	// Non LE master instances also are required to expose the metrics server.
	if config.Metrics.BindAddress != "" {
		metrics.StartMetricsServer(config.Metrics.BindAddress, config.Metrics.EnablePprof,
			config.Metrics.NodeServerCert, config.Metrics.NodeServerPrivKey, ctx.Done(), ovnKubeStartWg)
	}

	// no need for leader election in node mode
	// only node mode
	if !runMode.clusterManager && !runMode.ovnkubeController {
		return runOvnKube(ctx.Context, runMode, ovnClientset, eventRecorder)
	}

	// ovnkube-controller with node
	if runMode.node && runMode.ovnkubeController {
		metrics.RegisterOVNKubeControllerBase()
		return runOvnKube(ctx.Context, runMode, ovnClientset, eventRecorder)
	}

	// Register prometheus metrics that do not depend on becoming ovnkube-controller
	// leader and get the proper HA config depending on the mode. For ovnkube
	// controller mode or combined cluster manager and ovnkube-controller modes (the classic
	// master mode), the master HA config applies. For cluster manager
	// standalone mode, the cluster manager HA config applies.
	var haConfig *config.HAConfig
	var name string
	switch {
	case runMode.ovnkubeController && runMode.clusterManager:
		metrics.RegisterClusterManagerBase()
		fallthrough
	case runMode.ovnkubeController:
		metrics.RegisterOVNKubeControllerBase()
		haConfig = &config.MasterHA
		name = controllerManagerLockName()
	case runMode.clusterManager:
		metrics.RegisterClusterManagerBase()
		haConfig = &config.ClusterMgrHA
		name = "ovn-kubernetes-master"
	}

	// Set up leader election process. Use lease resource lock as configmap and
	// endpoint lock support has been removed from leader election library.
	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		name,
		ovnClientset.KubeClient.CoreV1(),
		ovnClientset.KubeClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      runMode.identity,
			EventRecorder: eventRecorder,
		},
	)
	if err != nil {
		return err
	}

	ovnKubeStopped := false
	ovnKubeStopLock := sync.Mutex{}
	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(haConfig.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(haConfig.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(haConfig.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// Looking at the leader elector implementation,
				// OnStartedLeading is called asynchronously with respect
				// OnStoppedLeading and there seems to be no guarantee that
				// OnStartedLeading won't run after OnStoppedLeading, so take
				// some additional precautions to ensure we don't start when we
				// shouldn't.
				ovnKubeStopLock.Lock()
				if ovnKubeStopped {
					ovnKubeStopLock.Unlock()
					return
				}
				ovnKubeStartWg.Add(1)
				defer ovnKubeStartWg.Done()
				ovnKubeStopLock.Unlock()
				klog.Infof("Won leader election; in active mode")
				if err := runOvnKube(ctx, runMode, ovnClientset, eventRecorder); err != nil {
					klog.Error(err)
					cancel()
				}
			},
			OnStoppedLeading: func() {
				ovnKubeStopLock.Lock()
				defer ovnKubeStopLock.Unlock()
				klog.Infof("No longer leader; exiting")
				ovnKubeStopped = true
				cancel()
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != runMode.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(ovnkubeMetricsProvider{runMode})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	leaderElector.Run(ctx.Context)
	// Looking at the leader election implementation, OnStoppedLeading is called
	// synchronously before Run exits. But the callbacks are documented as
	// asynchronous so again out of precaution we make sure we don't start when
	// we shouldn't.
	ovnKubeStopLock.Lock()
	ovnKubeStopped = true
	ovnKubeStopLock.Unlock()

	return nil
}

func runOvnKube(ctx context.Context, runMode *ovnkubeRunMode, ovnClientset *util.OVNClientset, eventRecorder record.EventRecorder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic in runOvnKube: %v", r)
		}
	}()
	startTime := time.Now()

	if runMode.cleanupNode {
		return ovnnode.CleanupClusterNode(runMode.identity)
	}

	watchFactory, err := newWatchFactory(runMode, ovnClientset)
	if err != nil {
		return fmt.Errorf("failed to initialize watch factory: %w", err)
	}

	// there might be dependencies across components when starting so run them
	// in separate threads
	wg := &sync.WaitGroup{}
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	var managerErr, controllerErr, nodeErr, ovsCLIErr error

	if runMode.clusterManager {
		wg.Add(1)
		go func() {
			defer cancel()
			defer wg.Done()

			clusterManager, err := clustermanager.NewClusterManager(
				ovnClientset.GetClusterManagerClientset(),
				watchFactory,
				runMode.identity,
				eventRecorder)
			if err != nil {
				managerErr = fmt.Errorf("failed to create new cluster manager: %w", err)
				return
			}

			metrics.RegisterClusterManagerFunctional()

			err = clusterManager.Start(ctx)
			if err != nil {
				managerErr = fmt.Errorf("failed to start cluster manager: %w", err)
				return
			}

			// record delay until ready
			metrics.MetricClusterManagerReadyDuration.Set(time.Since(startTime).Seconds())

			<-ctx.Done()
			clusterManager.Stop()
		}()
	}
	// when ovnkube is running in ovnkube-controller and ovnkube node mode in the same process, bool is used to inform ovnkube-node that ovnkube-controller
	// has sync'd once and changes have propagated to SB DB. ovnkube-node will then remove flows for dropping GARPs.
	// Remove when OVN supports native silencing of GARPs on startup: https://issues.redhat.com/browse/FDP-1537
	// isOVNKubeControllerSyncd is true when ovnkube controller has sync and changes are in OVN Southbound database.
	var isOVNKubeControllerSyncd *atomic.Bool
	if runMode.ovnkubeController && runMode.node && config.OVNKubernetesFeature.EnableEgressIP && config.OVNKubernetesFeature.EnableInterconnect && config.OvnKubeNode.Mode == types.NodeModeFull {
		isOVNKubeControllerSyncd = &atomic.Bool{}
	}

	if runMode.ovnkubeController {
		wg.Add(1)
		go func() {
			defer cancel()
			defer wg.Done()

			libovsdbOvnNBClient, err := libovsdb.NewNBClient(ctx.Done())
			if err != nil {
				controllerErr = fmt.Errorf("failed to initialize libovsdb NB client: %w", err)
				return
			}

			libovsdbOvnSBClient, err := libovsdb.NewSBClient(ctx.Done())
			if err != nil {
				controllerErr = fmt.Errorf("failed to initialize libovsdb SB client: %w", err)
				return
			}

			controllerManager, err := controllermanager.NewControllerManager(
				ovnClientset,
				watchFactory,
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				eventRecorder,
				wg)
			if err != nil {
				controllerErr = fmt.Errorf("failed to initialize network controller: %w", err)
				return
			}

			err = controllerManager.Start(ctx)
			if err != nil {
				controllerErr = fmt.Errorf("failed to start network controller: %w", err)
				return
			}
			// record delay until ready
			metrics.MetricOVNKubeControllerReadyDuration.Set(time.Since(startTime).Seconds())

			if isOVNKubeControllerSyncd != nil {
				klog.Infof("Waiting for OVN northbound database changes to be processed by ovn-controller")
				if err = libovsdbutil.WaitUntilFlowsInstalled(ctx, libovsdbOvnNBClient); err != nil {
					controllerErr = fmt.Errorf("failed waiting for OVN northbound database changes to be processed by ovn-controller: %v", err)
					return
				} else {
					klog.Infof("Finished waiting for OVN northbound database changes to be processed by ovn-controller")
					isOVNKubeControllerSyncd.Store(true)
				}
			}

			<-ctx.Done()
			controllerManager.Stop()
		}()
	}

	var ovsClient client.Client
	if runMode.node {
		wg.Add(1)
		go func() {
			defer cancel()
			defer wg.Done()

			if config.Kubernetes.Token == "" {
				nodeErr = fmt.Errorf("cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
				return
			}

			// register ovnkube node specific prometheus metrics exported by the node
			metrics.RegisterNodeMetrics(ctx.Done())

			// OVS is not running on dpu-host nodes
			if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
				ovsClient, err = libovsdb.NewOVSClient(ctx.Done())
				if err != nil {
					nodeErr = fmt.Errorf("failed to initialize libovsdb vswitchd client: %w", err)
					return
				}
			}

			nodeControllerManager, err := controllermanager.NewNodeControllerManager(
				ovnClientset,
				watchFactory,
				runMode.identity,
				wg,
				eventRecorder,
				routemanager.NewController(),
				ovsClient,
			)
			if err != nil {
				nodeErr = fmt.Errorf("failed to create node network controller: %w", err)
				return
			}

			err = nodeControllerManager.Start(ctx, isOVNKubeControllerSyncd)
			if err != nil {
				nodeErr = fmt.Errorf("failed to start node network controller: %w", err)
				return
			}

			// record delay until ready
			metrics.MetricNodeReadyDuration.Set(time.Since(startTime).Seconds())

			<-ctx.Done()
			nodeControllerManager.Stop(isOVNKubeControllerSyncd)
		}()
	}

	// start the prometheus server to serve OVS and OVN Metrics (default port: 9476)
	// Note: for ovnkube node mode dpu-host no metrics is required as ovs/ovn is not running on the node.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.Metrics.OVNMetricsBindAddress != "" {
		metricsScrapeInterval := 30

		if ovsClient == nil {
			ovsClient, err = libovsdb.NewOVSClient(ctx.Done())
			if err != nil {
				ovsCLIErr = fmt.Errorf("failed to initialize libovsdb vswitchd client: %w", err)
				cancel()
			}
		}
		if ovsClient != nil {
			if config.Metrics.ExportOVSMetrics {
				metrics.RegisterOvsMetricsWithOvnMetrics(ovsClient, metricsScrapeInterval, ctx.Done())
			}
			metrics.RegisterOvnMetrics(ovnClientset.KubeClient, runMode.identity,
				ovsClient, metricsScrapeInterval, ctx.Done())
			metrics.StartOVNMetricsServer(config.Metrics.OVNMetricsBindAddress,
				config.Metrics.NodeServerCert, config.Metrics.NodeServerPrivKey, ctx.Done(), wg)
		}
	}

	// run until cancelled
	<-ctx.Done()
	klog.Infof("Stopping ovnkube...")
	cancel()
	watchFactory.Shutdown()
	wg.Wait()
	klog.Infof("Stopped ovnkube")

	err = utilerrors.Join(managerErr, controllerErr, nodeErr, ovsCLIErr)
	if err != nil {
		return fmt.Errorf("failed to run ovnkube: %w", err)
	}

	return nil
}

// newWatchFactory returns the proper watch factory to use depending on the run
// mode
func newWatchFactory(runMode *ovnkubeRunMode, ovnClientset *util.OVNClientset) (watchFactory *factory.WatchFactory, err error) {
	switch {
	case runMode.clusterManager && runMode.ovnkubeController:
		watchFactory, err = factory.NewMasterWatchFactory(ovnClientset.GetMasterClientset())
	case runMode.clusterManager:
		watchFactory, err = factory.NewClusterManagerWatchFactory(ovnClientset.GetClusterManagerClientset())
	case runMode.ovnkubeController:
		watchFactory, err = factory.NewOVNKubeControllerWatchFactory(ovnClientset.GetOVNKubeControllerClientset())
	case runMode.node:
		watchFactory, err = factory.NewNodeWatchFactory(ovnClientset.GetNodeClientset(), runMode.identity)
	default:
		err = fmt.Errorf("unsupported ovnkube run mode: %+v", runMode)
	}
	return
}

type leaderMetrics struct {
	runMode *ovnkubeRunMode
}

func (m leaderMetrics) On(string) {
	if m.runMode.ovnkubeController {
		metrics.MetricOVNKubeControllerLeader.Set(1)
	}
	if m.runMode.clusterManager {
		metrics.MetricClusterManagerLeader.Set(1)
	}
}

func (m leaderMetrics) Off(string) {
	if m.runMode.ovnkubeController {
		metrics.MetricOVNKubeControllerLeader.Set(0)
	}
	if m.runMode.clusterManager {
		metrics.MetricClusterManagerLeader.Set(0)
	}
}

func (m leaderMetrics) SlowpathExercised(string) {}

type ovnkubeMetricsProvider struct {
	runMode *ovnkubeRunMode
}

func (p ovnkubeMetricsProvider) NewLeaderMetric() leaderelection.LeaderMetric {
	return &leaderMetrics{p.runMode}
}

func controllerManagerLockName() string {
	// keep the same old lock name unless we are owners of a specific zone
	name := "ovn-kubernetes-master"
	if config.Default.Zone != types.OvnDefaultZone {
		name = name + "-" + config.Default.Zone
	}
	return name
}
