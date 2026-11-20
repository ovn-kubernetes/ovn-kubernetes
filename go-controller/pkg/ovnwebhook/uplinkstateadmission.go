// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
)

// UplinkStateAdmission validates creation. The admission policy enforces
// ownership and protects owner references on updates and deletion.
type UplinkStateAdmission struct{}

// NewUplinkStateAdmissionWebhook creates the UplinkState creation validator.
func NewUplinkStateAdmissionWebhook() *UplinkStateAdmission {
	return &UplinkStateAdmission{}
}

var _ admission.Validator[*uplinkv1alpha1.UplinkState] = &UplinkStateAdmission{}

// ValidateCreate checks ownership and the canonical key before a node can reserve it.
func (a UplinkStateAdmission) ValidateCreate(ctx context.Context, state *uplinkv1alpha1.UplinkState) (admission.Warnings, error) {
	if err := a.validateWriter(ctx, state); err != nil {
		return nil, err
	}
	if len(state.Finalizers) != 0 {
		return nil, fmt.Errorf("OVN node identities may not set UplinkState finalizers")
	}
	return nil, nil
}

// ValidateUpdate is outside the webhook rules; VAP validates updates.
func (a UplinkStateAdmission) ValidateUpdate(_ context.Context, _, _ *uplinkv1alpha1.UplinkState) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete is outside the webhook rules; VAP enforces delete ownership.
func (a UplinkStateAdmission) ValidateDelete(_ context.Context, _ *uplinkv1alpha1.UplinkState) (admission.Warnings, error) {
	return nil, nil
}

func (a UplinkStateAdmission) validateWriter(ctx context.Context, state *uplinkv1alpha1.UplinkState) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return err
	}
	if state.Spec.NodeName == "" || req.UserInfo.Username != csrapprover.NamePrefix+":"+state.Spec.NodeName {
		return fmt.Errorf("only the owning OVN node identity may write UplinkState %q", state.Name)
	}
	if state.Name != uplinkutil.StateName(state.Spec.UplinkName, state.Spec.NodeName) {
		return fmt.Errorf("UplinkState name must match its Uplink and node identity")
	}
	return nil
}
