// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package namespace

import (
	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
)

// NamespaceAnnotationState holds parsed annotation data for a single namespace,
// mirroring NodeAnnotationState. Fields are read through accessor methods; the
// package-level *Changed helpers compare two snapshots.
type NamespaceAnnotationState struct {
	namespaceName string

	aclLogging    libovsdbutil.ACLLoggingLevels
	aclLoggingErr error

	multicastEnabled bool

	routingGWs    sets.Set[string]
	routingGWsErr error

	bfdEnabled bool
}

func newNamespaceAnnotationState(
	namespaceName string,
	aclLogging libovsdbutil.ACLLoggingLevels,
	aclLoggingErr error,
	multicastEnabled bool,
	routingGWs sets.Set[string],
	routingGWsErr error,
	bfdEnabled bool,
) *NamespaceAnnotationState {
	return &NamespaceAnnotationState{
		namespaceName:    namespaceName,
		aclLogging:       aclLogging,
		aclLoggingErr:    aclLoggingErr,
		multicastEnabled: multicastEnabled,
		routingGWs:       routingGWs,
		routingGWsErr:    routingGWsErr,
		bfdEnabled:       bfdEnabled,
	}
}

// NamespaceName returns the name of the namespace this state describes.
func (s *NamespaceAnnotationState) NamespaceName() string {
	if s == nil {
		return ""
	}
	return s.namespaceName
}

// ACLLogging returns a copy of the parsed ACL-logging levels, or the zero
// value if state is nil or unparsable.
func (s *NamespaceAnnotationState) ACLLogging() libovsdbutil.ACLLoggingLevels {
	if s == nil {
		return libovsdbutil.ACLLoggingLevels{}
	}
	return s.aclLogging
}

// MulticastEnabled reports whether multicast is annotation-enabled (false if nil).
func (s *NamespaceAnnotationState) MulticastEnabled() bool {
	if s == nil {
		return false
	}
	return s.multicastEnabled
}

// ExternalGWs returns a copy of the parsed routing-external-gws set. The error
// is non-nil only when the annotation was present but failed to parse.
func (s *NamespaceAnnotationState) ExternalGWs() (sets.Set[string], error) {
	if s == nil {
		return sets.New[string](), nil
	}
	if s.routingGWsErr != nil {
		return nil, s.routingGWsErr
	}
	return sets.New(s.routingGWs.UnsortedList()...), nil
}

// BFDEnabled reports whether the namespace's BFD annotation is set (false if nil).
func (s *NamespaceAnnotationState) BFDEnabled() bool {
	if s == nil {
		return false
	}
	return s.bfdEnabled
}

// ACLLoggingChanged returns true if the parsed ACL-logging value differs
// between two annotation snapshots. Two nil snapshots compare equal.
func ACLLoggingChanged(oldState, newState *NamespaceAnnotationState) bool {
	if oldState == nil && newState == nil {
		return false
	}
	if oldState == nil || newState == nil {
		return true
	}
	return oldState.aclLogging != newState.aclLogging
}

// MulticastChanged returns true if the parsed multicast-enabled flag
// differs between two annotation snapshots.
func MulticastChanged(oldState, newState *NamespaceAnnotationState) bool {
	if oldState == nil && newState == nil {
		return false
	}
	if oldState == nil || newState == nil {
		return true
	}
	return oldState.multicastEnabled != newState.multicastEnabled
}

// ExternalGWAnnotationChanged returns true if the parsed
// routing-external-gws annotation (or its BFD flag) differs between two
// snapshots. Two nil snapshots compare equal.
func ExternalGWAnnotationChanged(oldState, newState *NamespaceAnnotationState) bool {
	if oldState == nil && newState == nil {
		return false
	}
	if oldState == nil || newState == nil {
		return true
	}
	if oldState.bfdEnabled != newState.bfdEnabled {
		return true
	}
	// Either side may have a parse error; treat any error change as a change.
	if (oldState.routingGWsErr == nil) != (newState.routingGWsErr == nil) {
		return true
	}
	if oldState.routingGWsErr != nil {
		// Both have errors; compare strings to catch distinct failure modes.
		return oldState.routingGWsErr.Error() != newState.routingGWsErr.Error()
	}
	return !oldState.routingGWs.Equal(newState.routingGWs)
}
