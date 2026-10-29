// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package portalloc

import (
	"math/rand"
	"sync"
)

// PortAllocator hands out ports from [start, end] uniformly at random, without replacement.
type PortAllocator struct {
	ports []uint16 // pre-shuffled, remaining
	mu    sync.Mutex
}

func New(start, end uint16) *PortAllocator {
	if end < start {
		panic("invalid port range")
	}
	ports := make([]uint16, int(end)-int(start)+1)
	for i := range ports {
		ports[i] = start + uint16(i)
	}
	rand.Shuffle(len(ports), func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })
	return &PortAllocator{ports: ports}
}

func (pr *PortAllocator) Allocate() uint16 {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if len(pr.ports) == 0 {
		panic("port allocation limit reached")
	}
	val := pr.ports[0]
	pr.ports = pr.ports[1:]
	return val
}
