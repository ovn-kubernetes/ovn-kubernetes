// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package webhook_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/cmd/ovnkube-identity/webhook"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntls "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = BeforeSuite(func() {
	Expect(configv1.Install(k8sscheme.Scheme)).To(Succeed())
})

var _ = Describe("Run", func() {
	var (
		config    webhook.Config
		tlsClient client.Client
	)

	BeforeEach(func() {
		tempDir, err := os.MkdirTemp("", "webhook-test-*")
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			_ = os.RemoveAll(tempDir)
		})

		// Generate valid test certificates
		certPEM, keyPEM, err := ovntesting.GenerateTestCertificate()
		Expect(err).NotTo(HaveOccurred())

		// Write test certificates
		certPath := filepath.Join(tempDir, "tls.crt")
		Expect(os.WriteFile(certPath, certPEM, 0600)).To(Succeed())

		keyPath := filepath.Join(tempDir, "tls.key")
		Expect(os.WriteFile(keyPath, keyPEM, 0600)).To(Succeed())

		tlsClient = fake.NewClientBuilder().
			WithScheme(ovntls.NewScheme()).
			WithObjects(&configv1.APIServer{
				ObjectMeta: metav1.ObjectMeta{
					Name: openshifttls.APIServerName,
				},
				Spec: configv1.APIServerSpec{
					TLSSecurityProfile: &configv1.TLSSecurityProfile{
						Type: configv1.TLSProfileIntermediateType,
					},
				},
			}).
			Build()

		config = webhook.Config{
			CertDir: tempDir,
			Host:    "localhost",
			Port:    19443,
			NewGeneralClient: func(_ *rest.Config) (client.Client, error) {
				return tlsClient, nil
			},
			NewKubernetesClient: func(_ *rest.Config) (kubernetes.Interface, error) {
				return k8sfake.NewClientset(), nil
			},
		}
	})

	JustBeforeEach(func() {
		ctx, cancel := context.WithCancel(context.Background())

		doneCh := make(chan struct{})

		DeferCleanup(func() {
			cancel()
			Eventually(doneCh).Within(10 * time.Second).Should(BeClosed())
		})

		go func() {
			defer GinkgoRecover()

			err := webhook.Run(ctx, &rest.Config{
				Host: "https://localhost:6443",
			}, config)

			close(doneCh)
			Expect(err).NotTo(HaveOccurred())
		}()
	})

	Specify("the HTTP server should use the configured TLS profile from the API server", func() {
		Eventually(func(g Gomega) {
			conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", config.Host, config.Port), &tls.Config{
				InsecureSkipVerify: true,
				MaxVersion:         tls.VersionTLS12, // Force TLS 1.2 to test minimum version
				NextProtos:         []string{"h2"},   // Request HTTP/2 via ALPN
			})

			g.Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			err = conn.Handshake()
			g.Expect(err).NotTo(HaveOccurred())

			state := conn.ConnectionState()

			// For Intermediate profile with MaxVersion TLS 1.2, should negotiate exactly TLS 1.2
			g.Expect(int(state.Version)).To(Equal(tls.VersionTLS12))
			g.Expect(state.CipherSuite).NotTo(BeZero())
			g.Expect(state.NegotiatedProtocol).To(Equal("h2"))
		}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
	})

	Specify("the HTTP server should reject TLS versions below the minimum", func() {
		Eventually(func(g Gomega) {
			// Attempt to connect with TLS 1.1 (below the Intermediate profile minimum of TLS 1.2)
			conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", config.Host, config.Port), &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS10,
				MaxVersion:         tls.VersionTLS11, // Restrict client to TLS 1.1 or below
			})

			if err == nil {
				err = conn.Handshake()
				conn.Close()
			}

			// The connection or handshake should fail because server requires TLS 1.2+
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("protocol version"))
		}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
	})

	Context("", func() {
		var newManagerCalled chan struct{}

		BeforeEach(func() {
			newManagerCalled = make(chan struct{})

			oldNewManager := ovntls.NewManager

			ovntls.NewManager = func(config *rest.Config, options manager.Options) (manager.Manager, error) {
				close(newManagerCalled)
				return ctrl.NewManager(config, options)
			}

			DeferCleanup(func() {
				ovntls.NewManager = oldNewManager
			})
		})

		It("should watch for TLS profile changes", func() {
			// ovntls.WatchProfileAndSignalOnChange creates a controller Manager so it's sufficient here to just verify
			// the ovntls.NewManager hook is called.
			Eventually(newManagerCalled).Should(BeClosed())
		})
	})

	It("should always register the \"/node\" webhook endpoint", func() {
		assertWebhookRequestSuccess(config.Host, config.Port, "node", createNodeAdmissionReviewJSON())
	})

	Context("", func() {
		BeforeEach(func() {
			config.EnableInterconnect = true
		})

		It("should register the \"/pod\" webhook endpoint when interconnect is enabled", func() {
			assertWebhookRequestSuccess(config.Host, config.Port, "pod", createPodAdmissionReviewJSON())
		})
	})
})

func assertWebhookRequestSuccess(host string, port int, endpoint string, payload []byte) {
	httpClient := createWebhookClient()

	Eventually(func(g Gomega) {
		resp, err := httpClient.Post(
			fmt.Sprintf("https://%s/%s", net.JoinHostPort(host, strconv.Itoa(port)), endpoint),
			"application/json",
			bytes.NewBuffer(payload),
		)

		g.Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
}

func createNodeAdmissionReviewJSON() []byte {
	return createAdmissionReviewJSON(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	})
}

func createPodAdmissionReviewJSON() []byte {
	return createAdmissionReviewJSON(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
	})
}

func createAdmissionReviewJSON(obj any) []byte {
	raw, err := json.Marshal(obj)
	Expect(err).NotTo(HaveOccurred())

	objMeta, err := meta.Accessor(obj)
	Expect(err).NotTo(HaveOccurred())

	admissionReview := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Name:      objMeta.GetName(),
			Namespace: objMeta.GetNamespace(),
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: raw,
			},
		},
	}

	admJSON, err := json.Marshal(admissionReview)
	Expect(err).NotTo(HaveOccurred())

	return admJSON
}

func createWebhookClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// For testing, skip certificate verification
				// In production, you'd load the CA cert and verify
				InsecureSkipVerify: true,
			},
			// Force HTTP/2
			ForceAttemptHTTP2: true,
		},
	}
}

func TestWebhook(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Webhook Suite")
}
