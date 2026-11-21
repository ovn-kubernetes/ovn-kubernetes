// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testscenario

import "sigs.k8s.io/controller-runtime/pkg/client"

// ValidateCRScenario describes a create request and its expected admission error, if any.
type ValidateCRScenario struct {
	Description string
	Object      client.Object
	ExpectedErr string
}

// NewObject returns a fresh copy so client operations do not mutate the shared fixture.
func (s ValidateCRScenario) NewObject() client.Object {
	return s.Object.DeepCopyObject().(client.Object)
}

// UpdateCRScenario creates InitialObject, then updates it with Object.
// ExpectedErr is empty for an accepted update.
type UpdateCRScenario struct {
	InitialObject client.Object
	ValidateCRScenario
}

// NewInitialObject returns a fresh copy of the object created before an update.
func (s UpdateCRScenario) NewInitialObject() client.Object {
	return s.InitialObject.DeepCopyObject().(client.Object)
}
