// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package webhookcert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "ovn-kubernetes"

func TestEnsureWebhookCert_GeneratesValidChain(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	caPEM, err := EnsureWebhookCert(context.Background(), c, testNamespace, "default")
	if err != nil {
		t.Fatalf("EnsureWebhookCert: %v", err)
	}

	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: SecretName, Namespace: testNamespace}, sec); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, caCertKey, caKeyKey} {
		if len(sec.Data[k]) == 0 {
			t.Fatalf("secret missing key %q", k)
		}
	}

	caBlock, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	srvBlock, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
	srvCert, err := x509.ParseCertificate(srvBlock.Bytes)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	if _, err := srvCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: serverHostname}); err != nil {
		t.Fatalf("server cert does not verify against CA for localhost: %v", err)
	}
}

func TestEnsureWebhookCert_ReusesExistingSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: testNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("server-cert"),
			corev1.TLSPrivateKeyKey: []byte("server-key"),
			caCertKey:               []byte("EXISTING-CA"),
			caKeyKey:                []byte("ca-key"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(existing).Build()
	caPEM, err := EnsureWebhookCert(context.Background(), c, testNamespace, "default")
	if err != nil {
		t.Fatalf("EnsureWebhookCert: %v", err)
	}
	if string(caPEM) != "EXISTING-CA" {
		t.Fatalf("expected existing CA to be reused, got %q", string(caPEM))
	}
}

func TestEnsureWebhookCert_ErrorsWhenExistingSecretMissingCA(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: testNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("server-cert"),
			corev1.TLSPrivateKeyKey: []byte("server-key"),
			// ca.crt intentionally absent
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(existing).Build()
	bundle, err := EnsureWebhookCert(context.Background(), c, testNamespace, "default")
	if err == nil {
		t.Fatal("expected an error when ca.crt is missing, got nil")
	}
	if !strings.Contains(err.Error(), "missing ca.crt") {
		t.Fatalf("expected error to contain %q, got: %v", "missing ca.crt", err)
	}
	if bundle != nil {
		t.Fatalf("expected nil bundle on error, got %q", string(bundle))
	}
}
