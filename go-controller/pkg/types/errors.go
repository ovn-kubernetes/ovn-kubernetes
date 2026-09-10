// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"

	k8stypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

// PodUIDMismatchError indicates that an annotation update targets a pod that
// has been replaced. Callers must not retry the update against the new UID.
type PodUIDMismatchError struct {
	Namespace   string
	Name        string
	ExpectedUID k8stypes.UID
	ActualUID   k8stypes.UID
}

func (e *PodUIDMismatchError) Error() string {
	return fmt.Sprintf("pod %s/%s was replaced while updating annotations: expected UID %q, found %q",
		e.Namespace, e.Name, e.ExpectedUID, e.ActualUID)
}

// IsPodUIDMismatchError reports whether err wraps a PodUIDMismatchError.
func IsPodUIDMismatchError(err error) bool {
	var mismatch *PodUIDMismatchError
	return errors.As(err, &mismatch)
}

type SuppressedError struct {
	Inner error
}

func (e *SuppressedError) Error() string {
	return fmt.Sprintf("suppressed error logged: %v", e.Inner.Error())
}

func (e *SuppressedError) Unwrap() error {
	return e.Inner
}

func NewSuppressedError(err error) error {
	return &SuppressedError{
		Inner: err,
	}
}

func IsSuppressedError(err error) bool {
	var suppressedError *SuppressedError
	// errors.As() is not supported with Aggregate type error. Aggregate.Errors() converts an
	// Aggregate type error into a slice of builtin error and then errors.As() can be used
	if agg, ok := err.(kerrors.Aggregate); ok && err != nil {
		suppress := false
		for _, err := range agg.Errors() {
			if errors.As(err, &suppressedError) {
				suppress = true
			} else {
				return false
			}
		}
		return suppress
	}
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		errs := unwrapper.Unwrap()
		if len(errs) == 0 {
			return false
		}
		for _, e := range errs {
			if !IsSuppressedError(e) {
				return false
			}
		}
		return true
	}
	return errors.As(err, &suppressedError)
}
