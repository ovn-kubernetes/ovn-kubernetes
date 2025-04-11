package provider

import "sync"

type portAllocator struct {
	start uint16
	end   uint16
	next  uint16
	mu    sync.Mutex
}

func newPortAllocator(start, end uint16) *portAllocator {
	return &portAllocator{start: start, end: end, next: start, mu: sync.Mutex{}}
}

func (pr *portAllocator) allocate() uint16 {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	val := pr.next
	if val == 0 {
		panic("port allocation is zero and may have been exhausted")
	}
	pr.next += 1
	if pr.next > pr.end {
		panic("port allocation limit reached")
	}
	return val
}
