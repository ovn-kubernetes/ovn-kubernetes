package mac

import (
	"errors"
	"net"
	"sync"
)

// ReservationManager tracks reserved MAC addresses requests of pods and detect MAC conflicts,
// where one pod request static MAC address that is used by another pod.
type ReservationManager struct {
	// lock for storing a MAC reservation.
	lock sync.Mutex
	// store for reserved MAC address request by owner. Key is MAC address, value is owner identifier.
	store map[string]string
}

// NewManager creates a new ReservationManager.
func NewManager() *ReservationManager {
	return &ReservationManager{
		store: make(map[string]string),
	}
}

var ErrMACConflict = errors.New("MAC address already in use")
var ErrMACReserved = errors.New("MAC address already reserved for the given owner")

// Reserve stores the address reservation and its owner.
// Returns an error ErrMACConflict when "mac" is already reserved by different owner.
// Returns an error ErrMACReserved when "mac" is already reserved by "owner".
func (n *ReservationManager) Reserve(owner string, mac net.HardwareAddr) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	macKey := mac.String()
	currentOwner, macReserved := n.store[macKey]
	if macReserved && currentOwner != owner {
		return ErrMACConflict
	}
	if macReserved {
		return ErrMACReserved
	}

	n.store[macKey] = owner

	return nil
}

var ErrMismatchOwner = errors.New("MAC reserved for different owner")

// Release MAC address from store of the given owner.
// Return an error ErrMismatchOwner when "mac" reserved for different owner than the given one.
func (n *ReservationManager) Release(owner string, mac net.HardwareAddr) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	macKey := mac.String()
	currentOwner, macReserved := n.store[macKey]
	if currentOwner != owner {
		return ErrMismatchOwner
	}
	if !macReserved {
		return nil
	}

	delete(n.store, macKey)

	return nil
}
