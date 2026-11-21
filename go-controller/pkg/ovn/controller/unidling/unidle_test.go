// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package unidling

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUnidlingContoller(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Unilding Controller Suite")
	defer GinkgoRecover()
}

var _ = Describe("Unidling Controller", func() {
	var cleanup *libovsdbtest.Context

	BeforeEach(func() {
		cleanup = nil
	})

	AfterEach(func() {
		if cleanup != nil {
			cleanup.Cleanup()
		}
	})

	It("should fail creation when the SB client is not connected", func() {
		client := fake.NewSimpleClientset()
		recorder := record.NewFakeRecorder(10)
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		serviceInformer := informerFactory.Core().V1().Services().Informer()

		// an SB client that was never connected has a nil cache
		dbModel, err := sbdb.FullDatabaseModel()
		Expect(err).NotTo(HaveOccurred())
		sbClient, err := libovsdbclient.NewOVSDBClient(dbModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(sbClient.Cache()).To(BeNil())

		_, err = NewController(
			recorder,
			serviceInformer,
			sbClient,
		)
		Expect(err).To(MatchError(ContainSubstring("not connected")))
	})

	It("should respond to a controller event", func() {
		client := fake.NewSimpleClientset()
		recorder := record.NewFakeRecorder(10)
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		serviceInformer := informerFactory.Core().V1().Services().Informer()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testSetup := libovsdbtest.TestSetup{
			SBData: []libovsdbtest.TestData{
				&sbdb.ControllerEvent{
					EventType: sbdb.ControllerEventEventTypeEmptyLbBackends,
					SeqNum:    8,
					EventInfo: map[string]string{
						"vip":      "10.10.10.10:80",
						"protocol": "tcp",
					},
				},
			},
		}

		var sbClient libovsdbclient.Client
		var err error
		sbClient, cleanup, err = libovsdbtest.NewSBTestHarness(testSetup, nil)
		Expect(err).NotTo(HaveOccurred())

		c, err := NewController(
			recorder,
			serviceInformer,
			sbClient,
		)
		Expect(err).NotTo(HaveOccurred())

		informerFactory.Start(ctx.Done())

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo_ns", Name: "foo_service",
				Annotations: map[string]string{"ovn/idled-at": "2022-02-22T22:22:22Z"},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "10.10.10.10",
				Ports:     []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}},
				Type:      corev1.ServiceTypeClusterIP,
			},
		}
		_, err = client.CoreV1().Services("foo_ns").Create(context.Background(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		cache.WaitForCacheSync(ctx.Done(), serviceInformer.HasSynced)

		go c.Run(ctx.Done())

		// Controller_Event is deleted
		Eventually(
			func() int {
				ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				var events []sbdb.ControllerEvent
				err = sbClient.List(ctx, &events)
				Expect(err).NotTo(HaveOccurred())
				return len(events)
			},
			5*time.Second,
		).Should(Equal(0))

		timeout := time.Tick(5 * time.Second)
		select {
		case event := <-recorder.Events:
			// Recorder event is sent
			Expect(event).To(Equal("Normal NeedPods The service foo_service needs pods"))
		case <-timeout:
			Fail("did not receive controller_event event")
		}
	})

	Context("processing pending controller events", func() {
		var (
			stopCh            chan struct{}
			stopped           chan struct{}
			stop              func()
			releaseWorker     func()
			controllerStarted bool
		)

		BeforeEach(func() {
			stopCh = make(chan struct{})
			stopped = make(chan struct{})
			stop = sync.OnceFunc(func() { close(stopCh) })
			releaseWorker = nil
			controllerStarted = false
		})

		AfterEach(func() {
			if releaseWorker != nil {
				releaseWorker()
			}
			stop()
			if controllerStarted {
				Eventually(stopped, 10*time.Second).Should(BeClosed())
			}
		})

		newSBClient := func(data ...libovsdbtest.TestData) libovsdbclient.Client {
			GinkgoHelper()
			var sbClient libovsdbclient.Client
			var err error
			sbClient, cleanup, err = libovsdbtest.NewSBTestHarness(libovsdbtest.TestSetup{SBData: data}, nil)
			Expect(err).NotTo(HaveOccurred())
			return sbClient
		}

		newController := func(sbClient libovsdbclient.Client, serviceCount int) (*unidlingController, *record.FakeRecorder) {
			GinkgoHelper()
			recorder := record.NewFakeRecorder(serviceCount)
			serviceInformer := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0).Core().V1().Services().Informer()
			c, err := NewController(recorder, serviceInformer, sbClient)
			Expect(err).NotTo(HaveOccurred())
			for i := 0; i < serviceCount; i++ {
				c.AddServiceVIPToName(unidlingTestEvent(i).EventInfo["vip"], corev1.ProtocolTCP, "default", fmt.Sprintf("svc-%d", i))
			}
			return c, recorder
		}

		startController := func(c *unidlingController) {
			controllerStarted = true
			go func() {
				defer GinkgoRecover()
				c.Run(stopCh)
				close(stopped)
			}()
		}

		insertEvents := func(sbClient libovsdbclient.Client, first, count int) {
			GinkgoHelper()
			events := make([]model.Model, count)
			for i := range events {
				events[i] = unidlingTestEvent(first + i)
			}
			ops, err := sbClient.Create(events...)
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			results, err := sbClient.Transact(ctx, ops...)
			Expect(err).NotTo(HaveOccurred())
			_, err = ovsdb.CheckOperationResults(results, ops)
			Expect(err).NotTo(HaveOccurred())
		}

		observeEvents := func(sbClient libovsdbclient.Client, count int) <-chan struct{} {
			observed := make(chan struct{}, count)
			sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
				AddFunc: func(_ string, m model.Model) {
					if _, ok := m.(*sbdb.ControllerEvent); ok {
						observed <- struct{}{}
					}
				},
			})
			return observed
		}

		awaitSignals := func(signals <-chan struct{}, count int) {
			GinkgoHelper()
			for i := 0; i < count; i++ {
				Eventually(signals, 10*time.Second).Should(Receive(), "received %d of %d expected signals", i, count)
			}
		}

		It("should process startup events and bursts without blocking other SB handlers", func() {
			// Insert more than the old queue capacity while deletion of a row
			// that predates handler registration is blocked.
			const burstSize = 1001
			sbClient := newSBClient(unidlingTestEvent(0))
			transactionStarted := make(chan struct{}, 1)
			release := make(chan struct{})
			releaseWorker = sync.OnceFunc(func() { close(release) })
			workerClient := &unidlingTransactionClient{
				Client: sbClient,
				transact: func(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
					select {
					case transactionStarted <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return sbClient.Transact(ctx, ops...)
				},
			}
			c, recorder := newController(workerClient, burstSize+1)
			startController(c)
			awaitSignals(transactionStarted, 1)

			observed := observeEvents(sbClient, burstSize)
			insertEvents(sbClient, 1, burstSize)
			awaitSignals(observed, burstSize)
			releaseWorker()

			expected := make(map[string]bool, burstSize+1)
			for i := 0; i <= burstSize; i++ {
				expected[fmt.Sprintf("Normal NeedPods The service svc-%d needs pods", i)] = true
			}
			for len(expected) > 0 {
				var event string
				Eventually(recorder.Events, 15*time.Second).Should(Receive(&event))
				Expect(expected).To(HaveKey(event), "unexpected or duplicate wakeup")
				delete(expected, event)
			}
			Eventually(func(g Gomega) {
				var pending []sbdb.ControllerEvent
				g.Expect(sbClient.List(context.Background(), &pending)).To(Succeed())
				g.Expect(pending).To(BeEmpty())
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())
		})

		It("should allow SB handlers to progress after the controller stops", func() {
			sbClient := newSBClient()
			c, recorder := newController(sbClient, 1)
			startController(c)
			stop()
			Eventually(stopped, 10*time.Second).Should(BeClosed())

			const count = 1002
			observed := observeEvents(sbClient, count)
			insertEvents(sbClient, 0, count)
			awaitSignals(observed, count)
			Expect(recorder.Events).To(BeEmpty())
			var pending []sbdb.ControllerEvent
			Expect(sbClient.List(context.Background(), &pending)).To(Succeed())
			Expect(pending).To(HaveLen(count))
		})

		It("should leave unprocessed events in SB when stopped during a batch", func() {
			sbClient := newSBClient(unidlingTestEvent(0), unidlingTestEvent(1))
			var transactions atomic.Int32
			workerClient := &unidlingTransactionClient{
				Client: sbClient,
				transact: func(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
					transactions.Add(1)
					results, err := sbClient.Transact(ctx, ops...)
					stop()
					return results, err
				},
			}
			c, recorder := newController(workerClient, 2)
			startController(c)
			Eventually(stopped, 10*time.Second).Should(BeClosed())
			Expect(transactions.Load()).To(Equal(int32(1)))
			Expect(recorder.Events).To(HaveLen(1))
			Eventually(func(g Gomega) {
				var pending []sbdb.ControllerEvent
				g.Expect(sbClient.List(context.Background(), &pending)).To(Succeed())
				g.Expect(pending).To(HaveLen(1))
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())
		})

		It("should retry failed SB deletions without another cache event", func() {
			sbClient := newSBClient(unidlingTestEvent(0))
			var failTransactions atomic.Bool
			failTransactions.Store(true)
			attempted := make(chan struct{}, 1)
			workerClient := &unidlingTransactionClient{
				Client: sbClient,
				transact: func(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
					if failTransactions.Load() {
						select {
						case attempted <- struct{}{}:
						default:
						}
						return nil, errors.New("test transaction failure")
					}
					return sbClient.Transact(ctx, ops...)
				},
			}
			c, recorder := newController(workerClient, 1)
			startController(c)
			awaitSignals(attempted, 1)
			var pending []sbdb.ControllerEvent
			Expect(sbClient.List(context.Background(), &pending)).To(Succeed())
			Expect(pending).To(HaveLen(1))
			Expect(recorder.Events).To(BeEmpty())
			failTransactions.Store(false)

			// No new cache event is generated: the periodic scan must retry the row.
			Eventually(recorder.Events, controllerEventResyncInterval+5*time.Second).Should(
				Receive(Equal("Normal NeedPods The service svc-0 needs pods")))
		})
	})

	It("should update unidled-at annotation when unidling", func() {
		client := fake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		serviceInformer := informerFactory.Core().V1().Services().Informer()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testStartTime := time.Now().Format(time.RFC3339)

		kube := &kube.Kube{
			KClient: client,
		}
		_, err := NewUnidledAtController(kube, serviceInformer)
		Expect(err).NotTo(HaveOccurred())

		informerFactory.Start(ctx.Done())
		cache.WaitForCacheSync(ctx.Done(), serviceInformer.HasSynced)

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "svc1",
			},
		}
		_, err = client.CoreV1().Services("default").Create(context.Background(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = kube.SetAnnotationsOnService("default", "svc1",
			map[string]interface{}{"k8s.ovn.org/idled-at": "2023-02-06T13:48:49Z"})
		Expect(err).ToNot(HaveOccurred())

		err = kube.SetAnnotationsOnService("default", "svc1",
			map[string]interface{}{"k8s.ovn.org/idled-at": nil})
		Expect(err).ToNot(HaveOccurred())

		Eventually(func(g Gomega) {
			alteredSvc, err := client.CoreV1().Services("default").Get(context.Background(), "svc1", metav1.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			unidledAt := alteredSvc.Annotations["k8s.ovn.org/unidled-at"]
			g.Expect(unidledAt).ToNot(BeNil())
			g.Expect(unidledAt >= testStartTime).To(BeTrue(), "expected %s >= %s", unidledAt, testStartTime)
		}).Should(Succeed())
	})
})

// Keep the real cache and database while controlling worker transaction progress.
type unidlingTransactionClient struct {
	libovsdbclient.Client
	transact func(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error)
}

func (c *unidlingTransactionClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return c.transact(ctx, ops...)
}

func unidlingTestEvent(index int) *sbdb.ControllerEvent {
	return &sbdb.ControllerEvent{
		EventType: sbdb.ControllerEventEventTypeEmptyLbBackends,
		SeqNum:    index,
		EventInfo: map[string]string{
			"vip":      fmt.Sprintf("10.10.10.10:%d", 10000+index),
			"protocol": "tcp",
		},
	}
}
