package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/ovn-kubernetes/libovsdb/ovsdb/serverdb"
)

// ConnState represents the FSM state of the connection manager.
type ConnState int32

const (
	StateDisconnected ConnState = iota
	StateConnecting
	StateConnected
)

// inactivityProbeFirstDelay is the delay before the first inactivity probe tick when
// opts.inactivityTimeout is large; min(10ms, timeout/2) so tests can detect unresponsive servers quickly.
const inactivityProbeFirstDelay = 10 * time.Millisecond

func (s ConnState) String() string {
	switch s {
	case StateDisconnected:
		return "Disconnected"
	case StateConnecting:
		return "Connecting"
	case StateConnected:
		return "Connected"
	default:
		return "Unknown"
	}
}

// CommandType identifies the kind of command sent from client to connection manager.
type CommandType int

const (
	CmdConnect CommandType = iota
	CmdDisconnect
	CmdClose
	CmdNotLeader
	// CmdCall runs a single RPC via CallWithContext(ctx, CallMethod, CallArgs, CallReply).
	CmdCall
	CmdUpdateOptions
	CmdUpdateEndpoints
	CmdQueryState
	CmdQueryEndpoint
	// CmdRunInactivityEcho runs one inactivity echo check immediately (test-only; used to verify disconnect on echo failure).
	CmdRunInactivityEcho
)

// EventType identifies the kind of event sent from connection manager to client.
type EventType int

const (
	EventDisconnected EventType = iota
	EventReconnected
	EventSchemaData
	EventInboundRPC
)

// SchemaPayload carries schema per database name (for Connect response or Reconnected event).
type SchemaPayload map[string]ovsdb.DatabaseSchema

// InboundRPCPayload carries server-push RPC data (update/update2/update3) for the client to apply.
type InboundRPCPayload struct {
	Method string            // "update", "update2", "update3"
	Args   []json.RawMessage // client decodes and applies
}

// Command is sent by the client on the command channel.
type Command struct {
	Type       CommandType
	ResponseCh chan<- CommandResult // optional; closed after send or when command is done
	Ctx        context.Context      // for RPC timeout/cancel

	// Payloads per command type (only one is used per command)
	CallMethod      string // for CmdCall: RPC method name (e.g. "transact", "echo", "monitor_cond_since")
	CallArgs        any    // for CmdCall: request payload
	CallReply       any    // for CmdCall: pointer to reply (filled by CM)
	UpdateOptions   *options
	UpdateEndpoints []string
}

// CommandResult is sent on Command.ResponseCh for commands that need a response.
type CommandResult struct {
	Err              error
	Schema           SchemaPayload           // for Connect
	Results          []ovsdb.OperationResult // for Transact
	State            ConnState               // for QueryState
	EndpointAddress  string                  // for QueryEndpoint
	EndpointServerID string                  // for QueryEndpoint
}

// Event is sent by the connection manager on the event channel.
type Event struct {
	Type             EventType
	Schema           SchemaPayload      // for EventReconnected, EventSchemaData
	Inbound          *InboundRPCPayload // for EventInboundRPC
	EndpointServerID string             // for EventReconnected, so client can track current endpoint without blocking on CM
}

// ConnectionManager runs the connection FSM and proxies RPC. Only the run loop goroutine may mutate state.
type ConnectionManager struct {
	state atomic.Int32 // ConnState

	opts          *options
	optsMu        sync.RWMutex
	dbNames       []string
	primaryDBName string

	cmdCh       chan Command
	lifecycleCh chan Event // for EventDisconnected/Reconnected/SchemaData; blocking send, never dropped
	eventCh     chan Event // for EventInboundRPC; non-blocking send, may drop under load

	// inactivity probe: when Connected and opts.inactivityTimeout > 0, a goroutine sends on inactivityProbeCh
	// every inactivityTimeout; run loop performs Echo and on failure closes the connection.
	// inactivityMu protects these so stopInactivityProbe and the probe goroutine / run loop don't race.
	inactivityMu      sync.Mutex
	inactivityProbeCh chan struct{} // nil when not probing
	inactivityStopCh  chan struct{} // closed to stop the probe goroutine

	rpcClient *rpc2.Client
	shutdown  atomic.Bool

	endpoints []*epInfo
	logger    *logr.Logger
}

// NewConnectionManager creates a connection manager.
// lifecycleCh receives EventDisconnected/Reconnected/SchemaData events via a blocking send (must not be full).
// eventCh receives EventInboundRPC events via a non-blocking send (may drop under load).
// dbNames is the list of database names to use for list_dbs/get_schema.
func NewConnectionManager(opts *options, primaryDBName string, dbNames []string, lifecycleCh, eventCh chan Event) *ConnectionManager {
	cm := &ConnectionManager{
		opts:          opts,
		dbNames:       dbNames,
		primaryDBName: primaryDBName,
		cmdCh:         make(chan Command, 32),
		lifecycleCh:   lifecycleCh,
		eventCh:       eventCh,
		logger:        opts.logger,
	}
	if cm.logger == nil {
		l := logr.Discard()
		cm.logger = &l
	}
	cm.state.Store(int32(StateDisconnected))
	// Build initial endpoints from options
	for _, addr := range opts.endpoints {
		cm.endpoints = append(cm.endpoints, &epInfo{address: addr})
	}
	return cm
}

// CommandChannel returns the channel on which the client must send commands. Do not close it.
func (cm *ConnectionManager) CommandChannel() chan<- Command {
	return cm.cmdCh
}

// EventChannel returns the event channel (for tests or when client needs to pass it elsewhere).
func (cm *ConnectionManager) EventChannel() chan Event {
	return cm.eventCh
}

// Connected returns true if the manager is in Connected state.
func (cm *ConnectionManager) Connected() bool {
	return ConnState(cm.state.Load()) == StateConnected
}

// State returns the current FSM state.
func (cm *ConnectionManager) State() ConnState {
	return ConnState(cm.state.Load())
}

// Run starts the connection manager's run loop. It blocks until the command
// channel is closed or becomes permanently idle after CmdClose.
// Must be called once; the client typically runs it in a goroutine.
// Only this run loop goroutine mutates connection state (rpcClient, state, endpoints); RPC and disconnect/inactivity handlers run from here.
func (cm *ConnectionManager) Run() {
	for {
		if cm.shutdown.Load() {
			// After Close(), drain commands with ErrNotConnected so callers don't block.
			// CmdConnect is special: reset shutdown so the run loop resumes normally.
			cmd, ok := <-cm.cmdCh
			if !ok {
				return
			}
			if cmd.Type == CmdConnect {
				cm.shutdown.Store(false)
				cm.handleCommand(cmd)
			} else {
				cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: ErrNotConnected})
			}
			continue
		}
		// Use the current rpcClient's own disconnect channel. Each new connection produces a new
		// channel, so signals from a previous (now-closed) rpcClient are never selected here —
		// no grace timer needed.
		var serverCh <-chan struct{}
		if cm.rpcClient != nil {
			serverCh = cm.rpcClient.DisconnectNotify()
		}
		cm.inactivityMu.Lock()
		probeCh := cm.inactivityProbeCh
		cm.inactivityMu.Unlock()

		select {
		case <-serverCh:
			cm.handleServerDisconnected()

		case <-probeCh:
			// Inactivity probe tick: send Echo; on failure treat as disconnect.
			_ = cm.runInactivityEcho()

		case cmd, ok := <-cm.cmdCh:
			if !ok {
				return
			}
			cm.handleCommand(cmd)
			// If CmdClose was processed, shutdown=true and the next iteration
			// enters drain mode; no explicit action needed here.
		}
	}
}

// handleServerDisconnected clears connection state and either reconnects or notifies client.
func (cm *ConnectionManager) handleServerDisconnected() {
	cm.closeRPCClient()
	cm.state.Store(int32(StateDisconnected))
	cm.sendLifecycleEvent(Event{Type: EventDisconnected})

	if cm.shutdown.Load() {
		return
	}
	cm.optsMu.RLock()
	reconnect := cm.opts.reconnect
	cm.optsMu.RUnlock()
	if reconnect {
		// Transition to Connecting and run connect sequence with backoff (done in handleCommand(CmdConnect) or a dedicated reconnect loop).
		cm.state.Store(int32(StateConnecting))
		cm.runReconnectLoop()
	}
}

// closeRPCClient closes the rpc2 client and clears reference. Caller must be run loop.
func (cm *ConnectionManager) closeRPCClient() {
	cm.stopInactivityProbe()
	if cm.rpcClient == nil {
		return
	}
	cm.rpcClient.Close()
	cm.rpcClient = nil
}

// handleCommand processes one command and returns true if Run should exit (after CmdClose).
func (cm *ConnectionManager) handleCommand(cmd Command) (exit bool) {
	defer func() {
		if cmd.ResponseCh != nil {
			close(cmd.ResponseCh)
		}
	}()

	switch cmd.Type {
	case CmdConnect:
		return cm.doCmdConnect(cmd)
	case CmdDisconnect:
		return cm.doCmdDisconnect(cmd)
	case CmdClose:
		return cm.doCmdClose(cmd)
	case CmdNotLeader:
		return cm.doCmdNotLeader(cmd)
	case CmdUpdateOptions:
		return cm.doCmdUpdateOptions(cmd)
	case CmdUpdateEndpoints:
		return cm.doCmdUpdateEndpoints(cmd)
	case CmdQueryState:
		return cm.doCmdQueryState(cmd)
	case CmdQueryEndpoint:
		return cm.doCmdQueryEndpoint(cmd)
	case CmdRunInactivityEcho:
		return cm.doCmdRunInactivityEcho(cmd)
	case CmdCall:
		return cm.doCmdCall(cmd)
	default:
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: ErrNotConnected})
		return false
	}
}

func (cm *ConnectionManager) sendCmdResult(ch chan<- CommandResult, r CommandResult) {
	if ch != nil {
		ch <- r
	}
}

func (cm *ConnectionManager) doCmdConnect(cmd Command) bool {
	// If already connected or connecting, return ErrAlreadyConnected. Connect() maps this
	// error to nil so callers see a no-op; StateConnecting means a reconnect loop is running
	// and we must not start a second connection attempt.
	current := ConnState(cm.state.Load())
	if current == StateConnected || current == StateConnecting {
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: ErrAlreadyConnected})
		return false
	}
	cm.state.Store(int32(StateConnecting))
	schema, serverID, err := cm.runConnectSequence(cmd.Ctx)
	if err != nil {
		cm.state.Store(int32(StateDisconnected))
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: err})
		return false
	}
	cm.state.Store(int32(StateConnected))
	cm.startInactivityProbe()
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{Schema: schema, EndpointServerID: serverID})
	return false
}

func (cm *ConnectionManager) doCmdDisconnect(cmd Command) bool {
	cm.optsMu.RLock()
	reconnect := cm.opts.reconnect
	cm.optsMu.RUnlock()

	cm.closeRPCClient()
	cm.state.Store(int32(StateDisconnected))

	if reconnect {
		// For reconnect clients (WithInactivityCheck or WithReconnect), treat a user-initiated
		// Disconnect as a signal to reconnect. We send EventDisconnected and then trigger the
		// reconnect loop by calling handleServerDisconnected indirectly: set state to Connecting
		// and run the reconnect loop from here (we are already on the run loop goroutine).
		cm.sendLifecycleEvent(Event{Type: EventDisconnected})
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
		cm.state.Store(int32(StateConnecting))
		cm.runReconnectLoop()
		return false
	}

	cm.sendLifecycleEvent(Event{Type: EventDisconnected})
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
	return false
}

func (cm *ConnectionManager) doCmdClose(cmd Command) bool {
	cm.shutdown.Store(true)
	cm.closeRPCClient()
	cm.state.Store(int32(StateDisconnected))
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
	return true
}

func (cm *ConnectionManager) doCmdNotLeader(cmd Command) bool {
	cm.closeRPCClient()
	cm.state.Store(int32(StateConnecting))
	cm.optsMu.Lock()
	if len(cm.endpoints) > 1 {
		first := cm.endpoints[0]
		cm.endpoints = append(cm.endpoints[1:], first)
	}
	cm.optsMu.Unlock()
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
	cm.runReconnectLoop()
	return false
}

func (cm *ConnectionManager) doCmdUpdateOptions(cmd Command) bool {
	cm.optsMu.Lock()
	if cmd.UpdateOptions != nil {
		cm.opts = cmd.UpdateOptions
		cm.endpoints = make([]*epInfo, 0, len(cm.opts.endpoints))
		for _, a := range cm.opts.endpoints {
			cm.endpoints = append(cm.endpoints, &epInfo{address: a})
		}
	}
	cm.optsMu.Unlock()
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
	return false
}

func (cm *ConnectionManager) doCmdUpdateEndpoints(cmd Command) bool {
	cm.optsMu.Lock()
	currentAddr := ""
	if len(cm.endpoints) > 0 {
		currentAddr = cm.endpoints[0].address
	}
	needReconnect := false
	if len(cmd.UpdateEndpoints) > 0 {
		cm.opts.endpoints = cmd.UpdateEndpoints
		originEps := cm.endpoints
		var newEps []*epInfo
		activeIdx := -1
		for i, addr := range cmd.UpdateEndpoints {
			ep := &epInfo{address: addr}
			for j, origin := range originEps {
				if addr == origin.address {
					if j == 0 {
						activeIdx = i
					}
					ep.serverID = origin.serverID
					break
				}
			}
			newEps = append(newEps, ep)
		}
		cm.endpoints = newEps
		if activeIdx > 0 {
			first := cm.endpoints[activeIdx]
			rest := append(cm.endpoints[:activeIdx], cm.endpoints[activeIdx+1:]...)
			cm.endpoints = append([]*epInfo{first}, rest...)
			cm.optsMu.Unlock()
		} else if activeIdx == -1 && currentAddr != "" && cm.state.Load() == int32(StateConnected) {
			needReconnect = cm.opts.reconnect
			cm.optsMu.Unlock()
			// runReconnectLoop may block for the full backoff duration; commands that arrive
			// during that time are serviced inline by runReconnectLoop's inner select.
			cm.closeRPCClient()
			cm.state.Store(int32(StateDisconnected))
			cm.sendLifecycleEvent(Event{Type: EventDisconnected})
			if needReconnect {
				cm.state.Store(int32(StateConnecting))
				cm.runReconnectLoop()
			}
		} else {
			cm.optsMu.Unlock()
		}
	} else {
		cm.optsMu.Unlock()
	}
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
	return false
}

func (cm *ConnectionManager) doCmdQueryState(cmd Command) bool {
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{State: cm.State()})
	return false
}

func (cm *ConnectionManager) doCmdQueryEndpoint(cmd Command) bool {
	addr, sid := "", ""
	if cm.state.Load() == int32(StateConnected) && len(cm.endpoints) > 0 {
		addr = cm.endpoints[0].address
		sid = cm.endpoints[0].serverID
	}
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{EndpointAddress: addr, EndpointServerID: sid})
	return false
}

func (cm *ConnectionManager) doCmdRunInactivityEcho(cmd Command) bool {
	echoErr := cm.runInactivityEcho()
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: echoErr})
	return false
}

func (cm *ConnectionManager) doCmdCall(cmd Command) bool {
	if cm.rpcClient == nil {
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: ErrNotConnected})
		return false
	}
	cm.runRPCCommand(cmd)
	return false
}

// runConnectSequence attempts to connect (dial, list_dbs, get_schema, leader check). Returns (schema, serverID, err).
func (cm *ConnectionManager) runConnectSequence(ctx context.Context) (SchemaPayload, string, error) {
	cm.optsMu.RLock()
	endpoints := make([]*epInfo, len(cm.endpoints))
	copy(endpoints, cm.endpoints)
	leaderOnly := cm.opts.leaderOnly
	tlsConfig := cm.opts.tlsConfig
	cm.optsMu.RUnlock()

	for i, ep := range endpoints {
		u, err := url.Parse(ep.address)
		if err != nil {
			continue
		}
		conn, err := cm.dial(ctx, u, tlsConfig)
		if err != nil {
			cm.logger.V(3).Info("dial failed", "endpoint", ep.address, "err", err)
			continue
		}
		cm.rpcClient = rpc2.NewClientWithCodec(jsonrpc.NewJSONCodec(conn))
		cm.rpcClient.SetBlocking(true)
		cm.registerInboundHandlers()
		go cm.rpcClient.Run()

		serverDBNames, err := cm.listDbs(ctx)
		if err != nil {
			cm.closeRPCClient()
			cm.logger.V(3).Info("list_dbs failed", "endpoint", ep.address, "err", err)
			continue
		}
		// Ensure all requested DBs exist
		dbSet := make(map[string]bool)
		for _, n := range serverDBNames {
			dbSet[n] = true
		}
		for _, dbName := range cm.dbNames {
			if !dbSet[dbName] {
				cm.closeRPCClient()
				return nil, "", fmt.Errorf("target database %s not found", dbName)
			}
		}

		schemaPayload := make(SchemaPayload)
		for _, dbName := range cm.dbNames {
			schema, err := cm.getSchema(ctx, dbName)
			if err != nil {
				cm.closeRPCClient()
				cm.logger.V(3).Info("get_schema failed", "db", dbName, "err", err)
				return nil, "", err
			}
			schemaPayload[dbName] = schema
		}

		if leaderOnly {
			leader, sid, err := cm.isEndpointLeader(ctx)
			if err != nil {
				cm.closeRPCClient()
				cm.logger.V(3).Info("leader check failed", "err", err)
				continue
			}
			if !leader {
				cm.closeRPCClient()
				cm.logger.V(3).Info("endpoint is not leader", "endpoint", ep.address)
				continue
			}
			ep.serverID = sid
		}

		// Move successful endpoint to front for next time
		if i > 0 {
			first := endpoints[i]
			rest := append(endpoints[:i], endpoints[i+1:]...)
			cm.endpoints = append([]*epInfo{first}, rest...)
		}

		cm.logger.V(3).Info("connected", "endpoint", ep.address)
		return schemaPayload, ep.serverID, nil
	}
	return nil, "", ErrNotConnected
}

func (cm *ConnectionManager) dial(ctx context.Context, u *url.URL, tlsConfig *tls.Config) (net.Conn, error) {
	switch u.Scheme {
	case UNIX:
		var d net.Dialer
		return d.DialContext(ctx, u.Scheme, u.Path)
	case TCP:
		var d net.Dialer
		return d.DialContext(ctx, u.Scheme, u.Opaque)
	case SSL:
		d := tls.Dialer{Config: tlsConfig}
		return d.DialContext(ctx, "tcp", u.Opaque)
	default:
		return nil, fmt.Errorf("unknown scheme %s", u.Scheme)
	}
}

func (cm *ConnectionManager) listDbs(ctx context.Context) ([]string, error) {
	var dbs []string
	err := cm.rpcClient.CallWithContext(ctx, "list_dbs", nil, &dbs)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return nil, ErrNotConnected
		}
		return nil, err
	}
	return dbs, nil
}

func (cm *ConnectionManager) getSchema(ctx context.Context, dbName string) (ovsdb.DatabaseSchema, error) {
	args := ovsdb.NewGetSchemaArgs(dbName)
	var reply ovsdb.DatabaseSchema
	err := cm.rpcClient.CallWithContext(ctx, "get_schema", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return ovsdb.DatabaseSchema{}, ErrNotConnected
		}
		return ovsdb.DatabaseSchema{}, err
	}
	return reply, nil
}

func (cm *ConnectionManager) isEndpointLeader(ctx context.Context) (bool, string, error) {
	op := ovsdb.Operation{
		Op:      ovsdb.OperationSelect,
		Table:   "Database",
		Columns: []string{"name", "model", "leader", "sid"},
	}
	var results []ovsdb.OperationResult
	args := ovsdb.NewTransactArgs(serverDB, op)
	err := cm.rpcClient.CallWithContext(ctx, "transact", args, &results)
	if err != nil {
		return false, "", err
	}
	if len(results) != 1 {
		return true, "", nil
	}
	result := results[0]
	if len(result.Rows) == 0 {
		return true, "", nil
	}
	for _, row := range result.Rows {
		dbName, ok := row["name"].(string)
		if !ok {
			return false, "", fmt.Errorf("could not parse name")
		}
		if dbName != cm.primaryDBName {
			continue
		}
		model, ok := row["model"].(string)
		if !ok {
			return false, "", fmt.Errorf("could not parse model")
		}
		if model != serverdb.DatabaseModelClustered {
			return true, "", nil
		}
		sid, ok := row["sid"].(ovsdb.UUID)
		if !ok {
			return false, "", fmt.Errorf("could not parse server id")
		}
		leader, ok := row["leader"].(bool)
		if !ok {
			return false, "", fmt.Errorf("could not parse leader")
		}
		return leader, sid.GoUUID, nil
	}
	return true, "", nil
}

// registerInboundHandlers registers echo/update/update2/update3 handlers that push to event channel.
func (cm *ConnectionManager) registerInboundHandlers() {
	cm.rpcClient.Handle("echo", func(_ *rpc2.Client, args []any, reply *[]any) error {
		*reply = args
		return nil
	})
	cm.rpcClient.Handle("update", func(_ *rpc2.Client, args []json.RawMessage, reply *[]any) error {
		*reply = []any{}
		cm.sendEvent(Event{Type: EventInboundRPC, Inbound: &InboundRPCPayload{Method: "update", Args: args}})
		return nil
	})
	cm.rpcClient.Handle("update2", func(_ *rpc2.Client, args []json.RawMessage, reply *[]any) error {
		*reply = []any{}
		cm.sendEvent(Event{Type: EventInboundRPC, Inbound: &InboundRPCPayload{Method: "update2", Args: args}})
		return nil
	})
	cm.rpcClient.Handle("update3", func(_ *rpc2.Client, args []json.RawMessage, reply *[]any) error {
		*reply = []any{}
		cm.sendEvent(Event{Type: EventInboundRPC, Inbound: &InboundRPCPayload{Method: "update3", Args: args}})
		return nil
	})
}

// startInactivityProbe starts a goroutine that sends on inactivityProbeCh every inactivityTimeout when opts set.
func (cm *ConnectionManager) startInactivityProbe() {
	cm.optsMu.RLock()
	timeout := cm.opts.inactivityTimeout
	cm.optsMu.RUnlock()
	if timeout <= 0 {
		return
	}
	cm.inactivityMu.Lock()
	cm.inactivityProbeCh = make(chan struct{}, 1)
	cm.inactivityStopCh = make(chan struct{})
	probeCh := cm.inactivityProbeCh
	stopCh := cm.inactivityStopCh
	cm.inactivityMu.Unlock()
	go func() {
		firstDelay := inactivityProbeFirstDelay
		if timeout < firstDelay {
			firstDelay = timeout / 2
		}
		select {
		case <-time.After(firstDelay):
		case <-stopCh:
			return
		}
		select {
		case probeCh <- struct{}{}:
		default:
		}
		for {
			select {
			case <-time.After(timeout):
				select {
				case probeCh <- struct{}{}:
				default:
					// run loop hasn't consumed previous tick
				}
			case <-stopCh:
				return
			}
		}
	}()
}

// stopInactivityProbe stops the inactivity probe goroutine. Safe to call if not started.
func (cm *ConnectionManager) stopInactivityProbe() {
	cm.inactivityMu.Lock()
	if cm.inactivityStopCh != nil {
		close(cm.inactivityStopCh)
		cm.inactivityStopCh = nil
	}
	cm.inactivityProbeCh = nil
	cm.inactivityMu.Unlock()
}

// runInactivityEcho runs a single Echo RPC; on failure triggers a disconnect.
// Called from the run loop goroutine, so it may call handleServerDisconnected directly.
// Returns the error from the echo call (for testing).
func (cm *ConnectionManager) runInactivityEcho() error {
	if cm.rpcClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := ovsdb.NewEchoArgs()
	var reply []any
	err := cm.rpcClient.CallWithContext(ctx, "echo", args, &reply)
	if err != nil {
		cm.logger.V(3).Info("inactivity echo failed, disconnecting", "err", err)
		// Call handleServerDisconnected directly (we are already on the run loop goroutine)
		// so the grace timer does not suppress this legitimate disconnect.
		cm.handleServerDisconnected()
		return err
	}
	return nil
}

// runReconnectLoop runs connect with backoff until success or shutdown.
func (cm *ConnectionManager) runReconnectLoop() {
	cm.optsMu.RLock()
	backoff := cm.opts.backoff
	timeout := cm.opts.timeout
	cm.optsMu.RUnlock()
	if backoff == nil {
		cm.state.Store(int32(StateDisconnected))
		return
	}
	backoff.Reset()
	for !cm.shutdown.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		schema, serverID, err := cm.runConnectSequence(ctx)
		cancel()
		if err == nil {
			cm.state.Store(int32(StateConnected))
			cm.startInactivityProbe()
			cm.sendLifecycleEvent(Event{Type: EventReconnected, Schema: schema, EndpointServerID: serverID})
			return
		}
		next := backoff.NextBackOff()
		if next < 0 {
			cm.logger.V(2).Info("reconnect backoff exhausted, giving up")
			cm.state.Store(int32(StateDisconnected))
			cm.sendLifecycleEvent(Event{Type: EventDisconnected})
			return
		}
		// Wait next or a command that should interrupt the reconnect loop.
		t := time.NewTimer(next)
	waitCmd:
		for {
			select {
			case <-t.C:
				break waitCmd
			case cmd, ok := <-cm.cmdCh:
				if !ok {
					t.Stop()
					return
				}
				switch cmd.Type {
				case CmdClose, CmdDisconnect, CmdNotLeader, CmdUpdateEndpoints:
					// Control or reconnect-triggering commands must abort this loop; the
					// handler (or the caller for CmdClose/CmdDisconnect) takes over.
					t.Stop()
					cm.handleCommand(cmd)
					return
				default:
					// Non-reconnect commands (queries, RPCs) are serviced without
					// abandoning the backoff — they just return ErrNotConnected.
					cm.handleCommand(cmd)
				}
			}
		}
		t.Stop()
	}
	cm.state.Store(int32(StateDisconnected))
}

// runRPCCommand runs a single RPC via CallWithContext(ctx, method, args, reply).
func (cm *ConnectionManager) runRPCCommand(cmd Command) {
	if cm.rpcClient == nil {
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: ErrNotConnected})
		return
	}
	ctx := cmd.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	err := cm.rpcClient.CallWithContext(ctx, cmd.CallMethod, cmd.CallArgs, cmd.CallReply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			err = ErrNotConnected
		}
		cm.sendCmdResult(cmd.ResponseCh, CommandResult{Err: err})
		return
	}
	cm.sendCmdResult(cmd.ResponseCh, CommandResult{})
}

// sendLifecycleEvent sends a lifecycle event (EventDisconnected/Reconnected/SchemaData) on lifecycleCh.
// Blocks until the client consumes it; lifecycleCh has capacity 8 so this is fast in practice.
func (cm *ConnectionManager) sendLifecycleEvent(ev Event) {
	cm.lifecycleCh <- ev
}

// sendEvent sends an inbound-update event (EventInboundRPC) on eventCh.
// Non-blocking: drops the event if eventCh is full. Inbound-update drops are acceptable under bursts.
func (cm *ConnectionManager) sendEvent(ev Event) {
	select {
	case cm.eventCh <- ev:
	default:
		cm.logger.V(5).Info("event channel full, dropping inbound update", "type", ev.Type)
	}
}
