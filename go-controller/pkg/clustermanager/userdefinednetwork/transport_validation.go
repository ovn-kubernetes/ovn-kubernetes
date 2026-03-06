package userdefinednetwork

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

const (
	conditionTypeTransportAccepted = "TransportAccepted"
)

// setTransportStatusCondition validates the transport configuration for a CUDN and sets its TransportAccepted status condition.
// This function is called during CUDN reconciliation to ensure the transport is properly configured.
//
// For Geneve transport (default), the condition is set to True.
// For no-overlay and EVPN transports, it validates that a RouteAdvertisements CR exists and is accepted.
//
// This function only SETS the condition on the provided CUDN object; it does NOT apply status.
// The actual status update is handled by updateClusterUDNStatus() to ensure a single status update.
//
// Per OKEP, CUDN validation failures should only update status, not emit events.
//
// Returns true if the TransportAccepted condition was updated (changed from previous value).
func (c *Controller) setTransportStatusCondition(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) (bool, error) {
	if cudn == nil {
		return false, nil
	}

	transport := cudn.Spec.Network.GetTransport()

	// Handle default Geneve transport (empty string)
	if transport == "" {
		updated := c.setTransportCondition(cudn, metav1.ConditionTrue, "DefaultTransportAccepted", "Default transport has been configured.")
		return updated, nil
	}

	// Handle NoOverlay and EVPN transports - both require RouteAdvertisements validation
	if transport == userdefinednetworkv1.TransportOptionNoOverlay || transport == userdefinednetworkv1.TransportOptionEVPN {
		updated, err := c.validateTransportWithRouteAdvertisements(cudn, transport)
		return updated, err
	}

	// Unknown transport - no validation
	return false, nil
}

// validateTransportWithRouteAdvertisements validates that a CUDN with no-overlay or EVPN transport has proper RouteAdvertisements configuration.
// This function is used for both NoOverlay and EVPN transports as they both require RouteAdvertisements validation.
// Returns true if the TransportAccepted condition was updated (changed from previous value).
func (c *Controller) validateTransportWithRouteAdvertisements(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, transport userdefinednetworkv1.TransportOption) (bool, error) {
	if c.raLister == nil {
		// RouteAdvertisements feature not enabled
		updated := c.setTransportCondition(cudn, metav1.ConditionFalse, "RouteAdvertisementsNotEnabled",
			fmt.Sprintf("RouteAdvertisements feature is not enabled but required for %s transport.", transport))
		return updated, nil
	}

	klog.V(5).Infof("Validating %s ClusterUserDefinedNetwork %q", transport, cudn.Name)

	// Get all RouteAdvertisements CRs
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return false, fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	// Track if we found RAs advertising this CUDN (but not accepted)
	foundCUDNRA := false
	var notAcceptedRANames []string
	var acceptedRAName string

	// Check if any RouteAdvertisements CR is configured for this CUDN
	for _, ra := range ras {
		selects, err := isRASelectingCUDN(ra, cudn)
		if err != nil {
			return false, fmt.Errorf("failed to check if RouteAdvertisements %q selects CUDN %q: %w", ra.Name, cudn.Name, err)
		}
		if selects {
			// Check if it advertises pod networks
			if !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
				continue
			}

			foundCUDNRA = true

			if notifier.IsRAAccepted(ra.Status.Conditions) {
				// Valid configuration found for this CUDN
				klog.V(5).Infof("Found valid RouteAdvertisements %q for ClusterUserDefinedNetwork %q with %s transport", ra.Name, cudn.Name, transport)
				acceptedRAName = ra.Name
				break
			} else {
				klog.Warningf("RouteAdvertisements %q selects ClusterUserDefinedNetwork %q but status is not Accepted", ra.Name, cudn.Name)
				notAcceptedRANames = append(notAcceptedRANames, ra.Name)
			}
		}
	}

	// Generate reason and message strings based on transport type
	var acceptedReason, missingReason, notAcceptedReason string
	var acceptedMessage string

	if transport == userdefinednetworkv1.TransportOptionNoOverlay {
		acceptedReason = "NoOverlayTransportAccepted"
		acceptedMessage = "Transport has been configured as 'no-overlay'."
		missingReason = "NoOverlayRouteAdvertisementsIsMissing"
		notAcceptedReason = "NoOverlayRouteAdvertisementsNotAccepted"
	} else { // EVPN
		acceptedReason = "EVPNTransportAccepted"
		acceptedMessage = "Transport has been configured as 'EVPN'."
		missingReason = "EVPNRouteAdvertisementsIsMissing"
		notAcceptedReason = "EVPNRouteAdvertisementsNotAccepted"
	}

	// Set status condition based on what we found and track if it changed
	var updated bool
	if acceptedRAName != "" {
		// Found an accepted RouteAdvertisements CR
		updated = c.setTransportCondition(cudn, metav1.ConditionTrue, acceptedReason, acceptedMessage)
	} else if !foundCUDNRA {
		// No RouteAdvertisements CR advertising this CUDN
		updated = c.setTransportCondition(cudn, metav1.ConditionFalse, missingReason, "No RouteAdvertisements CR is advertising the pod networks.")
	} else {
		// Found RAs advertising this CUDN, but none are accepted
		raNamesList := strings.Join(notAcceptedRANames, ", ")
		message := fmt.Sprintf("RouteAdvertisements CR %s advertises the pod subnets, but its status is not accepted.", raNamesList)
		updated = c.setTransportCondition(cudn, metav1.ConditionFalse, notAcceptedReason, message)
	}

	return updated, nil
}

// isRASelectingCUDN checks if a RouteAdvertisements CR selects a specific ClusterUserDefinedNetwork
func isRASelectingCUDN(ra *ratypes.RouteAdvertisements, cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) (bool, error) {
	for _, networkSelector := range ra.Spec.NetworkSelectors {
		if networkSelector.NetworkSelectionType == apitypes.ClusterUserDefinedNetworks {
			// Check if the label selector matches this CUDN
			if networkSelector.ClusterUserDefinedNetworkSelector != nil {
				selector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
				if err != nil {
					return false, fmt.Errorf("failed to convert label selector to selector: %w", err)
				}
				if selector.Matches(labels.Set(cudn.Labels)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// setTransportCondition sets the TransportAccepted status condition on a ClusterUserDefinedNetwork.
// This function only SETS the condition on the provided CUDN object; it does NOT apply status to the API server.
// The actual status update is handled by updateClusterUDNStatus() to ensure a single status update with all conditions.
// Returns true if the condition was updated (changed from previous value).
func (c *Controller) setTransportCondition(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, status metav1.ConditionStatus, reason, message string) bool {
	if cudn == nil {
		return false
	}

	// Create and set the condition using meta.SetStatusCondition
	// This will automatically handle updates vs creates and transition times
	now := metav1.Now()
	condition := metav1.Condition{
		Type:               conditionTypeTransportAccepted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}

	updated := meta.SetStatusCondition(&cudn.Status.Conditions, condition)
	klog.V(5).Infof("Set TransportAccepted condition for ClusterUserDefinedNetwork %q: %s (reason: %s)", cudn.Name, status, reason)
	return updated
}
