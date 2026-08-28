// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
)

func TestEnsureBootstrapCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := EnsureBootstrapCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		t.Fatal("bootstrap CA files are not PEM encoded")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Fatal("bootstrap certificate is not a CA")
	}
	publicKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("bootstrap certificate uses %T, want an ECDSA public key", cert.PublicKey)
	}
	if !publicKey.Equal(&key.PublicKey) {
		t.Fatal("bootstrap certificate and private key do not match")
	}

	secondCertPath, secondKeyPath, err := EnsureBootstrapCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	secondCertPEM, err := os.ReadFile(secondCertPath)
	if err != nil {
		t.Fatal(err)
	}
	secondKeyPEM, err := os.ReadFile(secondKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if certPath != secondCertPath || keyPath != secondKeyPath ||
		!bytes.Equal(certPEM, secondCertPEM) || !bytes.Equal(keyPEM, secondKeyPEM) {
		t.Fatal("EnsureBootstrapCA did not reuse the existing CA files")
	}
}
