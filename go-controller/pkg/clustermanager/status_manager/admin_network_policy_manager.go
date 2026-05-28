// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpapiapply "sigs.k8s.io/network-policy-api/pkg/client/applyconfiguration/apis/v1alpha1"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

const (
	// policyReadyStatusType is the prefix for zone-specific readiness status conditions
	policyReadyStatusType = "Ready-In-Zone-"
)

// anpZoneDeleteCleanupManager is NOT like other status managers
// It only takes care of deleting statuses from zones as part of
// zone deletion
type anpZoneDeleteCleanupManager struct {
	client anpclientset.Interface
}

func newANPManager(client anpclientset.Interface) *anpZoneDeleteCleanupManager {
	return &anpZoneDeleteCleanupManager{
		client: client,
	}
}

// GetANPs returns the list of all AdminNetworkPolicy objects from kubernetes API Server
func (m *anpZoneDeleteCleanupManager) GetANPs() ([]*anpapi.AdminNetworkPolicy, error) {
	list := []*anpapi.AdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().AdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*anpapi.AdminNetworkPolicy))
		return nil
	})
	return list, err
}

// GetBANPs returns the list of all BaselineAdminNetworkPolicy objects from kubernetes API Server
func (m *anpZoneDeleteCleanupManager) GetBANPs() ([]*anpapi.BaselineAdminNetworkPolicy, error) {
	list := []*anpapi.BaselineAdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*anpapi.BaselineAdminNetworkPolicy))
		return nil
	})
	return list, err
}

// removeZoneStatusFromAllANPs removes the condition managed by zone
// in the conditions status of all ANPs and BANP in the cluster
// This is best effort, so errors are silently ignored by emitting
// warning messages.
func (m *anpZoneDeleteCleanupManager) removeZoneStatusFromAllANPs(existingANPs []*anpapi.AdminNetworkPolicy, existingBANPs []*anpapi.BaselineAdminNetworkPolicy, zone string) {
	klog.Infof("Deleting status for zone %s from existing admin network policies", zone)
	for _, existingANP := range existingANPs {
		applyObj := anpapiapply.AdminNetworkPolicy(existingANP.Name)
		_, err := m.client.PolicyV1alpha1().AdminNetworkPolicies().
			ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from ANP %s: %v", zone, existingANP.Name, err)
		}
	}
	for _, existingBANP := range existingBANPs {
		applyObj := anpapiapply.BaselineAdminNetworkPolicy(existingBANP.Name)
		_, err := m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().
			ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from BANP %s: %v", zone, existingBANP.Name, err)
		}
	}
}

// cleanupDeletedZoneStatuses loops through the provided zones and cleans the statuses of those
// zones from existing ANPs and BANPs
func (m *anpZoneDeleteCleanupManager) cleanupDeletedZoneStatuses(deletedZones sets.Set[string]) {
	// let us try to fetch all the ANPs/BANPs in one go so that we don't query API server for each zone
	existingANPs, err := m.GetANPs()
	if err != nil {
		klog.Warningf("Unable to fetch ANPs: %v", err)
	}
	existingBANPs, err := m.GetBANPs()
	if err != nil {
		klog.Warningf("Unable to fetch BANPs: %v", err)
	}
	if len(existingANPs) > 0 || len(existingBANPs) > 0 {
		for _, zone := range deletedZones.UnsortedList() {
			m.removeZoneStatusFromAllANPs(existingANPs, existingBANPs, zone)
		}
	}
}

// doStartupCleanup performs a one-time cleanup of stale ANP/BANP managedFields at startup.
// This is similar to the cleanup done in cleanupDeletedZoneStatuses when zones are deleted at runtime.
// It detects stale zones by checking for managedFields from zones that no longer exist.
func (m *anpZoneDeleteCleanupManager) doStartupCleanup(currentZones sets.Set[string]) error {
	klog.Infof("StatusManager: performing one-time startup cleanup for ANP/BANP managedFields")

	existingANPs, err := m.GetANPs()
	if err != nil {
		return fmt.Errorf("failed to fetch ANPs for startup cleanup: %w", err)
	}
	existingBANPs, err := m.GetBANPs()
	if err != nil {
		return fmt.Errorf("failed to fetch BANPs for startup cleanup: %w", err)
	}

	if len(existingANPs) == 0 && len(existingBANPs) == 0 {
		klog.V(5).Infof("StatusManager: no ANPs or BANPs found, skipping startup cleanup")
		return nil
	}

	// Find stale zones by checking managedFields on ANPs/BANPs
	staleZones := sets.New[string]()
	for _, anp := range existingANPs {
		for _, mf := range anp.ManagedFields {
			if mf.Subresource == "status" && !currentZones.Has(mf.Manager) && isEmptyStatusManagedField(mf) {
				staleZones.Insert(mf.Manager)
			}
		}
	}
	for _, banp := range existingBANPs {
		for _, mf := range banp.ManagedFields {
			if mf.Subresource == "status" && !currentZones.Has(mf.Manager) && isEmptyStatusManagedField(mf) {
				staleZones.Insert(mf.Manager)
			}
		}
	}

	if len(staleZones) > 0 {
		klog.Infof("StatusManager: found stale zones in ANP/BANP managedFields: %v", staleZones.UnsortedList())
		for _, zone := range staleZones.UnsortedList() {
			m.removeZoneStatusFromAllANPs(existingANPs, existingBANPs, zone)
		}
	}

	klog.Infof("StatusManager: ANP/BANP managedFields cleanup complete")

	// Also cleanup stale status.conditions[] entries
	if err := m.cleanupStaleStatusConditions(currentZones, existingANPs, existingBANPs); err != nil {
		return fmt.Errorf("failed to cleanup stale status conditions: %w", err)
	}

	return nil
}

// cleanupStaleStatusConditions removes status.conditions[] entries for deleted zones
// This addresses the issue where nodes deleted while the pod was down leave stale
// "Ready-In-Zone-<nodename>" conditions that never get cleaned up.
func (m *anpZoneDeleteCleanupManager) cleanupStaleStatusConditions(currentZones sets.Set[string], existingANPs []*anpapi.AdminNetworkPolicy, existingBANPs []*anpapi.BaselineAdminNetworkPolicy) error {
	klog.Infof("StatusManager: performing cleanup for stale ANP/BANP status conditions")

	totalStaleEntries := 0

	// Clean up stale zones from ANPs
	for _, anp := range existingANPs {
		staleZones := m.findStaleZonesInStatus(anp.Status.Conditions, currentZones)
		if len(staleZones) > 0 {
			klog.Infof("StatusManager: found %d stale status conditions in ANP %s", len(staleZones), anp.Name)
			totalStaleEntries += len(staleZones)

			// Remove stale zones one by one using Server-Side Apply
			for _, staleZone := range staleZones {
				applyObj := anpapiapply.AdminNetworkPolicy(anp.Name)
				_, err := m.client.PolicyV1alpha1().AdminNetworkPolicies().
					ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: staleZone, Force: true})
				if err != nil {
					klog.Warningf("StatusManager: failed to remove stale zone %s from ANP %s: %v", staleZone, anp.Name, err)
				} else {
					klog.V(4).Infof("StatusManager: removed stale zone %s from ANP %s", staleZone, anp.Name)
				}
			}
		}
	}

	// Clean up stale zones from BANPs
	for _, banp := range existingBANPs {
		staleZones := m.findStaleZonesInStatus(banp.Status.Conditions, currentZones)
		if len(staleZones) > 0 {
			klog.Infof("StatusManager: found %d stale status conditions in BANP %s", len(staleZones), banp.Name)
			totalStaleEntries += len(staleZones)

			// Remove stale zones one by one using Server-Side Apply
			for _, staleZone := range staleZones {
				applyObj := anpapiapply.BaselineAdminNetworkPolicy(banp.Name)
				_, err := m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().
					ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: staleZone, Force: true})
				if err != nil {
					klog.Warningf("StatusManager: failed to remove stale zone %s from BANP %s: %v", staleZone, banp.Name, err)
				} else {
					klog.V(4).Infof("StatusManager: removed stale zone %s from BANP %s", staleZone, banp.Name)
				}
			}
		}
	}

	klog.Infof("StatusManager: ANP/BANP status conditions cleanup complete, removed %d stale entries", totalStaleEntries)
	return nil
}

// findStaleZonesInStatus extracts stale zone names from status conditions
// A zone is considered stale if it appears in status.conditions[] but doesn't exist in currentZones
func (m *anpZoneDeleteCleanupManager) findStaleZonesInStatus(conditions []metav1.Condition, currentZones sets.Set[string]) []string {
	staleZones := []string{}

	for _, condition := range conditions {
		// Extract zone name from condition type "Ready-In-Zone-<nodename>"
		if strings.HasPrefix(condition.Type, policyReadyStatusType) {
			zoneName := strings.TrimPrefix(condition.Type, policyReadyStatusType)

			// If zone is not in current active zones, it's stale
			if !currentZones.Has(zoneName) {
				staleZones = append(staleZones, zoneName)
			}
		}
	}

	return staleZones
}
