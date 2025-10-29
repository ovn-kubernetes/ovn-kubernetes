package clustermanager

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
)

// validationError represents different types of validation failures
type validationError struct {
	errorType string
	message   string
	raNames   []string // Names of RAs that exist but aren't accepted (for notAccepted scenario)
}

func (e *validationError) Error() string {
	return e.message
}

// noOverlayController validates no-overlay configuration with RouteAdvertisements.
// It watches NetworkAttachmentDefinition (NAD) resources for the default network
// and RouteAdvertisements CRs, triggering validation when relevant changes occur.
type noOverlayController struct {
	wf       *factory.WatchFactory
	recorder record.EventRecorder

	// raController watches RouteAdvertisements resources
	raController controllerutil.Controller

	// validationLock protects validation state
	validationLock sync.Mutex
	// lastValidationError tracks the last validation error to avoid spamming events
	lastValidationError string
}

// newNoOverlayController creates a new no-overlay validation controller.
// This should only be called when config.Default.Transport == config.TransportNoOverlay.
func newNoOverlayController(wf *factory.WatchFactory, recorder record.EventRecorder) (*noOverlayController, error) {
	klog.Infof("Creating no-overlay validation controller")

	c := &noOverlayController{
		wf:       wf,
		recorder: recorder,
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

	return c, nil
}

// Start starts the no-overlay validation controller
func (c *noOverlayController) Start() error {
	if c == nil {
		return nil
	}

	klog.Infof("Starting no-overlay validation controller")

	// Run initial validation when starting to catch the case where no RouteAdvertisements
	// exist (which won't trigger any Add events from the informer)
	go c.runValidation()

	// Start both controllers
	return controllerutil.Start(c.raController)
}

// Stop stops the no-overlay validation controller
func (c *noOverlayController) Stop() {
	if c == nil {
		return
	}

	klog.Infof("Stopping no-overlay validation controller")

	controllerutil.Stop(c.raController)
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

	// Only care about RAs if we're in no-overlay mode
	if config.Default.Transport != config.TransportNoOverlay {
		return false
	}

	isRAAdvertisingDefaultNetwork := func(ra *ratypes.RouteAdvertisements) bool {
		for _, networkSelector := range ra.Spec.NetworkSelectors {
			if networkSelector.NetworkSelectionType == apitypes.DefaultNetwork {
				return true
			}
		}
		return false
	}

	// If the RA started or stopped advertising default network, validate
	if isRAAdvertisingDefaultNetwork(oldRA) != isRAAdvertisingDefaultNetwork(newRA) {
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
	// If transport is not no-overlay, validation passes (not applicable)
	if config.Default.Transport != config.TransportNoOverlay {
		return nil
	}

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
			errorType: "noRouteAdvertisements",
			message:   "no RouteAdvertisements CR is advertising the default network pod networks",
		}
	}

	// Found RAs advertising default network, but none are accepted
	return &validationError{
		errorType: "notAccepted",
		message:   fmt.Sprintf("RouteAdvertisements CRs %v are advertising the default network pod networks but none have status Accepted=True", notAcceptedRANames),
		raNames:   notAcceptedRANames,
	}
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
			if len(valErr.raNames) > 0 {
				eventMessage = fmt.Sprintf("RouteAdvertisements CR(s) %v exist for the default network but none have status Accepted=True. "+
					"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks.",
					strings.Join(valErr.raNames, ", "))
			} else {
				eventMessage = "RouteAdvertisements CR(s) exist for the default network but none have status Accepted=True. " +
					"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks."
			}
		case "noRouteAdvertisements":
			// Scenario: No RAs advertising default network
			eventReason = "NoRouteAdvertisements"
			eventMessage = "No RouteAdvertisements CR is advertising the default network pod networks. " +
				"RouteAdvertisements configuration is required when transport=no-overlay."
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
