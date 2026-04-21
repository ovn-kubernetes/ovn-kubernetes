// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"crypto/tls"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"
	"github.com/openshift/library-go/pkg/crypto"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Profile struct {
	TLSConfig   *tls.Config
	ProfileSpec *configv1.TLSProfileSpec
	Adherence   configv1.TLSAdherencePolicy
}

func NewScheme() *runtime.Scheme {
	clientScheme := runtime.NewScheme()
	utilruntime.Must(configv1.Install(clientScheme))

	return clientScheme
}

// GetProfileFromAPIServer fetches the TLS security profile from apiserver.config.openshift.io/cluster
// and converts it to a tls.Config. It also returns the TLS profile spec for use with SecurityProfileWatcher.
//
// This implements OCP 4.23 TLS profile compliance by fetching the cluster's TLS profile
// and applying it using the official controller-runtime-common/pkg/tls package.
//
// If not running in OpenShift (E2E tests run on vanilla Kubernetes (kind)), it returns TLS 1.2.
func GetProfileFromAPIServer(ctx context.Context, client client.Client) (Profile, error) {
	return getProfileFromAPIServer(ctx, client, false, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Use Go's default cipher suites
	})
}

// GetProfileRespectingAdherence fetches the TLS security profile from apiserver.config.openshift.io/cluster
// and honors the tlsAdherence policy. Returns the TLS config and profile spec for use with SecurityProfileWatcher.
//
// This function implements the tlsAdherence behavior:
// - StrictAllComponents: Honor the cluster's TLS profile
// - LegacyAdheringComponentsOnly (or unset): Return the provided default TLS config
//
// This allows components that didn't previously honor cluster TLS to maintain backward compatibility
// while still providing secure TLS defaults.
func GetProfileRespectingAdherence(ctx context.Context, client client.Client, defaultTLSConfig *tls.Config) (Profile, error) {
	return getProfileFromAPIServer(ctx, client, true, defaultTLSConfig)
}

func getProfileFromAPIServer(ctx context.Context, tlsClient client.Client, checkTLSAdherence bool, defaultTLSConfig *tls.Config,
) (Profile, error) {
	// Fetch the API Server configuration to get both TLS profile and adherence policy
	apiServer := &configv1.APIServer{}
	if err := tlsClient.Get(ctx, types.NamespacedName{Name: openshifttls.APIServerName}, apiServer); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			// Not running on OpenShift, use default TLS 1.2 config
			klog.Info("OpenShift TLS profile API not available, using default TLS configuration")
			return Profile{TLSConfig: defaultTLSConfig}, nil
		}

		return Profile{}, fmt.Errorf("failed to fetch apiserver.config.openshift.io/%s: %w", openshifttls.APIServerName, err)
	}

	profileSpec, err := openshifttls.GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return Profile{}, fmt.Errorf("failed to get TLS profile spec: %w", err)
	}

	info := Profile{
		ProfileSpec: &profileSpec,
		Adherence:   apiServer.Spec.TLSAdherence,
	}

	if checkTLSAdherence && !crypto.ShouldHonorClusterTLSProfile(apiServer.Spec.TLSAdherence) {
		info.TLSConfig = defaultTLSConfig
		return info, nil
	}

	// Convert profile spec to tls.Config using official OpenShift package
	tlsConfigFunc, unsupportedCiphers := openshifttls.NewTLSConfigFromProfile(profileSpec)

	// Log warnings for any unsupported ciphers
	for _, cipher := range unsupportedCiphers {
		klog.Warningf("Cipher suite %q not available in this Go version, skipping", cipher)
	}

	// Create and configure tls.Config
	info.TLSConfig = &tls.Config{}
	tlsConfigFunc(info.TLSConfig)

	// Validate that cipher filtering didn't leave us with an empty list for pre-TLS 1.3
	if info.TLSConfig.MinVersion < tls.VersionTLS13 {
		profileCipherCount := len(profileSpec.Ciphers)
		actualCipherCount := len(info.TLSConfig.CipherSuites)

		if profileCipherCount > 0 && actualCipherCount == 0 {
			return Profile{}, fmt.Errorf("TLS profile specified %d cipher suites but none are supported by this Go version",
				profileCipherCount)
		}
	}

	klog.Infof("Applied TLS configuration: MinVersion=%s, CipherSuites=%d, Adherence=%s",
		crypto.TLSVersionToNameOrDie(info.TLSConfig.MinVersion), len(info.TLSConfig.CipherSuites), apiServer.Spec.TLSAdherence)

	return info, nil
}
