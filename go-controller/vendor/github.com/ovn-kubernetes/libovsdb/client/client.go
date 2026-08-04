package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/mapper"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/ovn-kubernetes/libovsdb/ovsdb/serverdb"
)

// Constants defined for libovsdb
const (
	SSL  = "ssl"
	TCP  = "tcp"
	UNIX = "unix"
)

const serverDB = "_Server"

// ErrNotConnected is an error returned when the client is not connected
var ErrNotConnected = errors.New("not connected")

// ErrAlreadyConnected is an error returned when the client is already connected
var ErrAlreadyConnected = errors.New("already connected")

// ErrUnsupportedRPC is an error returned when an unsupported RPC method is called
var ErrUnsupportedRPC = errors.New("unsupported rpc")

// Client represents an OVSDB Client Connection
// It provides all the necessary functionality to Connect to a server,
// perform transactions, and build your own replica of the database with
// Monitor or MonitorAll. It also provides a Cache that is populated from OVSDB
// update notifications.
type Client interface {
	Connect(context.Context) error
	Disconnect()
	Close()
	Schema() ovsdb.DatabaseSchema
	Cache() *cache.TableCache
	UpdateEndpoints([]string)
	SetOption(Option) error
	Connected() bool
	DisconnectNotify() chan struct{}
	Echo(context.Context) error
	Transact(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error)
	Monitor(context.Context, *Monitor) (MonitorCookie, error)
	MonitorAll(context.Context) (MonitorCookie, error)
	MonitorCancel(ctx context.Context, cookie MonitorCookie) error
	NewMonitor(...MonitorOption) *Monitor
	CurrentEndpoint() string
	API
	// GetSelectResultsByIndex parses the result of the select operation indicated by
	// the 0-based index from the transaction results of the provided operations.
	GetSelectResultsByIndex(ops []ovsdb.Operation, results []ovsdb.OperationResult, target interface{}, index int) error

	// GetSelectResults parses the result of the first select operation from the
	// transaction results of the provided operations.
	GetSelectResults(ops []ovsdb.Operation, results []ovsdb.OperationResult, target interface{}) error
}

type bufferedUpdate struct {
	updates   *ovsdb.TableUpdates
	updates2  *ovsdb.TableUpdates2
	lastTxnID string
}

type epInfo struct {
	address  string
	serverID string
}

// ovsdbClient is an OVSDB client. Connection lifecycle and RPC are delegated to the connection manager.
type ovsdbClient struct {
	options       *options
	metrics       metrics
	primaryDBName string
	databases     map[string]*database

	errorCh    chan error
	stopCh     chan struct{} // closed when client is Close()d to stop cache and event loop
	disconnect chan struct{} // sent one value when CM sends EventDisconnected (for DisconnectNotify API)
	doneCh     chan struct{} // closed when client is Close()d so event loop exits

	handlerShutdown *sync.WaitGroup
	closeOnce       sync.Once
	eventLoopWg     sync.WaitGroup // waited in Close() so event loop and error handler exit before stopCh is closed

	cm          *ConnectionManager
	lifecycleCh chan Event // receives EventDisconnected/Reconnected/SchemaData; blocking send from CM
	eventCh     chan Event // receives EventInboundRPC; non-blocking send from CM
	logger      *logr.Logger

	// currentEndpointServerID is set on Connect/Reconnected so watchForLeaderChange can use it without blocking on CM.
	currentEndpointServerIDMu sync.Mutex
	currentEndpointServerID   string
}

// database is everything needed to map between go types and an ovsdb Database
type database struct {
	// model encapsulates the database schema and model of the database we're connecting to
	model model.DatabaseModel
	// modelMutex protects model from being replaced (via reconnect) while in use
	modelMutex sync.RWMutex

	// cache is used to store the updates for monitored tables
	cache *cache.TableCache
	// cacheMutex protects cache from being replaced (via reconnect) while in use
	cacheMutex sync.RWMutex

	api API

	// any ongoing monitors, so we can re-create them if we disconnect
	monitors      map[string]*Monitor
	monitorsMutex sync.Mutex

	// tracks any outstanding updates while waiting for a monitor response
	deferUpdates    bool
	deferredUpdates []*bufferedUpdate
}

// NewOVSDBClient creates a new OVSDB Client with the provided
// database model. The client can be configured using one or more Option(s),
// like WithTLSConfig. If no WithEndpoint option is supplied, the default of
// unix:/var/run/openvswitch/ovsdb.sock is used
func NewOVSDBClient(clientDBModel model.ClientDBModel, opts ...Option) (Client, error) {
	return newOVSDBClient(clientDBModel, opts...)
}

// newOVSDBClient creates a new ovsdbClient
func newOVSDBClient(clientDBModel model.ClientDBModel, opts ...Option) (*ovsdbClient, error) {
	ovs := &ovsdbClient{
		primaryDBName: clientDBModel.Name(),
		databases: map[string]*database{
			clientDBModel.Name(): {
				model:           model.NewPartialDatabaseModel(clientDBModel),
				monitors:        make(map[string]*Monitor),
				deferUpdates:    true,
				deferredUpdates: make([]*bufferedUpdate, 0),
			},
		},
		errorCh:         make(chan error),
		handlerShutdown: &sync.WaitGroup{},
		disconnect:      make(chan struct{}, 1), // buffered so one DisconnectNotify is never dropped
		doneCh:          make(chan struct{}),
		stopCh:          make(chan struct{}),
		lifecycleCh:     make(chan Event, 8),
		eventCh:         make(chan Event, 64),
	}
	var err error
	ovs.options, err = newOptions(opts...)
	if err != nil {
		return nil, err
	}

	if ovs.options.logger == nil {
		logger := logr.Discard()
		ovs.logger = &logger
	} else {
		l := ovs.options.logger.WithValues("database", ovs.primaryDBName)
		ovs.logger = &l
	}
	ovs.metrics.init(clientDBModel.Name(), ovs.options.metricNamespace, ovs.options.metricSubsystem)
	ovs.registerMetrics()

	if ovs.options.leaderOnly {
		sm, err := serverdb.FullDatabaseModel()
		if err != nil {
			return nil, fmt.Errorf("could not initialize model _Server: %w", err)
		}
		ovs.databases[serverDB] = &database{
			model:    model.NewPartialDatabaseModel(sm),
			monitors: make(map[string]*Monitor),
		}
	}

	dbNames := make([]string, 0, len(ovs.databases))
	for name := range ovs.databases {
		dbNames = append(dbNames, name)
	}
	ovs.cm = NewConnectionManager(ovs.options, ovs.primaryDBName, dbNames, ovs.lifecycleCh, ovs.eventCh)
	go ovs.cm.Run()
	ovs.eventLoopWg.Add(2)
	go func() {
		defer ovs.eventLoopWg.Done()
		ovs.runEventLoop()
	}()
	go func() {
		defer ovs.eventLoopWg.Done()
		ovs.runErrorHandler()
	}()

	return ovs, nil
}

// runEventLoop dispatches events from the connection manager.
// Lifecycle events (lifecycleCh) are given priority over inbound updates (eventCh).
func (o *ovsdbClient) runEventLoop() {
	for {
		// Drain any pending lifecycle event before blocking on updates.
		select {
		case <-o.doneCh:
			return
		case ev := <-o.lifecycleCh:
			o.dispatchEvent(ev)
			continue
		default:
		}
		select {
		case <-o.doneCh:
			return
		case ev := <-o.lifecycleCh:
			o.dispatchEvent(ev)
		case ev := <-o.eventCh:
			o.dispatchEvent(ev)
		}
	}
}

// dispatchEvent processes a single event from the connection manager.
func (o *ovsdbClient) dispatchEvent(ev Event) {
	switch ev.Type {
	case EventDisconnected:
		// Purge caches and reset deferUpdates on all databases so that:
		// 1. Stale data is not served while disconnected.
		// 2. Any incoming server-push notifications during the subsequent reconnect
		//    phase are buffered rather than applied directly to the stale cache.
		for _, db := range o.databases {
			db.modelMutex.RLock()
			dbModel := db.model
			db.modelMutex.RUnlock()
			db.cacheMutex.Lock()
			if db.cache != nil {
				db.cache.Purge(dbModel)
			}
			db.deferUpdates = true
			db.deferredUpdates = make([]*bufferedUpdate, 0)
			db.cacheMutex.Unlock()
		}
		select {
		case o.disconnect <- struct{}{}:
		default:
			// Buffer full or no listener; leave previous value for next receiver
		}
	case EventReconnected, EventSchemaData:
		o.setCurrentEndpointServerID(ev.EndpointServerID)
		if ev.Schema != nil {
			o.applySchema(ev.Schema, true)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-o.doneCh:
					cancel()
				case <-ctx.Done():
				}
			}()
			o.reestablishMonitors(ctx)
			cancel()
		}
	case EventInboundRPC:
		if ev.Inbound != nil {
			o.applyInboundRPC(ev.Inbound)
		}
	}
}

// applySchema updates each database's model and cache from the schema payload.
// When reconnecting is true (called from EventReconnected handler), reestablishMonitors()
// will be called immediately after, so we set deferUpdates=true to buffer any
// server-push notifications that arrive before the monitors are fully reestablished.
// For manual Connect() calls (reconnecting=false) the user must explicitly call Monitor()
// so we leave deferUpdates at its current value (already set to true by EventDisconnected).
func (o *ovsdbClient) applySchema(schemaPayload SchemaPayload, reconnecting bool) {
	for dbName, schema := range schemaPayload {
		db, ok := o.databases[dbName]
		if !ok {
			continue
		}
		db.modelMutex.Lock()
		var errs []error
		db.model, errs = model.NewDatabaseModel(schema, db.model.Client())
		db.modelMutex.Unlock()
		if len(errs) > 0 {
			o.logger.V(2).Info("schema validation errors for database", "db", dbName, "errors", errs)
			continue
		}
		db.cacheMutex.Lock()
		if db.cache == nil {
			var err error
			db.cache, err = cache.NewTableCache(db.model, nil, o.logger)
			if err != nil {
				db.cacheMutex.Unlock()
				continue
			}
			dbNameForWait := dbName
			dbForWait := db
			db.api = newAPI(db.cache, o.logger, o.options.validateModel, func(ctx context.Context) func() {
				waitForCacheConsistent(ctx, dbForWait, o.logger, dbNameForWait)
				return dbForWait.cacheMutex.RUnlock
			})
			o.handlerShutdown.Add(1)
			go func() {
				defer o.handlerShutdown.Done()
				db.cache.Run(o.stopCh)
			}()
		} else if reconnecting {
			// Cache already exists (auto-reconnect path). Reset deferUpdates so that any
			// server-push notifications arriving before reestablishMonitors completes
			// are buffered and replayed rather than applied to the (possibly stale) cache.
			db.deferUpdates = true
			db.deferredUpdates = make([]*bufferedUpdate, 0)
		} else {
			// Manual Connect() path: cache exists but no automatic monitor reestablishment.
			// Clear deferUpdates so that reads from the cache work immediately (returning
			// ErrNotFound since the cache was purged on disconnect). The user must call
			// Monitor() explicitly; at that point deferUpdates will be set correctly.
			// Any server-push notifications before Monitor() is called are unexpected
			// (the server won't send them without a monitor) so no buffering is needed.
			db.deferUpdates = false
			db.deferredUpdates = make([]*bufferedUpdate, 0)
		}
		db.cacheMutex.Unlock()
	}
}

// reestablishMonitors re-sends monitor RPCs for all known monitors (after reconnect).
// ctx should be cancelled when the client is closing so the RPC calls are unblocked.
func (o *ovsdbClient) reestablishMonitors(ctx context.Context) {
	for dbName, db := range o.databases {
		db.monitorsMutex.Lock()
		for id, mon := range db.monitors {
			cookie := MonitorCookie{DatabaseName: dbName, ID: id}
			_ = o.monitor(ctx, cookie, true, mon)
		}
		db.monitorsMutex.Unlock()
	}
}

// applyInboundRPC applies server-push update/update2/update3 to the cache.
func (o *ovsdbClient) applyInboundRPC(in *InboundRPCPayload) {
	var reply []any
	switch in.Method {
	case "update":
		_ = o.update(in.Args, &reply)
	case "update2":
		_ = o.update2(in.Args, &reply)
	case "update3":
		_ = o.update3(in.Args, &reply)
	}
}

// Connect opens a connection to an OVSDB Server using the
// endpoint provided when the Client was created.
func (o *ovsdbClient) Connect(ctx context.Context) error {
	// If the client was previously Close()d, the event loop goroutines and cache Run
	// goroutines have all exited. Restart them before sending CmdConnect so that
	// schema events and inbound RPCs are processed after the connection is established.
	select {
	case <-o.doneCh:
		// Previously closed — rebuild event loop infrastructure.
		o.doneCh = make(chan struct{})
		o.stopCh = make(chan struct{})
		o.closeOnce = sync.Once{}
		// Reset caches to nil so applySchema creates fresh instances and starts new Run goroutines.
		for _, db := range o.databases {
			db.cacheMutex.Lock()
			db.cache = nil
			db.cacheMutex.Unlock()
		}
		o.eventLoopWg.Add(2)
		go func() {
			defer o.eventLoopWg.Done()
			o.runEventLoop()
		}()
		go func() {
			defer o.eventLoopWg.Done()
			o.runErrorHandler()
		}()
	default:
	}
	respCh := make(chan CommandResult, 1)
	select {
	case o.cm.CommandChannel() <- Command{Type: CmdConnect, ResponseCh: respCh, Ctx: ctx}:
	case <-ctx.Done():
		return ctx.Err()
	}
	var r CommandResult
	select {
	case r = <-respCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	if r.Err != nil {
		if r.Err == ErrAlreadyConnected {
			return nil
		}
		return r.Err
	}
	if r.Schema != nil {
		o.applySchema(r.Schema, false)
	}
	o.setCurrentEndpointServerID(r.EndpointServerID)
	if o.options.leaderOnly {
		if err := o.watchForLeaderChange(); err != nil {
			return err
		}
	}
	return nil
}

func (o *ovsdbClient) primaryDB() *database {
	return o.databases[o.primaryDBName]
}

// Schema returns the DatabaseSchema that is being used by the client
// it will be nil until a connection has been established
func (o *ovsdbClient) Schema() ovsdb.DatabaseSchema {
	db := o.primaryDB()
	db.modelMutex.RLock()
	defer db.modelMutex.RUnlock()
	return db.model.Schema
}

// Cache returns the TableCache that is populated from
// ovsdb update notifications. It will be nil until a connection
// has been established, and empty unless you call Monitor
func (o *ovsdbClient) Cache() *cache.TableCache {
	db := o.primaryDB()
	db.cacheMutex.RLock()
	defer db.cacheMutex.RUnlock()
	return db.cache
}

// UpdateEndpoints sets client endpoints and notifies the connection manager.
func (o *ovsdbClient) UpdateEndpoints(endpoints []string) {
	o.logger.V(3).Info("update endpoints", "endpoints", endpoints)
	if len(endpoints) == 0 {
		endpoints = []string{defaultUnixEndpoint}
	}
	o.options.endpoints = endpoints
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{Type: CmdUpdateEndpoints, UpdateEndpoints: endpoints, ResponseCh: respCh}
	<-respCh
}

// SetOption sets a new value for an option. It may only be called when the client is not connected.
func (o *ovsdbClient) SetOption(opt Option) error {
	if o.cm.Connected() {
		return fmt.Errorf("cannot set option when client is connected")
	}
	if err := opt(o.options); err != nil {
		return err
	}
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{Type: CmdUpdateOptions, UpdateOptions: o.options, ResponseCh: respCh}
	<-respCh
	return nil
}

// Connected returns whether or not the client is currently connected to the server.
func (o *ovsdbClient) Connected() bool {
	return o.cm.Connected()
}

func (o *ovsdbClient) CurrentEndpoint() string {
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{Type: CmdQueryEndpoint, ResponseCh: respCh}
	r := <-respCh
	return r.EndpointAddress
}

// getCurrentEndpointInfo returns the active endpoint address and server ID (for tests in same package).
func (o *ovsdbClient) getCurrentEndpointInfo() (address, serverID string) {
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{Type: CmdQueryEndpoint, ResponseCh: respCh}
	r := <-respCh
	return r.EndpointAddress, r.EndpointServerID
}

// DisconnectNotify returns a channel which will notify the caller when the server has disconnected.
func (o *ovsdbClient) DisconnectNotify() chan struct{} {
	return o.disconnect
}

// RFC 7047 : Section 4.1.6 : Echo
func (o *ovsdbClient) echo(args []any, reply *[]any) error {
	*reply = args
	return nil
}

// RFC 7047 : Update Notification Section 4.1.6
// params is an array of length 2: [json-value, table-updates]
// - json-value: the arbitrary json-value passed when creating the Monitor, i.e. the "cookie"
// - table-updates: map of table name to table-update. Table-update is a map of uuid to (old, new) row paris
func (o *ovsdbClient) update(params []json.RawMessage, reply *[]any) error {
	cookie := MonitorCookie{}
	*reply = []any{}
	if len(params) > 2 {
		return fmt.Errorf("update requires exactly 2 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates
	err = json.Unmarshal(params[1], &updates)
	if err != nil {
		return err
	}
	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}
	o.metrics.numUpdates.WithLabelValues(cookie.DatabaseName).Inc()
	for tableName := range updates {
		o.metrics.numTableUpdates.WithLabelValues(cookie.DatabaseName, tableName).Inc()
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{&updates, nil, ""})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update(cookie.ID, updates)
	db.cacheMutex.RUnlock()

	if err != nil {
		o.errorCh <- err
	}

	return err
}

// update2 handling from ovsdb-server.7
func (o *ovsdbClient) update2(params []json.RawMessage, reply *[]any) error {
	cookie := MonitorCookie{}
	*reply = []any{}
	if len(params) > 2 {
		return fmt.Errorf("update2 requires exactly 2 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates2
	err = json.Unmarshal(params[1], &updates)
	if err != nil {
		return err
	}
	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{nil, &updates, ""})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update2(cookie, updates)
	db.cacheMutex.RUnlock()

	if err != nil {
		o.errorCh <- err
	}

	return err
}

// update3 handling from ovsdb-server.7
func (o *ovsdbClient) update3(params []json.RawMessage, reply *[]any) error {
	cookie := MonitorCookie{}
	*reply = []any{}
	if len(params) > 3 {
		return fmt.Errorf("update requires exactly 3 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var lastTransactionID string
	err = json.Unmarshal(params[1], &lastTransactionID)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates2
	err = json.Unmarshal(params[2], &updates)
	if err != nil {
		return err
	}
	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{nil, &updates, lastTransactionID})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update2(cookie, updates)
	db.cacheMutex.RUnlock()

	if err == nil {
		db.monitorsMutex.Lock()
		mon := db.monitors[cookie.ID]
		mon.LastTransactionID = lastTransactionID
		db.monitorsMutex.Unlock()
	}

	return err
}

// logFromContext returns a Logger from ctx or return the default logger
func (o *ovsdbClient) logFromContext(ctx context.Context) *logr.Logger {
	if logger, err := logr.FromContext(ctx); err == nil {
		return &logger
	}
	return o.logger
}

// Transact performs the provided Operations on the database (RFC 7047 transact).
func (o *ovsdbClient) Transact(ctx context.Context, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	logger := o.logFromContext(ctx)
	// For non-reconnect clients, fail fast when not connected.
	// For reconnect clients, skip the early exit and let the command go to the CM;
	// if it returns ErrNotConnected the polling loop below will wait for reconnection.
	if !o.cm.Connected() && !o.options.reconnect {
		return nil, ErrNotConnected
	}
	db := o.primaryDB()
	db.modelMutex.RLock()
	schema := db.model.Schema
	db.modelMutex.RUnlock()
	if reflect.DeepEqual(schema, ovsdb.DatabaseSchema{}) {
		if o.options.reconnect {
			// Schema may not be available yet during reconnect; wait for it.
			logger.V(5).Info("schema unknown, waiting for reconnection", "database", o.primaryDBName)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("%w: while awaiting reconnection", ctx.Err())
				case <-ticker.C:
					if o.cm.Connected() {
						return o.Transact(ctx, operation...)
					}
				}
			}
		}
		return nil, fmt.Errorf("cannot transact to database %s: schema unknown", o.primaryDBName)
	}
	if ok := schema.ValidateOperations(operation...); !ok {
		return nil, fmt.Errorf("validation failed for the operation")
	}
	var reply []ovsdb.OperationResult
	args := ovsdb.NewTransactArgs(o.primaryDBName, operation...)
	respCh := make(chan CommandResult, 1)
	cmd := Command{
		Type:       CmdCall,
		CallMethod: "transact",
		CallArgs:   args,
		CallReply:  &reply,
		ResponseCh: respCh,
		Ctx:        ctx,
	}
	select {
	case o.cm.CommandChannel() <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var r CommandResult
	select {
	case r = <-respCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.Err != nil {
		if r.Err == ErrNotConnected && o.options.reconnect {
			logger.V(5).Info("blocking transaction until reconnected", "operations", fmt.Sprintf("%+v", operation))
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("%w: while awaiting reconnection", ctx.Err())
				case <-ticker.C:
					if o.cm.Connected() {
						return o.Transact(ctx, operation...)
					}
				}
			}
		}
		return nil, r.Err
	}
	if logger.V(4).Enabled() {
		logger.V(4).Info("transacted operations", "database", o.primaryDBName)
	}
	return reply, nil
}

// MonitorAll is a convenience method to monitor every table/column
func (o *ovsdbClient) MonitorAll(ctx context.Context) (MonitorCookie, error) {
	m := newMonitor()
	for name := range o.primaryDB().model.Types() {
		m.Tables = append(m.Tables, TableMonitor{Table: name})
	}
	return o.Monitor(ctx, m)
}

// MonitorCancel cancels a previously issued monitor request (RFC 7047 monitor_cancel).
func (o *ovsdbClient) MonitorCancel(ctx context.Context, cookie MonitorCookie) error {
	var reply ovsdb.OperationResult
	args := ovsdb.NewMonitorCancelArgs(cookie)
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{
		Type:       CmdCall,
		CallMethod: "monitor_cancel",
		CallArgs:   args,
		CallReply:  &reply,
		ResponseCh: respCh,
		Ctx:        ctx,
	}
	r := <-respCh
	if r.Err != nil {
		return r.Err
	}
	if reply.Error != "" {
		return fmt.Errorf("error while executing transaction: %s", reply.Error)
	}
	o.primaryDB().monitorsMutex.Lock()
	delete(o.primaryDB().monitors, cookie.ID)
	o.primaryDB().monitorsMutex.Unlock()
	o.metrics.numMonitors.Dec()
	return nil
}

// Monitor will provide updates for a given table/column
// and populate the cache with them. Subsequent updates will be processed
// by the Update Notifications
// RFC 7047 : monitor
func (o *ovsdbClient) Monitor(ctx context.Context, monitor *Monitor) (MonitorCookie, error) {
	cookie := newMonitorCookie(o.primaryDBName)
	db := o.databases[o.primaryDBName]
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	return cookie, o.monitor(ctx, cookie, false, monitor)
}

// If fields is provided, the request will be constrained to the provided columns
// If no fields are provided, all columns will be used
func newMonitorRequest(data *mapper.Info, fields []string, conditions []ovsdb.Condition) (*ovsdb.MonitorRequest, error) {
	var columns []string
	if len(fields) > 0 {
		columns = append(columns, fields...)
	} else {
		for c := range data.Metadata.TableSchema.Columns {
			columns = append(columns, c)
		}
	}
	return &ovsdb.MonitorRequest{Columns: columns, Where: conditions, Select: ovsdb.NewDefaultMonitorSelect()}, nil
}

// sendMonitorRPC sends a monitor RPC via the connection manager and returns tableUpdates and lastTransactionFound.
func (o *ovsdbClient) sendMonitorRPC(ctx context.Context, _ MonitorCookie, monitor *Monitor, args []any) (tableUpdates any, lastTransactionFound bool, err error) {
	respCh := make(chan CommandResult, 1)
	switch monitor.Method {
	case ovsdb.MonitorRPC:
		var reply ovsdb.TableUpdates
		o.cm.CommandChannel() <- Command{
			Type: CmdCall, CallMethod: monitor.Method, CallArgs: args, CallReply: &reply,
			ResponseCh: respCh, Ctx: ctx,
		}
		r := <-respCh
		if r.Err != nil {
			return nil, false, r.Err
		}
		return reply, false, nil
	case ovsdb.ConditionalMonitorRPC:
		var reply ovsdb.TableUpdates2
		o.cm.CommandChannel() <- Command{
			Type: CmdCall, CallMethod: monitor.Method, CallArgs: args, CallReply: &reply,
			ResponseCh: respCh, Ctx: ctx,
		}
		r := <-respCh
		if r.Err != nil {
			return nil, false, r.Err
		}
		return reply, false, nil
	case ovsdb.ConditionalMonitorSinceRPC:
		var reply ovsdb.MonitorCondSinceReply
		o.cm.CommandChannel() <- Command{
			Type: CmdCall, CallMethod: monitor.Method, CallArgs: args, CallReply: &reply,
			ResponseCh: respCh, Ctx: ctx,
		}
		r := <-respCh
		if r.Err != nil {
			return nil, false, r.Err
		}
		if reply.Found {
			monitor.LastTransactionID = reply.LastTransactionID
			return reply.Updates, true, nil
		}
		return reply.Updates, false, nil
	default:
		return nil, false, fmt.Errorf("unsupported monitor method: %v", monitor.Method)
	}
}

// monitor must only be called with a lock on monitorsMutex
//
//gocyclo:ignore
func (o *ovsdbClient) monitor(ctx context.Context, cookie MonitorCookie, reconnecting bool, monitor *Monitor) error {
	if !o.cm.Connected() {
		return ErrNotConnected
	}
	if len(monitor.Errors) != 0 {
		var errString []string
		for _, e := range monitor.Errors {
			errString = append(errString, e.Error())
		}
		return errors.New(strings.Join(errString, ". "))
	}
	if len(monitor.Tables) == 0 {
		return errors.New("at least one table should be monitored")
	}
	dbName := cookie.DatabaseName
	db := o.databases[dbName]
	db.modelMutex.RLock()
	typeMap := db.model.Types()
	requests := make(map[string]ovsdb.MonitorRequest)
	for _, t := range monitor.Tables {
		_, ok := typeMap[t.Table]
		if !ok {
			db.modelMutex.RUnlock()
			return fmt.Errorf("type for table %s does not exist in model", t.Table)
		}
		model, err := db.model.NewModel(t.Table)
		if err != nil {
			db.modelMutex.RUnlock()
			return err
		}
		info, err := db.model.NewModelInfo(model)
		if err != nil {
			db.modelMutex.RUnlock()
			return err
		}
		request, err := newMonitorRequest(info, t.Fields, t.Conditions)
		if err != nil {
			db.modelMutex.RUnlock()
			return err
		}
		requests[t.Table] = *request
	}
	db.modelMutex.RUnlock()

	var args []any
	if monitor.Method == ovsdb.ConditionalMonitorSinceRPC {
		transactionID := emptyUUID
		if reconnecting && len(db.monitors) == 1 {
			transactionID = monitor.LastTransactionID
		}
		args = ovsdb.NewMonitorCondSinceArgs(dbName, cookie, requests, transactionID)
	} else {
		args = ovsdb.NewMonitorArgs(dbName, cookie, requests)
	}

	tableUpdates, lastTransactionFound, err := o.sendMonitorRPC(ctx, cookie, monitor, args)
	if err != nil {
		if err.Error() == "unknown method" {
			if monitor.Method == ovsdb.ConditionalMonitorSinceRPC {
				o.logger.V(3).Error(err, "method monitor_cond_since not supported, falling back to monitor_cond")
				monitor.Method = ovsdb.ConditionalMonitorRPC
				return o.monitor(ctx, cookie, reconnecting, monitor)
			}
			if monitor.Method == ovsdb.ConditionalMonitorRPC {
				o.logger.V(3).Error(err, "method monitor_cond not supported, falling back to monitor")
				monitor.Method = ovsdb.MonitorRPC
				return o.monitor(ctx, cookie, reconnecting, monitor)
			}
		}
		return err
	}

	if !reconnecting {
		db.monitors[cookie.ID] = monitor
		o.metrics.numMonitors.Inc()
	}

	db.cacheMutex.Lock()
	defer db.cacheMutex.Unlock()

	// On reconnect, purge the cache _unless_ the only monitor is a
	// MonitorCondSince one, whose LastTransactionID was known to the
	// server. In this case the reply contains only updates to the existing
	// cache data, while otherwise it includes complete DB data so we must
	// purge to get rid of old rows.
	if reconnecting && (len(db.monitors) > 1 || !lastTransactionFound) {
		db.cache.Purge(db.model)
	}

	if monitor.Method == ovsdb.MonitorRPC {
		u := tableUpdates.(ovsdb.TableUpdates)
		err = db.cache.Populate(u)
	} else {
		u := tableUpdates.(ovsdb.TableUpdates2)
		err = db.cache.Populate2(u)
	}

	if err != nil {
		return err
	}

	// populate any deferred updates
	db.deferUpdates = false
	for _, update := range db.deferredUpdates {
		if update.updates != nil {
			if err = db.cache.Populate(*update.updates); err != nil {
				return err
			}
		}

		if update.updates2 != nil {
			if err = db.cache.Populate2(*update.updates2); err != nil {
				return err
			}
		}
		if len(update.lastTxnID) > 0 {
			db.monitors[cookie.ID].LastTransactionID = update.lastTxnID
		}
	}
	// clear deferred updates for next time
	db.deferredUpdates = make([]*bufferedUpdate, 0)

	return err
}

// Echo tests the liveness of the OVSDB connection.
func (o *ovsdbClient) Echo(ctx context.Context) error {
	args := ovsdb.NewEchoArgs()
	var reply []any
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{
		Type:       CmdCall,
		CallMethod: "echo",
		CallArgs:   args,
		CallReply:  &reply,
		ResponseCh: respCh,
		Ctx:        ctx,
	}
	r := <-respCh
	if r.Err != nil {
		return r.Err
	}
	if !reflect.DeepEqual(args, reply) {
		return fmt.Errorf("incorrect server response: %v, %v", args, reply)
	}
	return nil
}

func (o *ovsdbClient) setCurrentEndpointServerID(sid string) {
	o.currentEndpointServerIDMu.Lock()
	defer o.currentEndpointServerIDMu.Unlock()
	o.currentEndpointServerID = sid
}

func (o *ovsdbClient) getCurrentEndpointServerID() string {
	o.currentEndpointServerIDMu.Lock()
	defer o.currentEndpointServerIDMu.Unlock()
	return o.currentEndpointServerID
}

// watchForLeaderChange sends CmdNotLeader to the connection manager when the connected endpoint loses leadership.
func (o *ovsdbClient) watchForLeaderChange() error {
	updates := make(chan model.Model)
	o.databases[serverDB].cache.AddEventHandler(&cache.EventHandlerFuncs{
		UpdateFunc: func(table string, _, n model.Model) {
			if table == "Database" {
				updates <- n
			}
		},
	})

	m := newMonitor()
	m.Method = ovsdb.ConditionalMonitorRPC
	m.Tables = []TableMonitor{{Table: "Database"}}
	db := o.databases[serverDB]
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	if err := o.monitor(context.Background(), newMonitorCookie(serverDB), false, m); err != nil {
		return err
	}

	go func() {
		for ev := range updates {
			dbInfo, ok := ev.(*serverdb.Database)
			if !ok {
				continue
			}
			if dbInfo.Name != o.primaryDBName {
				continue
			}
			if dbInfo.Model != serverdb.DatabaseModelClustered {
				continue
			}
			var sid string
			if dbInfo.Sid != nil {
				sid = *dbInfo.Sid
			}
			if sid == "" {
				continue
			}
			currentSID := o.getCurrentEndpointServerID()
			if sid == currentSID && !dbInfo.Leader && o.cm.Connected() {
				o.logger.V(3).Info("endpoint lost leader, sending NotLeader to connection manager", "sid", sid)
				respCh := make(chan CommandResult, 1)
				o.cm.CommandChannel() <- Command{Type: CmdNotLeader, ResponseCh: respCh}
				<-respCh
			}
		}
	}()
	return nil
}

// runErrorHandler processes cache errors; on inconsistency it triggers Disconnect (CM will reconnect).
func (o *ovsdbClient) runErrorHandler() {
	var errColumnNotFound *mapper.ErrColumnNotFound
	var errCacheInconsistent *cache.ErrCacheInconsistent
	var errIndexExists *cache.ErrIndexExists
	for {
		select {
		case <-o.doneCh:
			return
		case err, ok := <-o.errorCh:
			if !ok {
				return
			}
			if errors.As(err, &errColumnNotFound) {
				o.logger.V(3).Error(err, "error updating cache, DB schema may be newer than client!")
			} else if errors.As(err, &errCacheInconsistent) || errors.As(err, &errIndexExists) {
				o.logger.V(3).Error(err, "triggering reconnect to rebuild cache")
				for _, db := range o.databases {
					db.monitorsMutex.Lock()
					for _, mon := range db.monitors {
						mon.LastTransactionID = emptyUUID
					}
					db.monitorsMutex.Unlock()
				}
				o.Disconnect()
			} else {
				o.logger.V(3).Error(err, "error updating cache")
			}
		}
	}
}

// Disconnect closes the connection to the OVSDB server. WithReconnect clients will reconnect on next use.
func (o *ovsdbClient) Disconnect() {
	respCh := make(chan CommandResult, 1)
	o.cm.CommandChannel() <- Command{Type: CmdDisconnect, ResponseCh: respCh}
	<-respCh
}

// Close closes the connection and stops the connection manager. The client will not reconnect.
// Close is idempotent; calling it multiple times is safe.
// Shutdown order: (1) send CmdClose and block until CM run loop exits, (2) signal
// DisconnectNotify() callers directly, (3) close doneCh so event loop and error handler exit,
// (4) wait for them, (5) close stopCh, (6) wait for cache handlers.
func (o *ovsdbClient) Close() {
	o.closeOnce.Do(func() {
		respCh := make(chan CommandResult, 1)
		o.cm.CommandChannel() <- Command{Type: CmdClose, ResponseCh: respCh}
		<-respCh
		// Signal DisconnectNotify() callers directly (non-blocking; the channel is buffered
		// so a single pending notification is never dropped). This must happen before
		// closing doneCh so that waiters on DisconnectNotify() are unblocked even if the
		// event loop exits before processing EventDisconnected from the CM's eventCh.
		select {
		case o.disconnect <- struct{}{}:
		default:
		}
		close(o.doneCh)
		o.eventLoopWg.Wait()
		close(o.stopCh)
		o.handlerShutdown.Wait()
	})
}

// Ensures the cache is consistent by evaluating that the client is connected
// and the monitor is fully setup, with the cache populated. Caller must hold
// the database's cache mutex for reading.
func isCacheConsistent(db *database) bool {
	// This works because when a client is disconnected the deferUpdates variable
	// will be set to true. deferUpdates is also protected by the db.cacheMutex.
	// When the client reconnects and then re-establishes the monitor; the final step
	// is to process all deferred updates, set deferUpdates back to false, and unlock cacheMutex
	return !db.deferUpdates
}

// best effort to ensure cache is in a good state for reading. RLocks the
// database's cache before returning; caller must always unlock.
func waitForCacheConsistent(ctx context.Context, db *database, logger *logr.Logger, dbName string) {
	if !hasMonitors(db) {
		db.cacheMutex.RLock()
		return
	}
	// Check immediately as a fastpath
	db.cacheMutex.RLock()
	if isCacheConsistent(db) {
		return
	}
	db.cacheMutex.RUnlock()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.V(3).Info("warning: unable to ensure cache consistency for reading",
				"database", dbName)
			db.cacheMutex.RLock()
			return
		case <-ticker.C:
			db.cacheMutex.RLock()
			if isCacheConsistent(db) {
				return
			}
			db.cacheMutex.RUnlock()
		}
	}
}

func hasMonitors(db *database) bool {
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	return len(db.monitors) > 0
}

// Client API interface wrapper functions
// We add this wrapper to allow users to access the API directly on the
// client object

// Get implements the API interface's Get function
func (o *ovsdbClient) Get(ctx context.Context, model model.Model) error {
	return o.primaryDB().api.Get(ctx, model)
}

// Create implements the API interface's Create function
func (o *ovsdbClient) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	return o.primaryDB().api.Create(models...)
}

// List implements the API interface's List function
func (o *ovsdbClient) List(ctx context.Context, result any) error {
	return o.primaryDB().api.List(ctx, result)
}

// Where implements the API interface's Where function
func (o *ovsdbClient) Where(models ...model.Model) ConditionalAPI {
	return o.primaryDB().api.Where(models...)
}

// WhereAny implements the API interface's WhereAny function
func (o *ovsdbClient) WhereAny(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.primaryDB().api.WhereAny(m, conditions...)
}

// WhereAll implements the API interface's WhereAll function
func (o *ovsdbClient) WhereAll(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.primaryDB().api.WhereAll(m, conditions...)
}

// WhereCache implements the API interface's WhereCache function
func (o *ovsdbClient) WhereCache(predicate any) ConditionalAPI {
	return o.primaryDB().api.WhereCache(predicate)
}

// Select implements the API interface's Select function
func (o *ovsdbClient) Select(m model.Model, fields ...any) ([]ovsdb.Operation, error) {
	return o.primaryDB().api.Select(m, fields...)
}

// GetSelectResultsByIndex parses the results of a transaction containing select operations
// and populates the target slice with the specified select query's results.
// The index parameter specifies which select query to retrieve (0-based).
// Use index=0 for single select queries (WhereAny, WhereCache, etc.).
func (o *ovsdbClient) GetSelectResultsByIndex(ops []ovsdb.Operation, results []ovsdb.OperationResult, target interface{}, index int) error {
	if len(ops) != len(results) {
		return fmt.Errorf("number of operations (%d) and results (%d) must match", len(ops), len(results))
	}

	// Validate target parameter
	slicePtr := reflect.ValueOf(target)
	if slicePtr.Type().Kind() != reflect.Pointer || slicePtr.IsNil() {
		return &ErrWrongType{slicePtr.Type(), "target must be a non-nil pointer to a slice of models"}
	}

	sliceVal := reflect.Indirect(slicePtr)
	if sliceVal.Type().Kind() != reflect.Slice {
		return &ErrWrongType{slicePtr.Type(), "target must be a pointer to a slice of models"}
	}

	// GetSelectResultsByIndex only accepts a pointer to a slice of pointers to models
	modelType := sliceVal.Type().Elem()
	if modelType.Kind() != reflect.Pointer {
		return &ErrWrongType{slicePtr.Type(), "target must be a pointer to a slice of model pointers"}
	}
	modelType = modelType.Elem()

	o.primaryDB().modelMutex.RLock()
	dbModel := o.primaryDB().model
	o.primaryDB().modelMutex.RUnlock()

	// Determine the target table name from the model type
	dummyModel := reflect.New(modelType).Interface().(model.Model)
	info, err := dbModel.NewModelInfo(dummyModel)
	if err != nil {
		return fmt.Errorf("failed to get model info for target type: %w", err)
	}
	targetTable := info.Metadata.TableName

	// Create a map to store merged rows (deduplicated by UUID)
	mergedRows := make(map[string]ovsdb.Row)
	mergeRows := func(result ovsdb.OperationResult) error {
		if result.Error != "" {
			return fmt.Errorf("operation error: %s: %s", result.Error, result.Details)
		}

		for _, row := range result.Rows {
			uuidVal, ok := row["_uuid"]
			if !ok {
				return fmt.Errorf("failed to get UUID from row: %v", row)
			}
			uuid, ok := uuidVal.(ovsdb.UUID)
			if !ok {
				return fmt.Errorf("failed to cast UUID from row: %v", row)
			}
			// Deduplicate by UUID - later results overwrite earlier ones
			// Note different results may have different selected columns
			mergedRows[uuid.GoUUID] = row
		}
		return nil
	}

	// Single pass to find and collect results for the target index
	currentIndex := -1
	var currentCorrelationID string
	for i, op := range ops {
		if op.Op != ovsdb.OperationSelect || op.Table != targetTable {
			continue
		}

		correlationID := ovsdb.GetCorrelationID(op)
		if correlationID != currentCorrelationID {
			currentIndex++
			currentCorrelationID = correlationID
		}

		if currentIndex < index {
			continue
		}
		if currentIndex > index {
			break
		}

		err := mergeRows(results[i])
		if err != nil {
			return err
		}
	}

	if currentIndex < index {
		return fmt.Errorf("index %d is out of range: found %d query groups for table '%s'",
			index, currentIndex+1, targetTable)
	}

	// Populate the target slice with optimized memory allocation
	resultCount := len(mergedRows)

	// Pre-allocate slice with exact capacity to avoid repeated allocations
	if sliceVal.IsNil() || sliceVal.Cap() < resultCount {
		sliceVal.Set(reflect.MakeSlice(sliceVal.Type(), resultCount, resultCount))
	} else {
		// Reuse existing slice but set to exact length
		sliceVal.SetLen(resultCount)
	}

	// Use index-based assignment to avoid append overhead
	var i int
	for uuid, row := range mergedRows {
		model, err := model.CreateModel(dbModel, targetTable, &row, uuid)
		if err != nil {
			return fmt.Errorf("failed to create model: %w", err)
		}
		sliceVal.Index(i).Set(reflect.ValueOf(model))
		i++
	}

	return nil
}

// GetSelectResults parses select operation results from a transaction.
// Equivalent to GetSelectResultsByIndex with index 0 (first select query)
func (o *ovsdbClient) GetSelectResults(ops []ovsdb.Operation, results []ovsdb.OperationResult, target interface{}) error {
	return o.GetSelectResultsByIndex(ops, results, target, 0)
}
