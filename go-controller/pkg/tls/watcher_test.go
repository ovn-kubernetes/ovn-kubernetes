// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls_test

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WatchProfileAndSignalOnChange", func() {
	const signalNum = syscall.SIGUSR2

	var (
		fakeClient       client.Client
		signalCh         chan os.Signal
		initialAPIServer *configv1.APIServer
		watchStarted     bool
	)

	BeforeEach(func() {
		watchStarted = false
		oldNewManager := tls.NewManager

		signalCh = make(chan os.Signal, 1)
		signal.Notify(signalCh, signalNum)

		DeferCleanup(func() {
			tls.NewManager = oldNewManager
			signal.Stop(signalCh)
			close(signalCh)
		})

		// Create initial APIServer object with Intermediate profile
		initialAPIServer = &configv1.APIServer{
			ObjectMeta: metav1.ObjectMeta{
				Name: openshifttls.APIServerName,
			},
			Spec: configv1.APIServerSpec{
				TLSSecurityProfile: &configv1.TLSSecurityProfile{
					Type: configv1.TLSProfileIntermediateType,
				},
				TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			},
		}

		// Replace NewManager to return a FakeManager that uses our fake client
		tls.NewManager = func(config *rest.Config, options manager.Options) (manager.Manager, error) {
			watchStarted = true

			Expect(options.Scheme).NotTo(BeNil(), "Manager options should include a scheme")

			// Verify the scheme recognizes the configv1.APIServer GVK
			expectedGVK := schema.GroupVersionKind{
				Group:   "config.openshift.io",
				Version: "v1",
				Kind:    "APIServer",
			}
			gvks, _, err := options.Scheme.ObjectKinds(&configv1.APIServer{})
			Expect(err).NotTo(HaveOccurred(), "Scheme should recognize configv1.APIServer type")
			Expect(gvks).To(ContainElement(expectedGVK), "Scheme should contain correct GVK for APIServer")

			mgr, err := ctrl.NewManager(config, options)
			if err != nil {
				return nil, err
			}

			fakeMgr := &FakeManager{
				Manager: mgr,
			}

			// Create fake client with interceptor that triggers reconciliation on updates.
			fakeClient = fake.NewClientBuilder().
				WithScheme(k8sscheme.Scheme).
				WithObjects(initialAPIServer).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if err := client.Update(ctx, obj, opts...); err != nil {
							return err
						}

						// If this is an APIServer update, trigger reconciliation manually.
						if apiServer, ok := obj.(*configv1.APIServer); ok {
							fakeMgr.triggerReconcile(ctx, apiServer)
						}

						return nil
					},
				}).
				Build()

			fakeMgr.fakeClient = fakeClient

			return fakeMgr, nil
		}
	})

	When("the profile changes", func() {
		It("should send the signal", func(ctx context.Context) {
			ctx, cancel := context.WithCancel(ctx)
			DeferCleanup(cancel)

			profileSpec, err := openshifttls.GetTLSProfileSpec(initialAPIServer.Spec.TLSSecurityProfile)
			Expect(err).NotTo(HaveOccurred())

			// Start the watcher
			Expect(tls.WatchProfileAndSignalOnChange(ctx, &rest.Config{}, tls.Profile{
				ProfileSpec: &profileSpec,
				Adherence:   initialAPIServer.Spec.TLSAdherence,
			}, signalNum)).To(Succeed())

			// Update the APIServer object to change the TLS profile to Modern
			apiServer := &configv1.APIServer{}
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: openshifttls.APIServerName}, apiServer)).To(Succeed())

			apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileModernType,
			}

			Expect(fakeClient.Update(ctx, apiServer)).To(Succeed())

			// Wait for the signal to be received
			Eventually(signalCh).Within(2 * time.Second).Should(Receive(Equal(signalNum)))
		})
	})

	When("TLS adherence changes", func() {
		It("should send the signal when the TLS adherence changes", func(ctx context.Context) {
			ctx, cancel := context.WithCancel(ctx)
			DeferCleanup(cancel)

			profileSpec, err := openshifttls.GetTLSProfileSpec(initialAPIServer.Spec.TLSSecurityProfile)
			Expect(err).NotTo(HaveOccurred())

			// Start the watcher
			Expect(tls.WatchProfileAndSignalOnChange(ctx, &rest.Config{}, tls.Profile{
				ProfileSpec: &profileSpec,
				Adherence:   initialAPIServer.Spec.TLSAdherence,
			}, signalNum)).To(Succeed())

			// Update the APIServer object to change the TLS adherence
			apiServer := &configv1.APIServer{}
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: openshifttls.APIServerName}, apiServer)).To(Succeed())

			apiServer.Spec.TLSAdherence = configv1.TLSAdherencePolicyStrictAllComponents
			Expect(fakeClient.Update(ctx, apiServer)).To(Succeed())

			// Wait for the signal to be received
			Eventually(signalCh).Within(2 * time.Second).Should(Receive(Equal(signalNum)))
		})
	})

	When("the initial TLS profile is nil", func() {
		It("should not start the watch", func(ctx context.Context) {
			Expect(tls.WatchProfileAndSignalOnChange(ctx, &rest.Config{}, tls.Profile{}, signalNum)).To(Succeed())
			Expect(watchStarted).To(BeFalse())
		})
	})
})

type FakeManager struct {
	manager.Manager
	fakeClient  client.Client
	reconcilers []reconcile.Reconciler
}

func (f *FakeManager) GetClient() client.Client {
	return f.fakeClient
}

// Add captures reconcilers so we can trigger them manually
func (f *FakeManager) Add(runnable manager.Runnable) error {
	if reconciler, ok := runnable.(reconcile.Reconciler); ok {
		f.reconcilers = append(f.reconcilers, reconciler)
	}

	return f.Manager.Add(runnable)
}

func (f *FakeManager) triggerReconcile(ctx context.Context, apiServer *configv1.APIServer) {
	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name: apiServer.Name,
		},
	}

	for _, reconciler := range f.reconcilers {
		go func(r reconcile.Reconciler) {
			_, _ = r.Reconcile(ctx, req)
		}(reconciler)
	}
}
