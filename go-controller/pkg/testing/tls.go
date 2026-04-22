// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// GenerateTestCertificate generates a self-signed certificate for testing.
// Returns PEM-encoded certificate and private key bytes.
func GenerateTestCertificate() (certPEM, keyPEM []byte, err error) {
	// Generate a new ECDSA private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create a certificate template
	notBefore := time.Now()
	notAfter := notBefore.Add(365 * 24 * time.Hour) // Valid for 1 year

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"OVN Kubernetes Test"},
			CommonName:   "localhost",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	// Create the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode certificate to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// Encode private key to PEM
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyBytes,
	})

	return certPEM, keyPEM, nil
}

// CreateTestCertificateFiles generates a self-signed certificate for testing
// and writes it to temporary files. Returns the paths to the cert and key files.
// The caller is responsible for cleaning up the temporary files.
func CreateTestCertificateFiles() (certFile, keyFile string, err error) {
	// Use shared test utility to generate certificate
	certPEM, keyPEM, err := GenerateTestCertificate()
	if err != nil {
		return "", "", err
	}

	// Write cert to temp file
	certTempFile, err := os.CreateTemp("", "test-cert-*.pem")
	if err != nil {
		return "", "", fmt.Errorf("failed to create cert temp file: %w", err)
	}
	certFile = certTempFile.Name()

	if _, err = certTempFile.Write(certPEM); err != nil {
		certTempFile.Close()
		os.Remove(certFile)
		return "", "", fmt.Errorf("failed to write cert to file: %w", err)
	}
	certTempFile.Close()

	// Write key to temp file
	keyTempFile, err := os.CreateTemp("", "test-key-*.pem")
	if err != nil {
		os.Remove(certFile)
		return "", "", fmt.Errorf("failed to create key temp file: %w", err)
	}
	keyFile = keyTempFile.Name()

	if _, err = keyTempFile.Write(keyPEM); err != nil {
		keyTempFile.Close()
		os.Remove(certFile)
		os.Remove(keyFile)
		return "", "", fmt.Errorf("failed to write key to file: %w", err)
	}
	keyTempFile.Close()

	return certFile, keyFile, nil
}
