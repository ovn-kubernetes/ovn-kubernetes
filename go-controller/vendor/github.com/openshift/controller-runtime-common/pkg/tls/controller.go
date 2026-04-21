/*
Copyright 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tls

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SecurityProfileWatcher watches the APIServer object for TLS profile changes
// and triggers a graceful shutdown when the profile changes.
type SecurityProfileWatcher struct {
	client.Client

	// InitialTLSProfileSpec is the TLS profile spec that was configured when the operator started.
	InitialTLSProfileSpec configv1.TLSProfileSpec

	// InitialTLSAdherencePolicy is the TLS adherence policy that was configured when the operator started.
	InitialTLSAdherencePolicy configv1.TLSAdherencePolicy

	// OnProfileChange is a function that will be called when the TLS profile changes.
	// It receives the reconcile context, old and new TLS profile specs.
	// This allows the caller to make decisions based on the actual profile changes.
	//
	// The most common use case for this callback is
	// to trigger a graceful shutdown of the operator
	// to make it pick up the new configuration.
	//
	// Example:
	//
	// 	// Create a context that can be cancelled when there is a need to shut down the manager.
	//  ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	//  defer cancel()
	//
	//  watcher := &SecurityProfileWatcher{
	// 	  OnProfileChange: func(ctx context.Context, old, new configv1.TLSProfileSpec) {
	//      logger.Infof("TLS profile has changed, initiating a shutdown to reload it. %q: %+v, %q: %+v",
	//        "old profile", old,
	//        "new profile", new,
	//      )
	//      // Cancel the outer context to trigger a graceful shutdown of the manager.
	//      cancel()
	//    },
	//  }
	OnProfileChange func(ctx context.Context, oldTLSProfileSpec, newTLSProfileSpec configv1.TLSProfileSpec)

	// OnAdherencePolicyChange is a function that will be called when the TLS adherence policy changes.
	OnAdherencePolicyChange func(ctx context.Context, oldTLSAdherencePolicy, newTLSAdherencePolicy configv1.TLSAdherencePolicy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecurityProfileWatcher) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("tlssecurityprofilewatcher").
		For(&configv1.APIServer{}, builder.WithPredicates(
			predicate.Funcs{
				// Only watch the "cluster" APIServer object.
				CreateFunc: func(e event.CreateEvent) bool {
					return e.Object.GetName() == APIServerName
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return e.ObjectNew.GetName() == APIServerName
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return e.Object.GetName() == APIServerName
				},
				GenericFunc: func(e event.GenericEvent) bool {
					return e.Object.GetName() == APIServerName
				},
			},
		)).
		// Override the default log constructor as it makes the logs very chatty.
		WithLogConstructor(func(_ *reconcile.Request) logr.Logger {
			return mgr.GetLogger().WithValues(
				"controller", "tlssecurityprofilewatcher",
			)
		}).
		Complete(r); err != nil {
		return fmt.Errorf("could not set up controller for TLS security profile watcher: %w", err)
	}

	return nil
}

// Reconcile watches for changes to the APIServer TLS profile and triggers a shutdown
// when the profile changes from the initial configuration.
func (r *SecurityProfileWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx, "name", req.Name)

	logger.V(1).Info("Reconciling APIServer TLS profile")
	defer logger.V(1).Info("Finished reconciling APIServer TLS profile")

	// Fetch the APIServer object.
	apiServer := &configv1.APIServer{}
	if err := r.Get(ctx, req.NamespacedName, apiServer); err != nil {
		if apierrors.IsNotFound(err) {
			// If the APIServer object is not found, we don't need to do anything.
			// This could happen if the object was deleted.
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get APIServer %s: %w", req.NamespacedName.String(), err)
	}

	// Get the current TLS profile spec.
	currentTLSProfileSpec, err := GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get TLS profile from APIServer %s: %w", req.NamespacedName.String(), err)
	}

	// Compare the current TLS profile spec with the initial one.
	if tlsProfileChanged := !reflect.DeepEqual(r.InitialTLSProfileSpec, currentTLSProfileSpec); tlsProfileChanged {
		// TLS profile has changed, invoke the callback if it is set.
		if r.OnProfileChange != nil {
			r.OnProfileChange(ctx, r.InitialTLSProfileSpec, currentTLSProfileSpec)
		}

		// Persist the new profile for future change detection.
		r.InitialTLSProfileSpec = currentTLSProfileSpec
	}

	// Compare the current TLS adherence policy with the initial one.
	if tlsAdherencePolicyChanged := r.InitialTLSAdherencePolicy != apiServer.Spec.TLSAdherence; tlsAdherencePolicyChanged {
		// TLS adherence policy has changed, invoke the callback if it is set.
		if r.OnAdherencePolicyChange != nil {
			r.OnAdherencePolicyChange(ctx, r.InitialTLSAdherencePolicy, apiServer.Spec.TLSAdherence)
		}

		// Persist the new adherence policy for future change detection.
		r.InitialTLSAdherencePolicy = apiServer.Spec.TLSAdherence
	}

	// No need to requeue, as the callback will handle further actions.
	return ctrl.Result{}, nil
}
