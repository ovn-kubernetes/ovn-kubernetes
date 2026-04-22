// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"testing"

	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

func TestIsSuppressedError(t *testing.T) {
	suppressed := NewSuppressedError(fmt.Errorf("annotation not set"))
	plain := fmt.Errorf("real failure")
	wrapped := fmt.Errorf("outer: %w", suppressed)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"single suppressed", suppressed, true},
		{"single plain", plain, false},
		{"wrapped suppressed", wrapped, true},
		{"join all suppressed", utilerrors.Join(suppressed, NewSuppressedError(fmt.Errorf("other"))), true},
		{"join mixed", utilerrors.Join(suppressed, plain), false},
		{"join all plain", utilerrors.Join(plain, fmt.Errorf("another")), false},
		{"join single suppressed", utilerrors.Join(suppressed), true},
		{"join single plain", utilerrors.Join(plain), false},
		{"nested join all suppressed", utilerrors.Join(wrapped, suppressed), true},
		{"nested join mixed", utilerrors.Join(wrapped, plain), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSuppressedError(tt.err); got != tt.want {
				t.Errorf("IsSuppressedError() = %v, want %v", got, tt.want)
			}
		})
	}
}
