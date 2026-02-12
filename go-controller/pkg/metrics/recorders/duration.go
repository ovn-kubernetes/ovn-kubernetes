package recorders

import (
	"fmt"
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	updateOVNMeasurementChSize = 500
	deleteOVNMeasurementChSize = 50
	processChSize              = 1000
	nbGlobalTable              = "NB_Global"
	//fixme: remove when bug is fixed in OVN (Red Hat bugzilla bug number 2074019). Also, handle overflow event.
	maxNbCfg               = math.MaxUint32 - 1000
	maxMeasurementLifetime = 20 * time.Minute
)

var configDurationRegOnce sync.Once

type ConfigDurationRecorder struct {
	// rate at which measurements are allowed. Probabilistically, 1 in every measurementRate
	measurementRate uint64
	measurements    map[string]measurement
	// controls RW access to measurements map
	measurementsMu sync.RWMutex
	// channel to trigger processing a measurement following call to End func. Channel string is kind/namespace/name
	triggerProcessCh chan string
	enabled          bool
}

type ovnMeasurement struct {
	// time just before ovsdb tx is called
	startTimestamp time.Time
	// time when the nbCfg value and its associated configuration is applied to all nodes
	endTimestamp time.Time
	// OVN measurement complete - start and end timestamps are valid
	complete bool
	// nb_cfg value that started the measurement
	nbCfg int
}

// measurement stores a measurement attempt through OVN-Kubernetes controller and optionally OVN
type measurement struct {
	// kubernetes kind e.g. pod or service
	kind string
	// time when Add is executed
	startTimestamp time.Time
	// time when End is executed
	endTimestamp time.Time
	// if true endTimestamp is valid
	end bool
	// time when this measurement expires. Set during Add
	expiresAt time.Time
	// OVN measurement(s) via AddOVN
	ovnMeasurements []ovnMeasurement
}

// hvCfgUpdate holds the information received from OVN Northbound event handler
type hvCfgUpdate struct {
	// timestamp is in milliseconds
	timestamp int
	hvCfg     int
}

// global variable is needed because this functionality is accessed in many functions
var cdr *ConfigDurationRecorder

// lock for accessing the cdr global variable
var cdrMutex sync.Mutex

func GetConfigDurationRecorder() *ConfigDurationRecorder {
	cdrMutex.Lock()
	defer cdrMutex.Unlock()
	if cdr == nil {
		cdr = &ConfigDurationRecorder{}
	}
	return cdr
}

// removeOVNMeasurements remove any OVN measurements less than or equal argument hvCfg
func removeOVNMeasurements(measurements map[string]measurement, hvCfg int) {
	for kindNamespaceName, m := range measurements {
		var indexToDelete []int
		for i, ovnM := range m.ovnMeasurements {
			if ovnM.nbCfg <= hvCfg {
				indexToDelete = append(indexToDelete, i)
			}
		}
		if len(indexToDelete) == 0 {
			continue
		}
		if len(indexToDelete) == len(m.ovnMeasurements) {
			delete(measurements, kindNamespaceName)
		}
		for _, iDel := range indexToDelete {
			m.ovnMeasurements = removeOVNMeasurement(m.ovnMeasurements, iDel)
		}
		measurements[kindNamespaceName] = m
	}
}

var metricNetworkProgramming prometheus.ObserverVec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "network_programming_duration_seconds",
	Help: "The duration to apply network configuration for a kind (e.g. pod, service, networkpolicy). " +
		"Configuration includes add, update and delete events for each kind.",
	Buckets: merge(
		prometheus.LinearBuckets(0.25, 0.25, 2), // 0.25s, 0.50s
		prometheus.LinearBuckets(1, 1, 59),      // 1s, 2s, 3s, ... 59s
		prometheus.LinearBuckets(60, 5, 12),     // 60s, 65s, 70s, ... 115s
		prometheus.LinearBuckets(120, 30, 11))}, // 2min, 2.5min, 3min, ..., 7min
	[]string{
		"kind",
	})

var metricNetworkProgrammingOVN = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "network_programming_ovn_duration_seconds",
	Help:      "The duration for OVN to apply network configuration",
	Buckets: merge(
		prometheus.LinearBuckets(0.25, 0.25, 2), // 0.25s, 0.50s
		prometheus.LinearBuckets(1, 1, 59),      // 1s, 2s, 3s, ... 59s
		prometheus.LinearBuckets(60, 5, 12),     // 60s, 65s, 70s, ... 115s
		prometheus.LinearBuckets(120, 30, 11))}, // 2min, 2.5min, 3min, ..., 7min
)

// Run monitors the config duration for OVN-Kube master to configure k8 kinds. A measurement maybe allowed and this is
// related to the number of k8 nodes, N [1] and by argument k [2] where there is a probability that 1 out of N*k
// measurement attempts are allowed. If k=0, all measurements are allowed. mUpdatePeriod determines the period to
// process and publish metrics
// [1] 1<N<inf, [2] 0<=k<inf
func (cr *ConfigDurationRecorder) Run(nbClient libovsdbclient.Client, wf *factory.WatchFactory, k float64,
	workerLoopPeriod time.Duration, stop <-chan struct{}) {
	// ** configuration duration recorder - intro **
	// We measure the duration to configure whatever k8 kind (pod, services, etc.) object and optionally its application
	// to all nodes. Metrics record this duration. This will give a rough upper bound of how long it takes OVN-Kubernetes
	// controller container (CMS) and optionally (if AddOVN is called), OVN to configure all nodes under its control.
	// Not every attempt to record will result in a measurement if argument k > 0. The measurement rate is proportional to
	// the number of nodes, N and argument k. 1 out of every N*k attempted measurements will succeed.

	// For the optional OVN measurement by calling AddOVN, when the CMS is about to make a transaction to configure
	// whatever kind, a call to AddOVN function allows the caller to measure OVN duration.
	// An ovsdb operation is returned to the caller of AddOVN, which they can bundle with their existing transactions
	// sent to OVN which will tell OVN to measure how long it takes to configure all nodes with the config in the transaction.
	// Config duration then waits for OVN to configure all nodes and calculates the time delta.

	// ** configuration duration recorder - caveats **
	// For the optional OVN recording, it does not give you an exact time duration for how long it takes to configure your
	// k8 kind. When you are recording how long it takes OVN to complete your configuration to all nodes, other
	// transactions may have occurred which may increases the overall time. You may also get longer processing times if one
	// or more nodes are unavailable because we are measuring how long the functionality takes to apply to ALL nodes.

	// ** configuration duration recorder - How the duration of the config is measured within OVN **
	// We increment the nb_cfg integer value in the NB_Global table.
	// ovn-northd notices the nb_cfg change and copies the nb_cfg value to SB_Global table field nb_cfg along with any
	// other configuration that is changed in OVN Northbound database.
	// All ovn-controllers detect nb_cfg value change and generate a 'barrier' on the openflow connection to the
	// nodes ovs-vswitchd. Once ovn-controllers receive the 'barrier processed' reply from ovs-vswitchd which
	// indicates that all relevant openflow operations associated with NB_Globals nb_cfg value have been
	// propagated to the nodes OVS, it copies the SB_Global nb_cfg value to its Chassis_Private table nb_cfg record.
	// ovn-northd detects changes to the Chassis_Private startRecords and computes the minimum nb_cfg for all Chassis_Private
	// nb_cfg and stores this in NB_Global hv_cfg field along with a timestamp to field hv_cfg_timestamp which
	// reflects the time when the slowest chassis catches up with the northbound configuration.
	configDurationRegOnce.Do(func() {
		prometheus.MustRegister(metricNetworkProgramming)
		prometheus.MustRegister(metricNetworkProgrammingOVN)
	})

	cr.measurements = make(map[string]measurement)
	// watch node count and adjust measurement rate if node count changes
	cr.runMeasurementRateAdjuster(wf, k, time.Hour, stop)
	// we currently do not clean the following channels up upon exit
	cr.triggerProcessCh = make(chan string, processChSize)
	updateOVNMeasurementCh := make(chan hvCfgUpdate, updateOVNMeasurementChSize)
	deleteOVNMeasurementCh := make(chan int, deleteOVNMeasurementChSize)
	go cr.processMeasurements(workerLoopPeriod, updateOVNMeasurementCh, deleteOVNMeasurementCh, stop)

	nbClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != nbGlobalTable {
				return
			}
			oldRow := old.(*nbdb.NBGlobal)
			newRow := new.(*nbdb.NBGlobal)

			if oldRow.HvCfg != newRow.HvCfg && oldRow.HvCfgTimestamp != newRow.HvCfgTimestamp && newRow.HvCfgTimestamp > 0 {
				select {
				case updateOVNMeasurementCh <- hvCfgUpdate{hvCfg: newRow.HvCfg, timestamp: newRow.HvCfgTimestamp}:
				default:
					klog.Warning("Config duration recorder: unable to update OVN measurement")
					select {
					case deleteOVNMeasurementCh <- newRow.HvCfg:
					default:
					}
				}
			}
		},
	})
	cr.enabled = true
}

// Start allows the caller to attempt measurement of a control plane configuration duration, as a metric,
// the duration between functions Start and End. Optionally, if you wish to record OVN config duration,
// call AddOVN which will add the duration for OVN to apply the configuration to all nodes.
// The caller must pass kind,namespace,name which will be used to determine if the object
// is allowed to record. To allow no locking, each go routine that calls this function, can determine itself
// if it is allowed to measure.
// There is a mandatory two-step process to complete a measurement.
// Step 1) Call Start when you wish to begin a measurement - ideally when processing for the object starts
// Step 2) Call End which will complete a measurement
// Optionally, call AddOVN when you are making a transaction to OVN in order to add on the OVN duration to an existing
// measurement. This must be called between Start and End. Not every call to Start will result in a measurement
// and the rate of measurements depends on the number of nodes and function Run arg k.
// Only one measurement for a kind/namespace/name is allowed until the current measurement is Ended (via End) and
// processed. This is guaranteed by workqueues (even with multiple workers) and informer event handlers.
func (cr *ConfigDurationRecorder) Start(kind, namespace, name string) (time.Time, bool) {
	if !cr.enabled {
		return time.Time{}, false
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return time.Time{}, false
	}
	measurementTimestamp := time.Now()
	cr.measurementsMu.Lock()
	_, found := cr.measurements[kindNamespaceName]
	// we only record for measurements that aren't in-progress
	if !found {
		cr.measurements[kindNamespaceName] = measurement{kind: kind, startTimestamp: measurementTimestamp,
			expiresAt: measurementTimestamp.Add(maxMeasurementLifetime)}
	}
	cr.measurementsMu.Unlock()
	return measurementTimestamp, !found
}

// allowedToMeasure determines if we are allowed to measure or not. To avoid the cost of synchronisation by using locks,
// we use probability. For a value of kindNamespaceName that returns true, it will always return true.
func (cr *ConfigDurationRecorder) allowedToMeasure(kindNamespaceName string) bool {
	if cr.measurementRate == 0 {
		return true
	}
	// 1 in measurementRate chance of true
	if hashToNumber(kindNamespaceName)%cr.measurementRate == 0 {
		return true
	}
	return false
}

func (cr *ConfigDurationRecorder) End(kind, namespace, name string) time.Time {
	if !cr.enabled {
		return time.Time{}
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return time.Time{}
	}
	measurementTimestamp := time.Now()
	cr.measurementsMu.Lock()
	if m, ok := cr.measurements[kindNamespaceName]; ok {
		if !m.end {
			m.end = true
			m.endTimestamp = measurementTimestamp
			cr.measurements[kindNamespaceName] = m
			// if there are no OVN measurements, trigger immediate processing
			if len(m.ovnMeasurements) == 0 {
				select {
				case cr.triggerProcessCh <- kindNamespaceName:
				default:
					// doesn't matter if channel is full because the measurement will be processed later anyway
				}
			}
		}
	} else {
		// This can happen if Start was rejected for a resource because a measurement was in-progress for this
		// kind/namespace/name, but during execution of this resource, the measurement was completed and now no record
		// is found.
		measurementTimestamp = time.Time{}
	}
	cr.measurementsMu.Unlock()
	return measurementTimestamp
}

// AddOVN adds OVN config duration to an existing recording - previously started by calling function Start
// It will return ovsdb operations which a user can add to existing operations they wish to track.
// Upon successful transaction of the operations to the ovsdb server, the user of this function must call a call-back
// function to lock-in the request to measure and report. Failure to call the call-back function, will result in no OVN
// measurement and no metrics are reported. AddOVN will result in a no-op if Start isn't called previously for the same
// kind/namespace/name.
// If multiple AddOVN is called between Start and End for the same kind/namespace/name, then the
// OVN durations will be summed and added to the total. There is an assumption that processing of kind/namespace/name is
// sequential
func (cr *ConfigDurationRecorder) AddOVN(nbClient libovsdbclient.Client, kind, namespace, name string) (
	[]ovsdb.Operation, func(), time.Time, error) {
	if !cr.enabled {
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	cr.measurementsMu.RLock()
	m, ok := cr.measurements[kindNamespaceName]
	cr.measurementsMu.RUnlock()
	if !ok {
		// no measurement found, therefore no-op
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	if m.end {
		// existing measurement in-progress and not processed yet, therefore no-op
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	nbGlobal := &nbdb.NBGlobal{}
	nbGlobal, err := libovsdbops.GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("failed to find OVN Northbound NB_Global table"+
			" entry: %v", err)
	}
	if nbGlobal.NbCfg < 0 {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("nb_cfg is negative, failed to add OVN measurement")
	}
	//stop recording if we are close to overflow
	if nbGlobal.NbCfg > maxNbCfg {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("unable to measure OVN due to nb_cfg being close to overflow")
	}
	ops, err := nbClient.Where(nbGlobal).Mutate(nbGlobal, model.Mutation{
		Field:   &nbGlobal.NbCfg,
		Mutator: ovsdb.MutateOperationAdd,
		Value:   1,
	})
	if err != nil {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("failed to create update operation: %v", err)
	}
	ovnStartTimestamp := time.Now()

	return ops, func() {
		// there can be a race condition here where we queue the wrong nbCfg value, but it is ok as long as it is
		// less than or equal the hv_cfg value we see and this is the case because of atomic increments for nb_cfg
		cr.measurementsMu.Lock()
		m, ok = cr.measurements[kindNamespaceName]
		if !ok {
			klog.Errorf("Config duration recorder: expected a measurement entry. Call Start before AddOVN"+
				" for %s", kindNamespaceName)
			cr.measurementsMu.Unlock()
			return
		}
		m.ovnMeasurements = append(m.ovnMeasurements, ovnMeasurement{startTimestamp: ovnStartTimestamp,
			nbCfg: nbGlobal.NbCfg + 1})
		cr.measurements[kindNamespaceName] = m
		cr.measurementsMu.Unlock()
	}, ovnStartTimestamp, nil
}

// runMeasurementRateAdjuster will adjust the rate of measurements based on the number of nodes in the cluster and arg k
func (cr *ConfigDurationRecorder) runMeasurementRateAdjuster(wf *factory.WatchFactory, k float64, nodeCheckPeriod time.Duration,
	stop <-chan struct{}) {
	var currentMeasurementRate, newMeasurementRate uint64

	updateMeasurementRate := func() {
		if nodeCount, err := getNodeCount(wf); err != nil {
			klog.Errorf("Config duration recorder: failed to update ticker duration considering node count: %v", err)
		} else {
			newMeasurementRate = uint64(math.Round(k * float64(nodeCount)))
			if newMeasurementRate != currentMeasurementRate {
				if newMeasurementRate > 0 {
					currentMeasurementRate = newMeasurementRate
					cr.measurementRate = newMeasurementRate
				}
				klog.V(5).Infof("Config duration recorder: updated measurement rate to approx 1 in"+
					" every %d requests", newMeasurementRate)
			}
		}
	}

	// initial measurement rate adjustment
	updateMeasurementRate()

	go func() {
		nodeCheckTicker := time.NewTicker(nodeCheckPeriod)
		for {
			select {
			case <-nodeCheckTicker.C:
				updateMeasurementRate()
			case <-stop:
				nodeCheckTicker.Stop()
				return
			}
		}
	}()
}

// processMeasurements manages the measurements map. It calculates metrics and cleans up finished or stale measurements
func (cr *ConfigDurationRecorder) processMeasurements(period time.Duration, updateOVNMeasurementCh chan hvCfgUpdate,
	deleteOVNMeasurementCh chan int, stop <-chan struct{}) {
	ticker := time.NewTicker(period)
	var ovnKDelta, ovnDelta float64

	for {
		select {
		case <-stop:
			ticker.Stop()
			return
		// remove measurements if channel updateOVNMeasurementCh overflows, therefore we cannot trust existing measurements
		case hvCfg := <-deleteOVNMeasurementCh:
			cr.measurementsMu.Lock()
			removeOVNMeasurements(cr.measurements, hvCfg)
			cr.measurementsMu.Unlock()
		case h := <-updateOVNMeasurementCh:
			cr.measurementsMu.Lock()
			cr.addHvCfg(h.hvCfg, h.timestamp)
			cr.measurementsMu.Unlock()
		// used for processing measurements that didn't require OVN measurement. Helps to keep measurement map small
		case kindNamespaceName := <-cr.triggerProcessCh:
			cr.measurementsMu.Lock()
			m, ok := cr.measurements[kindNamespaceName]
			if !ok {
				klog.Errorf("Config duration recorder: expected measurement, but not found")
				cr.measurementsMu.Unlock()
				continue
			}
			if !m.end {
				cr.measurementsMu.Unlock()
				continue
			}
			if len(m.ovnMeasurements) != 0 {
				cr.measurementsMu.Unlock()
				continue
			}
			ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
			metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta)
			klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller took %v"+
				" seconds. No OVN measurement.", kindNamespaceName, ovnKDelta)
			delete(cr.measurements, kindNamespaceName)
			cr.measurementsMu.Unlock()
		// used for processing measurements that require OVN measurement or do not or are expired.
		case <-ticker.C:
			start := time.Now()
			cr.measurementsMu.Lock()
			// process and clean up measurements
			for kindNamespaceName, m := range cr.measurements {
				if start.After(m.expiresAt) {
					// measurement may expire if OVN is degraded or End wasn't called
					klog.Warningf("Config duration recorder: measurement expired for %s", kindNamespaceName)
					delete(cr.measurements, kindNamespaceName)
					continue
				}
				if !m.end {
					// measurement didn't end yet, process later
					continue
				}
				// for when no ovn measurements requested
				if len(m.ovnMeasurements) == 0 {
					ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
					metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta)
					klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller"+
						" took %v seconds. No OVN measurement.", kindNamespaceName, ovnKDelta)
					delete(cr.measurements, kindNamespaceName)
					continue
				}
				// for each kind/namespace/name, there can be multiple calls to AddOVN between start and end
				// we sum all the OVN durations and add it to the start and end duration
				// first lets make sure all OVN measurements are finished
				if complete := allOVNMeasurementsComplete(m.ovnMeasurements); !complete {
					continue
				}

				ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
				ovnDelta = calculateOVNDuration(m.ovnMeasurements)
				metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta + ovnDelta)
				metricNetworkProgrammingOVN.Observe(ovnDelta)
				klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller took"+
					" %v seconds. OVN took %v seconds. Total took %v seconds", kindNamespaceName, ovnKDelta,
					ovnDelta, ovnDelta+ovnKDelta)
				delete(cr.measurements, kindNamespaceName)
			}
			cr.measurementsMu.Unlock()
		}
	}
}

func (cr *ConfigDurationRecorder) addHvCfg(hvCfg, hvCfgTimestamp int) {
	var altered bool
	for i, m := range cr.measurements {
		altered = false
		for iOvnM, ovnM := range m.ovnMeasurements {
			if ovnM.complete {
				continue
			}
			if ovnM.nbCfg <= hvCfg {
				ovnM.endTimestamp = time.UnixMilli(int64(hvCfgTimestamp))
				ovnM.complete = true
				m.ovnMeasurements[iOvnM] = ovnM
				altered = true
			}
		}
		if altered {
			cr.measurements[i] = m
		}
	}
}

func getNodeCount(wf *factory.WatchFactory) (int, error) {
	nodes, err := wf.GetNodes()
	if err != nil {
		return 0, fmt.Errorf("unable to retrieve node list: %v", err)
	}
	return len(nodes), nil
}

func removeOVNMeasurement(oM []ovnMeasurement, i int) []ovnMeasurement {
	oM[i] = oM[len(oM)-1]
	return oM[:len(oM)-1]
}
func hashToNumber(s string) uint64 {
	h := fnv.New64()
	h.Write([]byte(s))
	return h.Sum64()
}

func calculateOVNDuration(ovnMeasurements []ovnMeasurement) float64 {
	var totalDuration float64
	for _, oM := range ovnMeasurements {
		if !oM.complete {
			continue
		}
		totalDuration += oM.endTimestamp.Sub(oM.startTimestamp).Seconds()
	}
	return totalDuration
}

func allOVNMeasurementsComplete(ovnMeasurements []ovnMeasurement) bool {
	for _, oM := range ovnMeasurements {
		if !oM.complete {
			return false
		}
	}
	return true
}

// merge direct copy from k8 pkg/proxy/metrics/metrics.go
func merge(slices ...[]float64) []float64 {
	result := make([]float64, 1)
	for _, s := range slices {
		result = append(result, s...)
	}
	return result
}
