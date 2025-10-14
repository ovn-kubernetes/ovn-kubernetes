package status_manager

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"

	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressfirewalllisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type egressFirewallManager struct {
	lister egressfirewalllisters.EgressFirewallLister
	client egressfirewallclientset.Interface
}

func newEgressFirewallManager(lister egressfirewalllisters.EgressFirewallLister, client egressfirewallclientset.Interface) *egressFirewallManager {
	return &egressFirewallManager{
		lister: lister,
		client: client,
	}
}

//lint:ignore U1000 generic interfaces throw false-positives https://github.com/dominikh/go-tools/issues/1440
func (m *egressFirewallManager) get(namespace, name string) (*egressfirewallapi.EgressFirewall, error) {
	return m.lister.EgressFirewalls(namespace).Get(name)
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) getMessages(egressFirewall *egressfirewallapi.EgressFirewall) []string {
	return egressFirewall.Status.Messages
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) updateStatus(egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions,
	applyEmptyOrFailed bool) error {
	if egressFirewall == nil {
		return nil
	}
	newStatus := "EgressFirewall Rules applied"
	for _, message := range egressFirewall.Status.Messages {
		if strings.Contains(message, types.EgressFirewallErrorMsg) {
			newStatus = types.EgressFirewallErrorMsg
			break
		}
	}
	if applyEmptyOrFailed && newStatus != types.EgressFirewallErrorMsg {
		newStatus = ""
	}

	if egressFirewall.Status.Status == newStatus {
		// already set to the same value
		return nil
	}

	applyStatus := egressfirewallapply.EgressFirewallStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}

	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(applyStatus)

	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) cleanupStatus(egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions) error {
	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace)
	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}

// egressFirewallZoneDeleteCleanupManager is used to clean up managedFields from deleted zones
type egressFirewallZoneDeleteCleanupManager struct {
	client egressfirewallclientset.Interface
}

func newEgressFirewallZoneDeleteCleanupManager(client egressfirewallclientset.Interface) *egressFirewallZoneDeleteCleanupManager {
	return &egressFirewallZoneDeleteCleanupManager{
		client: client,
	}
}

// GetEgressFirewalls returns the list of all EgressFirewall objects from kubernetes API Server
func (m *egressFirewallZoneDeleteCleanupManager) GetEgressFirewalls() ([]*egressfirewallapi.EgressFirewall, error) {
	list := []*egressfirewallapi.EgressFirewall{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.K8sV1().EgressFirewalls("").List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		ef, ok := obj.(*egressfirewallapi.EgressFirewall)
		if !ok {
			klog.Errorf("Expected *egressfirewallapi.EgressFirewall but got %T", obj)
			return nil
		}
		list = append(list, ef)
		return nil
	})
	return list, err
}

// removeZoneStatusFromEgressFirewalls removes the managedFields managed by the input zone
// in the status of the provided EgressFirewall objects
func (m *egressFirewallZoneDeleteCleanupManager) removeZoneStatusFromEgressFirewalls(egressFirewalls []*egressfirewallapi.EgressFirewall, zone string) {
	for _, ef := range egressFirewalls {
		applyObj := egressfirewallapply.EgressFirewall(ef.Name, ef.Namespace)
		_, err := m.client.K8sV1().EgressFirewalls(ef.Namespace).
			ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Errorf("Unable to remove the status owned by zone %s from EgressFirewall %s/%s: %v",
				zone, ef.Namespace, ef.Name, err)
		}
	}
}

// cleanupDeletedZoneStatuses loops through the provided zones and removes the statuses of those
// zones from existing EgressFirewall objects
func (m *egressFirewallZoneDeleteCleanupManager) cleanupDeletedZoneStatuses(deletedZones sets.Set[string]) {
	existingEgressFirewalls, err := m.GetEgressFirewalls()
	if err != nil {
		klog.Errorf("Unable to fetch EgressFirewalls: %v", err)
		return
	}
	if len(existingEgressFirewalls) > 0 {
		for _, zone := range deletedZones.UnsortedList() {
			m.removeZoneStatusFromEgressFirewalls(existingEgressFirewalls, zone)
		}
	}
}
