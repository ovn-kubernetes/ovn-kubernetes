package dpulease

import (
	"context"
	"fmt"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	// HolderIdentity is used on the DPU node lease object
	HolderIdentity  = "ovnkube-dpu-node"
	leaseNamePrefix = "ovn-dpu-"
)

// Manager handles lifecycle and readiness tracking for the DPU node lease.
type Manager struct {
	client        kubernetes.Interface
	namespace     string
	nodeName      string
	nodeUID       types.UID
	renewInterval time.Duration
	leaseDuration time.Duration

	statusMu sync.RWMutex
	ready    bool
	reason   string
}

// NewManager builds a new Manager.
func NewManager(client kubernetes.Interface, namespace string, node *corev1.Node, renewInterval, leaseDuration time.Duration) *Manager {
	m := &Manager{
		client:        client,
		namespace:     namespace,
		nodeName:      node.Name,
		nodeUID:       node.UID,
		renewInterval: renewInterval,
		leaseDuration: leaseDuration,
	}

	m.setStatus("", true)

	return m
}

// Ready reports the current readiness and message for consumers such as the CNI server.
func (m *Manager) Ready() (bool, string) {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.ready, m.reason
}

// EnsureLease creates or updates the DPU lease.
func (m *Manager) EnsureLease(ctx context.Context) (*coordinationv1.Lease, error) {
	if m.renewInterval == 0 {
		return nil, nil
	}

	var lease *coordinationv1.Lease
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := m.client.CoordinationV1().Leases(m.namespace).Get(ctx, m.leaseName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			now := metav1.NowMicro()
			lease = m.newLease(now)
			created, createErr := m.client.CoordinationV1().Leases(m.namespace).Create(ctx, lease, metav1.CreateOptions{})
			if createErr != nil {
				return createErr
			}
			lease = created
			return nil
		}
		if err != nil {
			return err
		}
		lease = existing.DeepCopy()
		if !m.updateLeaseSpec(lease, metav1.NowMicro(), true) {
			return nil
		}
		updated, updateErr := m.client.CoordinationV1().Leases(m.namespace).Update(ctx, lease, metav1.UpdateOptions{})
		if updateErr != nil {
			return updateErr
		}
		lease = updated
		return nil
	})
	if err != nil {
		m.setStatus(fmt.Sprintf("failed ensuring DPU lease: %v", err), false)
		return nil, err
	}

	m.setStatus("", true)
	return lease, nil
}

// RunUpdater periodically renews the lease heartbeat. Intended for DPU nodes.
func (m *Manager) RunUpdater(ctx context.Context) {
	if m.renewInterval == 0 {
		return
	}

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := m.Renew(ctx); err != nil {
			klog.Warningf("Failed to renew DPU lease %s: %v", m.leaseName(), err)
		}
	}, m.renewInterval)
}

// RunMonitor periodically checks the lease for expiry. Intended for DPU host nodes.
func (m *Manager) RunMonitor(ctx context.Context) {
	if m.renewInterval == 0 {
		return
	}

	period := m.monitorPeriod()
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := m.CheckStatus(ctx); err != nil {
			klog.Warningf("DPU lease %s marked unhealthy: %v", m.leaseName(), err)
		}
	}, period)
}

// CheckStatus validates the lease and updates readiness.
func (m *Manager) CheckStatus(ctx context.Context) error {
	if m.renewInterval == 0 {
		m.setStatus("", true)
		return nil
	}

	lease, err := m.client.CoordinationV1().Leases(m.namespace).Get(ctx, m.leaseName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.setStatus("DPU node lease not found", false)
		} else {
			m.setStatus(fmt.Sprintf("failed to read DPU node lease: %v", err), false)
		}
		return err
	}

	expired, msg := m.isExpired(lease)
	if expired {
		m.setStatus(msg, false)
		return fmt.Errorf("%s", msg)
	}

	m.setStatus("", true)
	return nil
}

// Renew bumps the lease renew time, creating the lease if needed.
func (m *Manager) Renew(ctx context.Context) error {
	if m.renewInterval == 0 {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease, err := m.client.CoordinationV1().Leases(m.namespace).Get(ctx, m.leaseName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = m.EnsureLease(ctx)
			return err
		}
		if err != nil {
			return err
		}
		if !m.updateLeaseSpec(lease, metav1.NowMicro(), true) {
			return nil
		}
		_, err = m.client.CoordinationV1().Leases(m.namespace).Update(ctx, lease, metav1.UpdateOptions{})
		return err
	})
}

func (m *Manager) monitorPeriod() time.Duration {
	period := m.renewInterval
	durationFraction := m.leaseDuration / 4
	if durationFraction > 0 && durationFraction < period {
		period = durationFraction
	}
	if period <= 0 {
		return time.Second
	}
	return period
}

func (m *Manager) setStatus(reason string, ready bool) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()

	if m.ready != ready || m.reason != reason {
		m.ready = ready
		m.reason = reason
	}
}

func (m *Manager) leaseName() string {
	return leaseNamePrefix + m.nodeName
}

func (m *Manager) newLease(now metav1.MicroTime) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.leaseName(),
			Namespace: m.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Node",
					Name:               m.nodeName,
					UID:                m.nodeUID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       stringPtr(HolderIdentity),
			LeaseDurationSeconds: int32Ptr(int32(m.leaseDuration.Seconds())),
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
}

func (m *Manager) updateLeaseSpec(lease *coordinationv1.Lease, now metav1.MicroTime, bumpRenew bool) bool {
	changed := false

	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != HolderIdentity {
		lease.Spec.HolderIdentity = stringPtr(HolderIdentity)
		changed = true
	}

	if lease.Spec.LeaseDurationSeconds == nil || int32(m.leaseDuration.Seconds()) != *lease.Spec.LeaseDurationSeconds {
		lease.Spec.LeaseDurationSeconds = int32Ptr(int32(m.leaseDuration.Seconds()))
		changed = true
	}

	if bumpRenew {
		if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Equal(&now) {
			lease.Spec.RenewTime = &now
			changed = true
		}
		if lease.Spec.AcquireTime == nil {
			lease.Spec.AcquireTime = &now
			changed = true
		}
	}

	if !m.hasOwnerRef(lease.OwnerReferences) {
		lease.OwnerReferences = append(lease.OwnerReferences, metav1.OwnerReference{
			APIVersion:         "v1",
			Kind:               "Node",
			Name:               m.nodeName,
			UID:                m.nodeUID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		})
		changed = true
	}

	return changed
}

func (m *Manager) hasOwnerRef(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.Kind == "Node" && ref.Name == m.nodeName && ref.UID == m.nodeUID {
			return true
		}
	}
	return false
}

func (m *Manager) isExpired(lease *coordinationv1.Lease) (bool, string) {
	if lease.Spec.LeaseDurationSeconds == nil || lease.Spec.RenewTime == nil {
		return true, "DPU node lease missing renew time or duration"
	}

	expire := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	if time.Now().After(expire) {
		return true, fmt.Sprintf("DPU node lease expired at %s", expire.UTC().Format(time.RFC3339))
	}
	return false, ""
}

func stringPtr(val string) *string {
	return &val
}

func int32Ptr(val int32) *int32 {
	return &val
}

func boolPtr(val bool) *bool {
	return &val
}
