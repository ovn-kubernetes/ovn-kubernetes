package metrics

import (
	"github.com/onsi/ginkgo"
)

var _ = ginkgo.Describe("OVN Northd metrics", func() {
	ginkgo.Context("Function signature compatibility for dependency injection", func() {
		ginkgo.It("compiles against the expected function signature", func() {
			// Compile-time assertion: will fail to compile if the signature changes.
			var fn func(func() bool, <-chan struct{}) = RegisterOvnNorthdMetrics
			_ = fn
		})

		ginkgo.It("returns immediately when the gate returns false", func() {
			stop := make(chan struct{})
			defer close(stop)
			// Should not register metrics or start updaters when gate is false; just ensure no panic.
			RegisterOvnNorthdMetrics(func() bool { return false }, stop)
		})
	})
})
