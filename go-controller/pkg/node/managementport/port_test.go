package managementport

import (
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockManagementPort is a mock implementation of managementPort interface for testing
type mockManagementPort struct {
	createFunc      func() error
	doReconcileFunc func() error
	period          time.Duration

	createCalls      int32
	doReconcileCalls int32
}

func (m *mockManagementPort) create() error {
	atomic.AddInt32(&m.createCalls, 1)
	if m.createFunc != nil {
		return m.createFunc()
	}
	return nil
}

func (m *mockManagementPort) reconcilePeriod() time.Duration {
	if m.period > 0 {
		return m.period
	}
	return 10 * time.Millisecond
}

func (m *mockManagementPort) doReconcile() error {
	atomic.AddInt32(&m.doReconcileCalls, 1)
	if m.doReconcileFunc != nil {
		return m.doReconcileFunc()
	}
	return nil
}

func (m *mockManagementPort) getCreateCalls() int {
	return int(atomic.LoadInt32(&m.createCalls))
}

var _ = Describe("Management Port start() tests", func() {
	var stopChan chan struct{}

	BeforeEach(func() {
		stopChan = make(chan struct{})
	})

	AfterEach(func() {
		close(stopChan)
	})

	Context("When starting management port", func() {
		It("should call create() once on start", func() {
			mp := &mockManagementPort{}
			_, err := start(mp, stopChan)
			Expect(err).NotTo(HaveOccurred())
			Expect(mp.getCreateCalls()).To(Equal(1))
		})

		It("should return error if create() fails on start", func() {
			mp := &mockManagementPort{
				createFunc: func() error {
					return errors.New("create failed")
				},
			}
			_, err := start(mp, stopChan)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("create failed"))
		})

		It("should return nil reconcile function for nil managementPort", func() {
			reconcile, err := start(nil, stopChan)
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcile).NotTo(BeNil())
			// Should not panic when called
			reconcile()
		})
	})

	Context("When reconciling management port", func() {
		It("should call create() when doReconcile() fails", func() {
			reconcileErr := errors.New("interface not found")
			doReconcileCalled := make(chan struct{}, 10)
			createCalled := make(chan struct{}, 10)

			mp := &mockManagementPort{
				period: 50 * time.Millisecond,
				doReconcileFunc: func() error {
					doReconcileCalled <- struct{}{}
					return reconcileErr
				},
				createFunc: func() error {
					createCalled <- struct{}{}
					return nil
				},
			}

			reconcile, err := start(mp, stopChan)
			Expect(err).NotTo(HaveOccurred())
			// create() called once during start
			Expect(mp.getCreateCalls()).To(Equal(1))

			// Trigger reconciliation
			reconcile()

			// Wait for doReconcile to be called
			Eventually(doReconcileCalled, 1*time.Second).Should(Receive())
			// Wait for create to be called (due to doReconcile failure)
			Eventually(createCalled, 1*time.Second).Should(Receive())

			// create() should have been called again (once at start + once during reconcile)
			Eventually(func() int {
				return mp.getCreateCalls()
			}, 1*time.Second).Should(BeNumerically(">=", 2))
		})

		It("should not call create() when doReconcile() succeeds", func() {
			doReconcileCalled := make(chan struct{}, 10)

			mp := &mockManagementPort{
				period: 50 * time.Millisecond,
				doReconcileFunc: func() error {
					doReconcileCalled <- struct{}{}
					return nil
				},
			}

			reconcile, err := start(mp, stopChan)
			Expect(err).NotTo(HaveOccurred())
			// create() called once during start
			Expect(mp.getCreateCalls()).To(Equal(1))

			// Trigger reconciliation
			reconcile()

			// Wait for doReconcile to be called
			Eventually(doReconcileCalled, 1*time.Second).Should(Receive())

			// create() should still only have been called once (at start)
			Consistently(func() int {
				return mp.getCreateCalls()
			}, 100*time.Millisecond).Should(Equal(1))
		})
	})
})
