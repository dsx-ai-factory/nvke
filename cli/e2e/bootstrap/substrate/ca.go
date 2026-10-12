// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	clientcert "k8s.io/client-go/util/cert"
)

// CACertFile and CAKeyFile are the bootstrap CA's file names inside CADir.
const (
	CACertFile = "ca.crt"
	CAKeyFile  = "ca.key"
)

// EnsureBootstrapCA ensures that a bootstrap CA certificate and key exist in
// dir. It reuses existing files or generates new files and returns their paths.
func EnsureBootstrapCA(dir string) (certPath, keyPath string, err error) {
	certPath = filepath.Join(dir, CACertFile)
	keyPath = filepath.Join(dir, CAKeyFile)

	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return certPath, keyPath, nil
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("creating the CA directory %s: %w", dir, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generating the CA key: %w", err)
	}

	cert, err := clientcert.NewSelfSignedCACert(clientcert.Config{
		CommonName: "nvkectl bootstrap E2E CA",
		NotBefore:  time.Now().Add(-time.Hour),
	}, key)
	if err != nil {
		return "", "", fmt.Errorf("creating the CA certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encoding the CA key: %w", err)
	}

	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil { //nolint:gosec // a public certificate
		return "", "", fmt.Errorf("writing %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", fmt.Errorf("writing %s: %w", keyPath, err)
	}
	return certPath, keyPath, nil
}
