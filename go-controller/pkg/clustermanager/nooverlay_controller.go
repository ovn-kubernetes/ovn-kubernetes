package clustermanager

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	udntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapplyconfkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	userdefinednetworkclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// validationError represents different types of validation failures
type validationError struct {
	errorType   string
	message     string
	raNames     []string // Names of RAs that exist but aren't accepted (for notAccepted scenario)
	networkType string   // Type of network: "DefaultNetwork" or "ClusterUserDefinedNetwork"
	networkName string   // Name of the network (for CUDN, empty for default network)
}

func (e *validationError) Error() string {
	return e.message
}

// noOverlayController validates no-overlay configuration with RouteAdvertisements.
// It watches NetworkAttachmentDefinition (NAD) resources for the default network,
// ClusterUserDefinedNetwork resources, and RouteAdvertisements CRs, triggering
// validation when relevant changes occur.
type noOverlayController struct {
	wf        *factory.WatchFactory
	recorder  record.EventRecorder
	udnClient userdefinednetworkclientset.Interface

	// raController watches RouteAdvertisements resources
	raController controllerutil.Controller

	// cudnController watches ClusterUserDefinedNetwork resources
	cudnController controllerutil.Controller

	// validationLock protects validation state
	validationLock sync.Mutex
	// lastValidationError tracks the last validation error to avoid spamming events
	lastValidationError string
}

// newNoOverlayController creates a new no-overlay validation controller.
// This should only be called when config.Default.Transport == types.NetworkTransportNoOverlay.
func newNoOverlayController(wf *factory.WatchFactory, recorder record.EventRecorder, udnClient userdefinednetworkclientset.Interface) (*noOverlayController, error) {
	klog.Infof("Creating no-overlay validation controller")

	c := &noOverlayController{
		wf:        wf,
		recorder:  recorder,
		udnClient: udnClient,
	}

	// Create controller config with RouteAdvertisements informer
	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileRA,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsValidation,
	}
	c.raController = controllerutil.NewController("no-overlay-ra-watcher", raConfig)

	// Create controller config with ClusterUserDefinedNetwork informer only if network segmentation is enabled
	if config.OVNKubernetesFeature.EnableNetworkSegmentation {
		cudnConfig := &controllerutil.ControllerConfig[udntypes.ClusterUserDefinedNetwork]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileCUDN,
			Threadiness:    1,
			Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
			Lister:         wf.ClusterUserDefinedNetworkInformer().Lister().List,
			ObjNeedsUpdate: c.cudnNeedsValidation,
		}
		c.cudnController = controllerutil.NewController("no-overlay-cudn-watcher", cudnConfig)
	}

	return c, nil
}

// Start starts the no-overlay validation controller
func (c *noOverlayController) Start() error {
	if c == nil {
		return nil
	}

	klog.Infof("Starting no-overlay validation controller")

	// Run initial validation when starting to catch the case where no RouteAdvertisements
	// or no ClusterUserDefinedNetworks exist (which won't trigger any Add events from the informer)
	go c.runValidation()

	// Start all controllers
	if c.cudnController != nil {
		return controllerutil.Start(c.raController, c.cudnController)
	}
	return controllerutil.Start(c.raController)
}

// Stop stops the no-overlay validation controller
func (c *noOverlayController) Stop() {
	if c == nil {
		return
	}

	klog.Infof("Stopping no-overlay validation controller")

	if c.cudnController != nil {
		controllerutil.Stop(c.raController, c.cudnController)
	} else {
		controllerutil.Stop(c.raController)
	}
}

// reconcileRA is called whenever a RouteAdvertisements resource changes
func (c *noOverlayController) reconcileRA(key string) error {
	klog.V(5).Infof("No-overlay controller reconciling RouteAdvertisements %q", key)
	c.runValidation()
	return nil
}

// raNeedsValidation checks if the RouteAdvertisements update requires validation
func (c *noOverlayController) raNeedsValidation(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	klog.V(5).Infof("No-overlay controller checking if RouteAdvertisements %q needs validation", newRA.Name)
	// If either object is nil, we need to validate
	if oldRA == nil || newRA == nil {
		return true
	}

	isRAAdvertisingNoOverlayNetwork := func(ra *ratypes.RouteAdvertisements) bool {
		for _, networkSelector := range ra.Spec.NetworkSelectors {
			// Check if it's advertising default network in no-overlay mode
			if networkSelector.NetworkSelectionType == apitypes.DefaultNetwork &&
				config.Default.Transport == types.NetworkTransportNoOverlay {
				return true
			}
			// Check if it's advertising ClusterUserDefinedNetworks
			if networkSelector.NetworkSelectionType == apitypes.ClusterUserDefinedNetworks {
				return true
			}
		}
		return false
	}

	// If the RA started or stopped advertising no-overlay networks, validate
	if isRAAdvertisingNoOverlayNetwork(oldRA) != isRAAdvertisingNoOverlayNetwork(newRA) {
		return true
	}

	// Check if NetworkSelectors changed
	if !reflect.DeepEqual(oldRA.Spec.NetworkSelectors, newRA.Spec.NetworkSelectors) {
		return true
	}

	// Check if Advertisements changed
	if !reflect.DeepEqual(oldRA.Spec.Advertisements, newRA.Spec.Advertisements) {
		return true
	}

	// Check if Accepted condition changed
	return isRAAccepted(oldRA.Status.Conditions) != isRAAccepted(newRA.Status.Conditions)
}

// reconcileCUDN is called whenever a ClusterUserDefinedNetwork resource changes
func (c *noOverlayController) reconcileCUDN(key string) error {
	klog.V(5).Infof("No-overlay controller reconciling ClusterUserDefinedNetwork %q", key)
	c.runValidation()
	return nil
}

// cudnNeedsValidation checks if the ClusterUserDefinedNetwork update requires validation
func (c *noOverlayController) cudnNeedsValidation(oldCUDN, newCUDN *udntypes.ClusterUserDefinedNetwork) bool {
	klog.V(5).Infof("No-overlay controller checking if ClusterUserDefinedNetwork %q needs validation", newCUDN.Name)
	// If either object is nil, we need to validate
	if oldCUDN == nil || newCUDN == nil {
		return true
	}

	// Check if transport changed to/from NoOverlay
	oldTransport := oldCUDN.Spec.Network.GetTransport()
	newTransport := newCUDN.Spec.Network.GetTransport()
	if oldTransport != newTransport {
		return true
	}

	// Only care about CUDNs with no-overlay transport
	if newTransport != udntypes.TransportOptionNoOverlay {
		return false
	}

	// Check if NoOverlay config changed (OutboundSNAT or Routing)
	if !reflect.DeepEqual(oldCUDN.Spec.Network.NoOverlay, newCUDN.Spec.Network.NoOverlay) {
		return true
	}

	// No relevant changes for no-overlay CUDN
	return false
}

// runValidation runs validation and emits events if the state changed
func (c *noOverlayController) runValidation() {
	c.validationLock.Lock()
	defer c.validationLock.Unlock()

	err := c.validate()
	currentError := ""
	if err != nil {
		currentError = err.Error()
	}

	// Only emit event if error state changed
	if currentError != c.lastValidationError {
		if err != nil {
			klog.Errorf("No-overlay validation failed: %v", err)
			c.emitValidationEvent(err)
		} else {
			klog.Infof("No-overlay validation passed: RouteAdvertisements configuration is now valid")
			c.emitReadyEvent()
		}
		c.lastValidationError = currentError
	}
}

// validate checks if the no-overlay configuration is valid
func (c *noOverlayController) validate() error {
	// Validate default network if it's in no-overlay mode
	if config.Default.Transport == types.NetworkTransportNoOverlay {
		if err := c.validateDefaultNetwork(); err != nil {
			return err
		}
	}

	// Validate ClusterUserDefinedNetworks with no-overlay transport
	// This runs unconditionally because CUDNs can have NoOverlay transport
	// independently of the default network's transport mode
	if err := c.validateClusterUserDefinedNetworks(); err != nil {
		return err
	}

	return nil
}

// validateDefaultNetwork checks if the default network no-overlay configuration is valid
func (c *noOverlayController) validateDefaultNetwork() error {
	// Get all RouteAdvertisements CRs
	ras, err := c.wf.RouteAdvertisementsInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	// Track if we found RAs advertising default network (but not accepted)
	foundDefaultNetworkRA := false
	notAcceptedRANames := []string{}

	// Check if any RouteAdvertisements CR is configured for the default network
	for _, ra := range ras {
		// Check if this RouteAdvertisements selects the default network
		for _, networkSelector := range ra.Spec.NetworkSelectors {
			// Check if it's selecting the default network
			if networkSelector.NetworkSelectionType == apitypes.DefaultNetwork {
				// Found a RouteAdvertisements for default network
				// Check if it advertises pod networks
				if !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
					continue
				}

				// We found at least one RA advertising default network
				foundDefaultNetworkRA = true

				if isRAAccepted(ra.Status.Conditions) {
					// Valid configuration found
					klog.V(5).Infof("Found valid RouteAdvertisements %q for default network with no-overlay transport", ra.Name)
					return nil
				} else {
					klog.Warningf("RouteAdvertisements %q selects default network but status is not Accepted", ra.Name)
					notAcceptedRANames = append(notAcceptedRANames, ra.Name)
				}
			}
		}
	}

	// Return specific error based on what we found
	if !foundDefaultNetworkRA {
		return &validationError{
			errorType:   "noRouteAdvertisements",
			message:     "no RouteAdvertisements CR is advertising the default network pod networks",
			networkType: "DefaultNetwork",
		}
	}

	// Found RAs advertising default network, but none are accepted
	return &validationError{
		errorType:   "notAccepted",
		message:     fmt.Sprintf("RouteAdvertisements CRs %v are advertising the default network pod networks but none have status Accepted=True", notAcceptedRANames),
		raNames:     notAcceptedRANames,
		networkType: "DefaultNetwork",
	}
}

// validateClusterUserDefinedNetworks checks if ClusterUserDefinedNetworks with no-overlay transport have valid RouteAdvertisements
// and updates the TransportAccepted status condition for each CUDN
func (c *noOverlayController) validateClusterUserDefinedNetworks() error {
	// Skip validation if network segmentation is not enabled
	if !config.OVNKubernetesFeature.EnableNetworkSegmentation {
		return nil
	}

	// Get all ClusterUserDefinedNetworks
	cudns, err := c.wf.ClusterUserDefinedNetworkInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list ClusterUserDefinedNetworks: %w", err)
	}

	// Get all RouteAdvertisements CRs (only needed if there are CUDNs)
	var ras []*ratypes.RouteAdvertisements
	if len(cudns) > 0 {
		ras, err = c.wf.RouteAdvertisementsInformer().Lister().List(labels.Everything())
		if err != nil {
			return fmt.Errorf("failed to list RouteAdvertisements: %w", err)
		}
	}

	// Collect any validation errors that should stop the controller
	var validationErrors []error

	// Check each CUDN and update its TransportAccepted status
	for _, cudn := range cudns {
		transport := cudn.Spec.Network.GetTransport()

		// Handle Geneve transport (or empty, which defaults to Geneve)
		if transport == "" || transport == udntypes.TransportOptionGeneve {
			if err := c.updateCUDNTransportStatus(cudn, metav1.ConditionTrue, "GeneveTransportAccepted", "Geneve transport has been configured."); err != nil {
				klog.Errorf("Failed to update TransportAccepted status for ClusterUserDefinedNetwork %q: %v", cudn.Name, err)
			}
			continue
		}

		// Handle NoOverlay transport
		if transport == udntypes.TransportOptionNoOverlay {
			klog.V(5).Infof("Validating no-overlay ClusterUserDefinedNetwork %q", cudn.Name)

			// Track if we found RAs advertising this CUDN (but not accepted)
			foundCUDNRA := false
			var notAcceptedRANames []string
			var acceptedRAName string

			// Check if any RouteAdvertisements CR is configured for this CUDN
			for _, ra := range ras {
				if c.isRASelectingCUDN(ra, cudn) {
					// Check if it advertises pod networks
					if !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
						continue
					}

					foundCUDNRA = true

					if isRAAccepted(ra.Status.Conditions) {
						// Valid configuration found for this CUDN
						klog.V(5).Infof("Found valid RouteAdvertisements %q for ClusterUserDefinedNetwork %q with no-overlay transport", ra.Name, cudn.Name)
						acceptedRAName = ra.Name
						break
					} else {
						klog.Warningf("RouteAdvertisements %q selects ClusterUserDefinedNetwork %q but status is not Accepted", ra.Name, cudn.Name)
						notAcceptedRANames = append(notAcceptedRANames, ra.Name)
					}
				}
			}

			// Update status based on what we found
			if acceptedRAName != "" {
				// Found an accepted RouteAdvertisements CR
				if err := c.updateCUDNTransportStatus(cudn, metav1.ConditionTrue, "NoOverlayTransportAccepted", "Transport has been configured as 'no-overlay'."); err != nil {
					klog.Errorf("Failed to update TransportAccepted status for ClusterUserDefinedNetwork %q: %v", cudn.Name, err)
				}
			} else if !foundCUDNRA {
				// No RouteAdvertisements CR advertising this CUDN
				if err := c.updateCUDNTransportStatus(cudn, metav1.ConditionFalse, "NoOverlayRouteAdvertisementsIsMissing", "No RouteAdvertisements CR is advertising the pod networks."); err != nil {
					klog.Errorf("Failed to update TransportAccepted status for ClusterUserDefinedNetwork %q: %v", cudn.Name, err)
				}
				// Add to validation errors for event emission
				validationErrors = append(validationErrors, &validationError{
					errorType:   "noRouteAdvertisements",
					message:     fmt.Sprintf("no RouteAdvertisements CR is advertising pod networks for ClusterUserDefinedNetwork %q", cudn.Name),
					networkType: "ClusterUserDefinedNetwork",
					networkName: cudn.Name,
				})
			} else {
				// Found RAs advertising this CUDN, but none are accepted
				raNamesList := strings.Join(notAcceptedRANames, ", ")
				message := fmt.Sprintf("RouteAdvertisements CR %s advertises the pod subnets, but its status is not accepted.", raNamesList)
				if err := c.updateCUDNTransportStatus(cudn, metav1.ConditionFalse, "NoOverlayRouteAdvertisementsNotAccepted", message); err != nil {
					klog.Errorf("Failed to update TransportAccepted status for ClusterUserDefinedNetwork %q: %v", cudn.Name, err)
				}
				// Add to validation errors for event emission
				validationErrors = append(validationErrors, &validationError{
					errorType:   "notAccepted",
					message:     fmt.Sprintf("RouteAdvertisements CRs %v are advertising pod networks for ClusterUserDefinedNetwork %q but none have status Accepted=True", notAcceptedRANames, cudn.Name),
					raNames:     notAcceptedRANames,
					networkType: "ClusterUserDefinedNetwork",
					networkName: cudn.Name,
				})
			}
		}
	}

	// If there are any validation errors, return the first one to trigger event emission
	if len(validationErrors) > 0 {
		return validationErrors[0]
	}

	return nil
}

// isRASelectingCUDN checks if a RouteAdvertisements CR selects a specific ClusterUserDefinedNetwork
func (c *noOverlayController) isRASelectingCUDN(ra *ratypes.RouteAdvertisements, cudn *udntypes.ClusterUserDefinedNetwork) bool {
	for _, networkSelector := range ra.Spec.NetworkSelectors {
		if networkSelector.NetworkSelectionType == apitypes.ClusterUserDefinedNetworks {
			// Check if the label selector matches this CUDN
			if networkSelector.ClusterUserDefinedNetworkSelector != nil {
				selector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
				if err != nil {
					klog.Errorf("Failed to convert label selector to selector: %v", err)
					return false
				}
				if selector.Matches(labels.Set(cudn.Labels)) {
					return true
				}
			}
		}
	}
	return false
}

// emitValidationEvent emits a Kubernetes event for validation failures
func (c *noOverlayController) emitValidationEvent(err error) {
	var eventReason, eventMessage string

	// Check if this is our custom validation error type
	if valErr, ok := err.(*validationError); ok {
		switch valErr.errorType {
		case "notAccepted":
			// Scenario: RAs exist but none are accepted
			eventReason = "RouteAdvertisementsNotAccepted"
			if valErr.networkType == "ClusterUserDefinedNetwork" {
				// Error for ClusterUserDefinedNetwork
				if len(valErr.raNames) > 0 {
					eventMessage = fmt.Sprintf("RouteAdvertisements CR(s) %v exist for ClusterUserDefinedNetwork %q but none have status Accepted=True. "+
						"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks.",
						strings.Join(valErr.raNames, ", "), valErr.networkName)
				} else {
					eventMessage = fmt.Sprintf("RouteAdvertisements CR(s) exist for ClusterUserDefinedNetwork %q but none have status Accepted=True. "+
						"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks.",
						valErr.networkName)
				}
			} else {
				// Error for default network
				if len(valErr.raNames) > 0 {
					eventMessage = fmt.Sprintf("RouteAdvertisements CR(s) %v exist for the default network but none have status Accepted=True. "+
						"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks.",
						strings.Join(valErr.raNames, ", "))
				} else {
					eventMessage = "RouteAdvertisements CR(s) exist for the default network but none have status Accepted=True. " +
						"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks."
				}
			}
		case "noRouteAdvertisements":
			// Scenario: No RAs advertising the network
			eventReason = "NoRouteAdvertisements"
			if valErr.networkType == "ClusterUserDefinedNetwork" {
				eventMessage = fmt.Sprintf("No RouteAdvertisements CR is advertising pod networks for ClusterUserDefinedNetwork %q. "+
					"RouteAdvertisements configuration is required when transport=no-overlay.",
					valErr.networkName)
			} else {
				eventMessage = "No RouteAdvertisements CR is advertising the default network pod networks. " +
					"RouteAdvertisements configuration is required when transport=no-overlay."
			}
		default:
			// Unknown validation error type
			eventReason = "NoOverlayConfigurationError"
			eventMessage = fmt.Sprintf("No-overlay transport configuration error: %v", err)
		}
	} else {
		// Generic error
		eventReason = "NoOverlayConfigurationError"
		eventMessage = fmt.Sprintf("No-overlay transport configuration error: %v", err)
	}

	c.recorder.Eventf(
		&corev1.ObjectReference{
			Kind:      "ClusterManager",
			Name:      "ovn-kubernetes",
			Namespace: config.Kubernetes.OVNConfigNamespace,
		},
		corev1.EventTypeWarning,
		eventReason,
		eventMessage,
	)
}

// emitReadyEvent emits a Normal event when validation passes
func (c *noOverlayController) emitReadyEvent() {
	c.recorder.Eventf(
		&corev1.ObjectReference{
			Kind:      "ClusterManager",
			Name:      "ovn-kubernetes",
			Namespace: config.Kubernetes.OVNConfigNamespace,
		},
		corev1.EventTypeNormal,
		"NoOverlayConfigurationReady",
		"No-overlay transport is properly configured with RouteAdvertisements CR advertising the default network pod networks with status Accepted=True",
	)
}

func isRAAccepted(conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, "Accepted")
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// updateCUDNTransportStatus updates the TransportAccepted status condition for a ClusterUserDefinedNetwork
func (c *noOverlayController) updateCUDNTransportStatus(cudn *udntypes.ClusterUserDefinedNetwork, status metav1.ConditionStatus, reason, message string) error {
	if cudn == nil {
		return nil
	}

	// Get the current CUDN to check if update is needed
	currentCUDN, err := c.wf.ClusterUserDefinedNetworkInformer().Lister().Get(cudn.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q: %w", cudn.Name, err)
	}

	// Check if the condition already exists with the same values
	existingCondition := meta.FindStatusCondition(currentCUDN.Status.Conditions, "TransportAccepted")
	if existingCondition != nil &&
		existingCondition.Status == status &&
		existingCondition.Reason == reason &&
		existingCondition.Message == message {
		// No update needed
		return nil
	}

	// Create the condition
	now := metav1.Now()
	condition := &metaapplyv1.ConditionApplyConfiguration{
		Type:               stringPtr("TransportAccepted"),
		Status:             (*metav1.ConditionStatus)(stringPtr(string(status))),
		LastTransitionTime: &now,
		Reason:             stringPtr(reason),
		Message:            stringPtr(message),
	}

	cudnStatus := udnapplyconfkv1.ClusterUserDefinedNetworkStatus().WithConditions(condition)
	applyCUDN := udnapplyconfkv1.ClusterUserDefinedNetwork(cudn.Name).WithStatus(cudnStatus)
	opts := metav1.ApplyOptions{
		FieldManager: "no-overlay-controller",
		Force:        true,
	}

	_, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(context.Background(), applyCUDN, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update ClusterUserDefinedNetwork %q status: %w", cudn.Name, err)
	}
	klog.Infof("Updated TransportAccepted status for ClusterUserDefinedNetwork %q: %s (reason: %s)", cudn.Name, status, reason)
	return nil
}

func stringPtr(s string) *string {
	return &s
}
