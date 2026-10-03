// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package webhookcert generates and persists the self-signed serving certificate
// for the ovnkube-identity validating webhook. It is the Go equivalent of the
// Helm chart's render-time genCA/genSignedCert, but idempotent: the cert is
// generated once into the `ovnkube-webhook-cert` Secret and reused thereafter,
// so reconciles don't rotate (and churn) the trust chain.
package webhookcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretName is the Secret holding the webhook server keypair (+ CA for rotation).
const SecretName = "ovnkube-webhook-cert"

const (
	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 365 * 24 * time.Hour

	caCertKey      = "ca.crt"
	caKeyKey       = "ca.key"
	serverHostname = "localhost"
)

// EnsureWebhookCert returns the PEM CA bundle for the ovnkube-identity webhook.
// On first call it generates a self-signed CA + a localhost server cert and
// persists them (tls.crt/tls.key/ca.crt/ca.key) in the Secret, labeled so the
// operator's label-based cleanup removes it. On later calls it returns the
// existing Secret's ca.crt unchanged.
// TODO(rotation): when the served cert is near expiry, regenerate the server cert from the persisted CA key.
func EnsureWebhookCert(ctx context.Context, c client.Client, namespace, crName string) ([]byte, error) {
	existing := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: namespace}, existing)
	if err == nil {
		return caFromSecret(existing)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get webhook cert secret: %w", err)
	}

	m, err := generate()
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":           "ovnk8s-operator",
				"ovnk8s-operator.ovn.org/ovnk8soperator": crName,
				"ovnk8s-operator.ovn.org/component":      "ovnkubeIdentity",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       m.serverCertPEM,
			corev1.TLSPrivateKeyKey: m.serverKeyPEM,
			caCertKey:               m.caCertPEM,
			caKeyKey:                m.caKeyPEM,
		},
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if gerr := c.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: namespace}, existing); gerr != nil {
				return nil, fmt.Errorf("webhook cert secret already exists but re-get failed: %w", gerr)
			}
			return caFromSecret(existing)
		}
		return nil, fmt.Errorf("create webhook cert secret: %w", err)
	}
	return m.caCertPEM, nil
}

// caFromSecret extracts the ca.crt field from the given Secret, returning an
// error if the field is absent or empty.
func caFromSecret(s *corev1.Secret) ([]byte, error) {
	ca := s.Data[caCertKey]
	if len(ca) == 0 {
		return nil, fmt.Errorf("secret %s/%s missing ca.crt", s.Namespace, s.Name)
	}
	return ca, nil
}

type material struct {
	caCertPEM, caKeyPEM         []byte
	serverCertPEM, serverKeyPEM []byte
}

func generate() (*material, error) {
	now := time.Now()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	// SerialNumber is left nil so CreateCertificate generates a random,
	// RFC 5280-compliant serial number (Go 1.22+).
	caTmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "ovnkube-webhook-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	srvTmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: serverHostname},
		DNSNames:    []string{serverHostname},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(serverValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server cert: %w", err)
	}

	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return nil, fmt.Errorf("marshal server key: %w", err)
	}

	toPEM := func(typ string, der []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	}
	return &material{
		caCertPEM:     toPEM("CERTIFICATE", caDER),
		caKeyPEM:      toPEM("EC PRIVATE KEY", caKeyDER),
		serverCertPEM: toPEM("CERTIFICATE", srvDER),
		serverKeyPEM:  toPEM("EC PRIVATE KEY", srvKeyDER),
	}, nil
}
