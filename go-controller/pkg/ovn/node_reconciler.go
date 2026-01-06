package ovn

// NodeReconciler queues node keys for reconciliation.
// Used by UDN controllers to requeue node work without per-network node watchers.
type NodeReconciler interface {
	Reconcile(key string)
}
