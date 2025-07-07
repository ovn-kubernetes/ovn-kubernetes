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

// Reserve stores the address reservation and its owner.
// Returns an error ErrMACConflict in case the given addresses is already reserved by different owner.
func (n *ReservationManager) Reserve(owner string, mac net.HardwareAddr) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	macKey := mac.String()
	currentOwner, macReserved := n.store[macKey]
	if macReserved && currentOwner != owner {
		return ErrMACConflict
	}

	if macReserved {
		return nil
	}
	n.store[macKey] = owner

	return nil
}

// Release MAC address from store of the given owner.
func (n *ReservationManager) Release(owner string, mac net.HardwareAddr) {
	n.lock.Lock()
	defer n.lock.Unlock()

	macKey := mac.String()
	currentOwner, macReserved := n.store[macKey]
	if !macReserved || currentOwner != owner {
		return
	}

	delete(n.store, macKey)
}
