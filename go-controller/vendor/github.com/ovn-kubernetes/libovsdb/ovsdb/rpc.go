package ovsdb

const (
	// MonitorRPC is the monitor RPC method
	MonitorRPC = "monitor"
	// ConditionalMonitorRPC is the monitor_cond
	ConditionalMonitorRPC = "monitor_cond"
	// ConditionalMonitorSinceRPC is the monitor_cond_since RPC method
	ConditionalMonitorSinceRPC = "monitor_cond_since"
)

// NewEchoArgs creates a new set of arguments for an echo RPC
func NewEchoArgs() []any {
	return []any{"libovsdb echo"}
}

// NewGetSchemaArgs creates a new set of arguments for a get_schemas RPC
func NewGetSchemaArgs(schema string) []any {
	return []any{schema}
}

// NewTransactArgs creates a new set of arguments for a transact RPC
func NewTransactArgs(database string, operations ...Operation) []any {
	dbSlice := make([]any, 1)
	dbSlice[0] = database

	opsSlice := make([]any, len(operations))
	for i, d := range operations {
		opsSlice[i] = d
	}

	ops := append(dbSlice, opsSlice...)
	return ops
}

// NewCancelArgs creates a new set of arguments for a cancel RPC
func NewCancelArgs(id any) []any {
	return []any{id}
}

// NewMonitorArgs creates a new set of arguments for a monitor RPC
func NewMonitorArgs(database string, value any, requests map[string]MonitorRequest) []any {
	return []any{database, value, requests}
}

// NewMonitorCondSinceArgs creates a new set of arguments for a monitor_cond_since RPC
func NewMonitorCondSinceArgs(database string, value any, requests map[string]MonitorRequest, lastTransactionID string) []any {
	return []any{database, value, requests, lastTransactionID}
}

// NewMonitorCancelArgs creates a new set of arguments for a monitor_cancel RPC
func NewMonitorCancelArgs(value any) []any {
	return []any{value}
}

// NewLockArgs creates a new set of arguments for a lock, steal or unlock RPC
func NewLockArgs(id any) []any {
	return []any{id}
}

// NotificationHandler is the interface that must be implemented to receive notifications
type NotificationHandler interface {
	// RFC 7047 section 4.1.6 Update Notification
	Update(context any, tableUpdates TableUpdates)

	// ovsdb-server.7 update2 notifications
	Update2(context any, tableUpdates TableUpdates2)

	// RFC 7047 section 4.1.9 Locked Notification
	Locked([]any)

	// RFC 7047 section 4.1.10 Stolen Notification
	Stolen([]any)

	// RFC 7047 section 4.1.11 Echo Notification
	Echo([]any)

	Disconnected()
}
