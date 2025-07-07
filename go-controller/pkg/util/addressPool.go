package util

import (
	"net"
	"sync"

	"k8s.io/klog/v2"
)

// NetworkPool tracks allocated IP and MAC addresses for a specific UDN network
type NetworkPool struct {
	allocatedIPs  map[string]string // IP -> owner identifier
	allocatedMACs map[string]string // MAC -> owner identifier
}

// NetworkPoolManager manages IP/MAC pools for all UDN networks
type NetworkPoolManager struct {
	pools map[string]*NetworkPool
	mu    sync.RWMutex
}

// NewNetworkPoolManager creates a new NetworkPoolManager
func NewNetworkPoolManager() *NetworkPoolManager {
	return &NetworkPoolManager{
		pools: make(map[string]*NetworkPool),
	}
}

// getOrCreatePool returns the pool for the given network, creating it if it doesn't exist
func (npm *NetworkPoolManager) getOrCreatePool(networkName string) *NetworkPool {
	if pool, exists := npm.pools[networkName]; exists {
		return pool
	}

	klog.V(5).Infof("Creating new network pool for network: %s", networkName)
	npm.pools[networkName] = &NetworkPool{
		allocatedIPs:  make(map[string]string),
		allocatedMACs: make(map[string]string),
	}
	return npm.pools[networkName]
}

// AddIPsToPool adds multiple IP addresses to the specified network pool with owner tracking
func (npm *NetworkPoolManager) AddIPsToPool(networkName string, ips []*net.IPNet, ownerID string) {
	if len(ips) == 0 {
		return
	}

	npm.mu.Lock()
	defer npm.mu.Unlock()

	pool := npm.getOrCreatePool(networkName)
	var addedIPs []string
	for _, ipNet := range ips {
		if ipNet != nil {
			pool.allocatedIPs[ipNet.IP.String()] = ownerID
			addedIPs = append(addedIPs, ipNet.IP.String())
		}
	}

	klog.V(5).Infof("Added IPs %v to network %s for owner %s", addedIPs, networkName, ownerID)
}

// AddMACToPool adds a MAC address to the specified network pool with owner tracking
func (npm *NetworkPoolManager) AddMACToPool(networkName string, mac net.HardwareAddr, ownerID string) {
	if mac == nil {
		return
	}

	npm.mu.Lock()
	defer npm.mu.Unlock()

	pool := npm.getOrCreatePool(networkName)
	pool.allocatedMACs[mac.String()] = ownerID

	klog.V(5).Infof("Added MAC %s to network %s for owner %s", mac.String(), networkName, ownerID)
}

// RemoveIPsFromPool removes multiple IP addresses from the specified network pool
func (npm *NetworkPoolManager) RemoveIPsFromPool(networkName string, ips []*net.IPNet) {
	if len(ips) == 0 {
		return
	}

	npm.mu.Lock()
	defer npm.mu.Unlock()

	if pool, exists := npm.pools[networkName]; exists {
		var removedIPs []string
		for _, ipNet := range ips {
			if ipNet != nil {
				delete(pool.allocatedIPs, ipNet.IP.String())
				removedIPs = append(removedIPs, ipNet.IP.String())
			}
		}
		klog.V(5).Infof("Removed IPs %v from network %s", removedIPs, networkName)
	}
}

// RemoveMACFromPool removes a MAC address from the specified network pool
func (npm *NetworkPoolManager) RemoveMACFromPool(networkName string, mac net.HardwareAddr) {
	if mac == nil {
		return
	}

	npm.mu.Lock()
	defer npm.mu.Unlock()

	if pool, exists := npm.pools[networkName]; exists {
		delete(pool.allocatedMACs, mac.String())
		klog.V(5).Infof("Removed MAC %s from network %s", mac.String(), networkName)
	}
}

// GetPoolStats returns the number of allocated IPs and MACs for a network (for debugging/monitoring)
func (npm *NetworkPoolManager) GetPoolStats(networkName string) (int, int) {
	npm.mu.RLock()
	defer npm.mu.RUnlock()

	pool, exists := npm.pools[networkName]
	if !exists {
		return 0, 0
	}

	return len(pool.allocatedIPs), len(pool.allocatedMACs)
}

// CheckIPConflicts checks if IP address(es) are already allocated in the network pool by a different owner.
// Returns conflicting IPs only if they are owned by someone other than the requester.
func (npm *NetworkPoolManager) CheckIPConflicts(networkName string, ips []*net.IPNet, ownerID string) []net.IP {
	if len(ips) == 0 {
		return nil
	}

	npm.mu.RLock()
	defer npm.mu.RUnlock()

	pool, exists := npm.pools[networkName]
	if !exists {
		return nil
	}

	var conflictingIPs []net.IP
	for _, ipNet := range ips {
		if ipNet != nil {
			if currentOwner, isAllocated := pool.allocatedIPs[ipNet.IP.String()]; isAllocated && currentOwner != ownerID {
				conflictingIPs = append(conflictingIPs, ipNet.IP)
			}
		}
	}

	return conflictingIPs
}

// IsMACConflict checks if the MAC address is already allocated in the network pool by a different owner
// Returns true only if the MAC is owned by someone other than the requester
func (npm *NetworkPoolManager) IsMACConflict(networkName string, mac net.HardwareAddr, ownerID string) bool {
	if mac == nil {
		return false
	}

	npm.mu.RLock()
	defer npm.mu.RUnlock()

	pool, exists := npm.pools[networkName]
	if !exists {
		return false
	}

	currentOwner, isAllocated := pool.allocatedMACs[mac.String()]
	return isAllocated && currentOwner != ownerID
}
