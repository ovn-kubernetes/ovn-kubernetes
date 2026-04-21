// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls_test

import (
	"context"
	"crypto/tls"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ovntls "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	_ = Describe("GetProfileFromAPIServer", testGetProfileFromAPIServer)
	_ = Describe("GetProfileRespectingAdherence", testGetProfileRespectingAdherence)
)

func testGetProfileFromAPIServer() {
	t := newTestDriver()

	Context("with Intermediate TLS profile", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSSecurityProfile.Type = configv1.TLSProfileIntermediateType
		})

		It("should return a valid tls.Config with MinVersion TLS 1.2", func(ctx context.Context) {
			profile, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
			Expect(profile.TLSConfig.CipherSuites).NotTo(BeEmpty())
			Expect(profile.TLSConfig.InsecureSkipVerify).To(BeFalse())
			Expect(profile.ProfileSpec).To(Equal(configv1.TLSProfiles[configv1.TLSProfileIntermediateType]))
		})
	})

	Context("with Modern TLS profile (TLS 1.3)", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSSecurityProfile.Type = configv1.TLSProfileModernType
		})

		It("should return a valid tls.Config with MinVersion TLS 1.3 and no CipherSuites", func(ctx context.Context) {
			profile, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS13)))
			Expect(profile.TLSConfig.CipherSuites).To(BeEmpty())
			Expect(profile.ProfileSpec).To(Equal(configv1.TLSProfiles[configv1.TLSProfileModernType]))
		})
	})

	Context("with Custom TLS profile", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS12,
						Ciphers: []string{
							"ECDHE-ECDSA-AES128-GCM-SHA256",
							"ECDHE-RSA-AES128-GCM-SHA256",
							"Unknown",
						},
					},
				},
			}
		})

		It("should return a valid tls.Config with the custom settings", func(ctx context.Context) {
			profile, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
			Expect(profile.TLSConfig.CipherSuites).To(HaveLen(2))
			Expect(profile.ProfileSpec).To(Equal(&t.apiServer.Spec.TLSSecurityProfile.Custom.TLSProfileSpec))
		})
	})

	Context("with nil TLS profile", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSSecurityProfile = nil
		})

		It("should default to Intermediate profile", func(ctx context.Context) {
			profile, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
			Expect(profile.TLSConfig.CipherSuites).NotTo(BeEmpty())
			Expect(profile.ProfileSpec).To(Equal(configv1.TLSProfiles[configv1.TLSProfileIntermediateType]))
		})
	})

	Context("with Custom TLS profile containing only unsupported ciphers for TLS 1.2", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS12,
						Ciphers: []string{
							"FAKE-UNSUPPORTED-CIPHER-1",
							"FAKE-UNSUPPORTED-CIPHER-2",
						},
					},
				},
			}
		})

		It("should return an error preventing silent fallback to defaults", func(ctx context.Context) {
			_, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)
			Expect(err).To(HaveOccurred())
		})
	})

	DescribeTableSubtree("when the APIServer resource",
		func(apiError error) {
			BeforeEach(func() {
				t.tlsClient = fake.NewClientBuilder().
					WithScheme(ovntls.NewScheme()).
					WithInterceptorFuncs(interceptor.Funcs{
						Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
							return apiError
						},
					}).
					Build()
			})

			It("should return the default TLS 1.2 config", func(ctx context.Context) {
				profile, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)

				Expect(err).NotTo(HaveOccurred())
				Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
				Expect(profile.ProfileSpec).To(BeNil())
			})
		},
		Entry("does not exist", apierrors.NewNotFound(schema.GroupResource{}, openshifttls.APIServerName)),
		Entry("is not installed", &meta.NoResourceMatchError{}),
	)

	When("the APIServer resource retrieval fails", func() {
		BeforeEach(func() {
			t.tlsClient = fake.NewClientBuilder().
				WithScheme(ovntls.NewScheme()).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return apierrors.NewServiceUnavailable("mock error")
					},
				}).
				Build()
		})

		It("should return an error", func(ctx context.Context) {
			_, err := ovntls.GetProfileFromAPIServer(ctx, t.tlsClient)
			Expect(err).To(HaveOccurred())
		})
	})
}

func testGetProfileRespectingAdherence() {
	t := newTestDriver()

	BeforeEach(func() {
		t.apiServer.Spec.TLSSecurityProfile.Type = configv1.TLSProfileModernType

	})

	Context("with tlsAdherence set to StrictAllComponents", func() {
		BeforeEach(func() {
			t.apiServer.Spec.TLSAdherence = configv1.TLSAdherencePolicyStrictAllComponents
		})

		It("should honor the cluster TLS profile", func(ctx context.Context) {
			profile, err := ovntls.GetProfileRespectingAdherence(ctx, t.tlsClient, &tls.Config{
				MinVersion: tls.VersionTLS12,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS13)))
			Expect(profile.ProfileSpec).To(Equal(configv1.TLSProfiles[t.apiServer.Spec.TLSSecurityProfile.Type]))
			Expect(profile.Adherence).To(Equal(t.apiServer.Spec.TLSAdherence))
		})
	})

	DescribeTableSubtree("with tlsAdherence",
		func(tlsAdherence configv1.TLSAdherencePolicy) {
			BeforeEach(func() {
				t.apiServer.Spec.TLSAdherence = tlsAdherence
			})

			It("should ignore the cluster TLS profile and return the provided default", func(ctx context.Context) {
				profile, err := ovntls.GetProfileRespectingAdherence(ctx, t.tlsClient, &tls.Config{
					MinVersion: tls.VersionTLS12,
				})

				Expect(err).NotTo(HaveOccurred())
				Expect(profile.TLSConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
				Expect(profile.ProfileSpec).To(Equal(configv1.TLSProfiles[t.apiServer.Spec.TLSSecurityProfile.Type]))
				Expect(profile.Adherence).To(Equal(t.apiServer.Spec.TLSAdherence))
			})
		},
		Entry("set to LegacyAdheringComponentsOnly", configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly),
		Entry("unset", configv1.TLSAdherencePolicyNoOpinion),
	)
}

type testDriver struct {
	tlsClient client.Client
	apiServer *configv1.APIServer
}

func newTestDriver() *testDriver {
	t := &testDriver{}

	BeforeEach(func() {
		t.apiServer = &configv1.APIServer{
			ObjectMeta: metav1.ObjectMeta{
				Name: openshifttls.APIServerName,
			},
			Spec: configv1.APIServerSpec{
				TLSSecurityProfile: &configv1.TLSSecurityProfile{},
			},
		}

		t.tlsClient = fake.NewClientBuilder().WithScheme(ovntls.NewScheme()).Build()
	})

	JustBeforeEach(func(ctx context.Context) {
		if t.apiServer != nil {
			Expect(t.tlsClient.Create(ctx, t.apiServer)).To(Succeed())
		}
	})

	return t
}
