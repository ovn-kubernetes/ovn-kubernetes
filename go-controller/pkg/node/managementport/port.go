package managementport

import (
	"net"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// Interface holds information about the management port that connects the OVN
// network with the host network
type Interface interface {
	// GetInterfaceName of the management port
	GetInterfaceName() string
	// GetAddresses, bot IPv4 and IPv6, of the management port
	GetAddresses() []*net.IPNet
}

// Controller manages the management port. It has a reconciliation
// loop that needs to be started and can reconcile on request
type Controller interface {
	Interface
	Start(stopChan <-chan struct{}) error
	Reconcile()
}

// managementPort is an internal representation of a device handled by
// Controller. The Controller can handle one or more of them (OVS, netdev,
// representor), the latter being generally when a management port as a whole is
// implemented with more than one device.
type managementPort interface {
	create() error
	reconcilePeriod() time.Duration
	doReconcile() error
}

// GetAddresses, bot IPv4 and IPv6, of the management port
func (c *managementPortController) GetAddresses() []*net.IPNet {
	return c.cfg.getAddresses()
}

func (c *managementPortController) Start(stopChan <-chan struct{}) error {
	return c.start(stopChan)
}

func (c *managementPortController) Reconcile() {
	c.reconcile()
}

func start(mp managementPort, stopChan <-chan struct{}) (func(), error) {
	if mp == nil {
		return func() {}, nil
	}
	err := mp.create()
	if err != nil {
		return func() {}, err
	}
	reconcileCh := make(chan struct{}, 1)
	reconcile := func() { reconcileCh <- struct{}{} }
	go func() {
		timer := time.NewTicker(mp.reconcilePeriod())
		defer timer.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-timer.C:
				reconcile()
			case <-reconcileCh:
				err := retry.OnError(
					wait.Backoff{
						Duration: 10 * time.Millisecond,
						Steps:    4,
						Factor:   5.0,
						Cap:      mp.reconcilePeriod(),
					},
					func(error) bool {
						select {
						case <-stopChan:
							return false
						default:
							return true
						}
					},
					func() error {
						if err := mp.doReconcile(); err != nil {
							// doReconcile may fail if the interface was deleted.
							// In that case, try to recreate it. create() is idempotent
							// and safe to call even if the interface already exists.
							klog.Warningf("Failed to reconcile management port, attempting to recreate: %v", err)
							return mp.create()
						}
						return nil
					},
				)
				if err != nil {
					klog.Errorf("Failed to reconcile management port: %v", err)
				}
			}
			timer.Reset(mp.reconcilePeriod())
		}
	}()
	return reconcile, nil
}
