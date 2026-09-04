// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var metricOVNDBSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemDB,
	Name:      "jsonrpc_server_sessions",
	Help:      "Active number of JSON RPC Server sessions to the DB"},
	[]string{
		"db_name",
	},
)

var metricOVNDBMonitor = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemDB,
	Name:      "ovsdb_monitors",
	Help:      "Number of OVSDB Monitors on the server"},
	[]string{
		"db_name",
	},
)

var metricDBSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemDB,
	Name:      "db_size_bytes",
	Help:      "The size of the database file associated with the OVN DB component."},
	[]string{
		"db_name",
	},
)

// updateOvnDBSizeMetrics collects and updates the OVN DB size metric
func updateOvnDBSizeMetrics(dbProps *util.OvsDbProperties) {
	if size, err := getOvnDBSizeViaPath(dbProps); err != nil {
		klog.Errorf("Failed to update OVN DB size metric: %v", err)
	} else {
		metricDBSize.WithLabelValues(dbProps.DbName).Set(float64(size))
	}
}

// isOvnDBFoundViaPath attempts to find the OVN DBs, return false if not found.
func isOvnDBFoundViaPath(dbProperties []*util.OvsDbProperties) bool {
	enabled := true
	for _, dbProperty := range dbProperties {
		if _, err := getOvnDBSizeViaPath(dbProperty); err != nil {
			enabled = false
			break
		}
	}
	return enabled
}

func getOvnDBSizeViaPath(dbProperties *util.OvsDbProperties) (int64, error) {
	fileInfo, err := util.AppFs.Stat(dbProperties.DbAlias)
	if err != nil {
		return 0, fmt.Errorf("failed to find OVN DB database %s at path %s: %v",
			dbProperties.DbName, dbProperties.DbAlias, err)
	}
	return fileInfo.Size(), nil
}

// updateOvnDBMemoryMetrics collects and updates the OVN DB memory metric
func updateOvnDBMemoryMetrics(dbProperties *util.OvsDbProperties) {
	var stdout, stderr string
	var err error

	stdout, stderr, err = dbProperties.AppCtl(5, "memory/show")
	if err != nil {
		klog.Errorf("Failed retrieving memory/show output for %q, stderr: %s, err: (%v)",
			strings.ToUpper(dbProperties.DbName), stderr, err)
		return
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "monitors:") {
			// kvPair will be of the form monitors:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBMonitor.WithLabelValues(dbProperties.DbName).Set(value)
			} else {
				klog.Errorf("Failed to parse the monitor's value %s to float64: err(%v)",
					fields[1], err)
			}
		} else if strings.HasPrefix(kvPair, "sessions:") {
			// kvPair will be of the form sessions:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBSessions.WithLabelValues(dbProperties.DbName).Set(value)
			} else {
				klog.Errorf("Failed to parse the sessions' value %s to float64: err(%v)",
					fields[1], err)
			}
		}
	}
}

var (
	ovnDbVersion      string
	nbDbSchemaVersion string
	sbDbSchemaVersion string
)

func getNBDBSockPath() (string, error) {
	paths := []string{config.OvsPaths.RunDir, config.OvnNorth.RunDir}
	for _, basePath := range paths {
		if _, err := util.AppFs.Stat(basePath + "ovnnb_db.sock"); err == nil {
			klog.Infof("ovnnb_db.sock found at %s", basePath)
			return basePath, nil
		} else {
			klog.Infof("%sovnnb_db.sock getting info failed: %s", basePath, err)
		}
	}
	return "", fmt.Errorf("ovn db sock files weren't found in %s", strings.Join(paths, " or "))
}

func getOvnDbVersionInfo() {
	stdout, _, err := util.RunOVNNBAppCtl("version")
	if err == nil && strings.HasPrefix(stdout, "ovsdb-server (Open vSwitch) ") {
		ovnDbVersion = strings.Fields(stdout)[3]
	}
	basePath, err := getNBDBSockPath()
	if err != nil {
		klog.Errorf("OVN db schema versions can't be fetched: %s", err)
		return
	}
	sockPath := "unix:" + basePath + "ovnnb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Northbound")
	if err == nil {
		nbDbSchemaVersion = strings.TrimSpace(stdout)
	} else {
		klog.Errorf("OVN nbdb schema version can't be fetched: %s", err)
	}
	sockPath = "unix:" + basePath + "ovnsb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Southbound")
	if err == nil {
		sbDbSchemaVersion = strings.TrimSpace(stdout)
	} else {
		klog.Errorf("OVN sbdb schema version can't be fetched: %s", err)
	}
}

func RegisterOvnDBMetrics(ovnRegistry prometheus.Registerer) ([]*util.OvsDbProperties, bool) {
	// get the ovsdb server version info
	getOvnDbVersionInfo()
	// register metrics that will be served off of /metrics path
	ovnRegistry.MustRegister(metricOVNDBMonitor)
	ovnRegistry.MustRegister(metricOVNDBSessions)
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnNamespace,
			Subsystem: types.MetricOvnSubsystemDB,
			Name:      "build_info",
			Help: "A metric with a constant '1' value labeled by ovsdb-server version and " +
				"NB and SB schema version",
			ConstLabels: prometheus.Labels{
				"version":           ovnDbVersion,
				"nb_schema_version": nbDbSchemaVersion,
				"sb_schema_version": sbDbSchemaVersion,
			},
		},
		func() float64 { return 1 },
	))
	var dbProperties []*util.OvsDbProperties
	nbdbProps, err := util.GetOvsDbProperties(config.OvnNorth.DbLocation)
	if err != nil {
		klog.Errorf("Failed to init nbdb properties: %s", err)
	} else {
		klog.Infof("Found OVN NB DB: %v", nbdbProps)
		dbProperties = append(dbProperties, nbdbProps)
	}
	sbdbProps, err := util.GetOvsDbProperties(config.OvnSouth.DbLocation)
	if err != nil {
		klog.Errorf("Failed to init sbdb properties: %s", err)
	} else {
		klog.Infof("Found OVN SB DB: %v", sbdbProps)
		dbProperties = append(dbProperties, sbdbProps)
	}
	if len(dbProperties) == 0 {
		klog.Errorf("Failed to init properties for all databases")
		return nil, false
	}

	dbFoundViaPath := isOvnDBFoundViaPath(dbProperties)

	if dbFoundViaPath {
		ovnRegistry.MustRegister(metricDBSize)
	} else {
		klog.Infof("Unable to enable OVN DB size metric because no OVN DBs found")
	}

	return dbProperties, dbFoundViaPath
}
