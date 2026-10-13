// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package externalmtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controllerCA stands in for the externally operated CA that issues the
// controller's client certificate. It signs more than one certificate so a
// test can tell "wrong CA" apart from "right CA, wrong service".
type controllerCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	bundlePath  string
	serial      int64
}

// newControllerCA mints a CA and writes its PEM bundle, returning the handle
// whose bundlePath ServerConfig reads.
func newControllerCA(t *testing.T) *controllerCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	bundlePath := filepath.Join(t.TempDir(), "controller-ca.crt")
	require.NoError(t, os.WriteFile(bundlePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	return &controllerCA{certificate: certificate, key: key, bundlePath: bundlePath, serial: 1}
}

// issueClient signs a client certificate carrying uri as its only URI SAN. An
// empty uri issues one with none, which is what a certificate from the right
// CA but the wrong issuing process looks like.
func (ca *controllerCA) issueClient(t *testing.T, uri string) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	ca.serial++
	template := x509.Certificate{
		SerialNumber: big.NewInt(ca.serial),
		Subject:      pkix.Name{CommonName: "test-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if uri != "" {
		parsed, parseErr := url.Parse(uri)
		require.NoError(t, parseErr)
		template.URIs = []*url.URL{parsed}
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, ca.certificate, &key.PublicKey, ca.key)
	require.NoError(t, err)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestServerConfig_RequiresAndVerifiesTheControllerCertificate(t *testing.T) {
	t.Parallel()

	cfg, err := ServerConfig(newControllerCA(t).bundlePath)
	require.NoError(t, err)

	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.NotNil(t, cfg.ClientCAs)
	// Chaining to the bundle is not sufficient on its own; the peer's identity
	// is checked too.
	require.NotNil(t, cfg.VerifyConnection)
	// The agent serves a certificate the controller does not verify, but it
	// still has to present one for the handshake to complete.
	require.Len(t, cfg.Certificates, 1)
	require.NotNil(t, cfg.Certificates[0].Leaf)
	assert.Equal(t, serverCertificateSubject, cfg.Certificates[0].Leaf.Subject.CommonName)
}

func TestServerConfig_GeneratesADistinctCertificateEachTime(t *testing.T) {
	t.Parallel()

	bundlePath := newControllerCA(t).bundlePath

	first, err := ServerConfig(bundlePath)
	require.NoError(t, err)
	second, err := ServerConfig(bundlePath)
	require.NoError(t, err)

	assert.NotEqual(t, first.Certificates[0].Leaf.SerialNumber, second.Certificates[0].Leaf.SerialNumber)
}

func TestServerConfig_RejectsAnUnusableBundle(t *testing.T) {
	t.Parallel()

	type parameters struct {
		path    func(t *testing.T) string
		wantErr string
	}
	tests := map[string]parameters{
		"empty path": {
			path:    func(*testing.T) string { return "" },
			wantErr: "requires a controller client ca bundle",
		},
		"missing file": {
			path:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.pem") },
			wantErr: "reading controller client ca bundle",
		},
		"no certificates": {
			path: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "empty.pem")
				require.NoError(t, os.WriteFile(path, []byte("not a certificate\n"), 0o600))
				return path
			},
			wantErr: "contains no certificates",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := ServerConfig(test.path(t))
			assert.Nil(t, cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

// TestServerConfig_HandshakeAcceptsSignedAndRejectsUnsigned drives a real TLS
// handshake, so the pool wiring and the identity check are proven rather than
// asserted on.
func TestServerConfig_HandshakeAcceptsSignedAndRejectsUnsigned(t *testing.T) {
	t.Parallel()

	ca := newControllerCA(t)

	serverCfg, err := ServerConfig(ca.bundlePath)
	require.NoError(t, err)

	tests := map[string]struct {
		clientCertificate tls.Certificate
		wantErr           bool
	}{
		"certificate from the configured ca": {
			clientCertificate: ca.issueClient(t, ControllerURI),
			wantErr:           false,
		},
		"legacy controller URI remains accepted": {
			clientCertificate: ca.issueClient(t, LegacyControllerURI),
		},
		"certificate from another ca": {
			clientCertificate: newControllerCA(t).issueClient(t, ControllerURI),
			wantErr:           true,
		},
		// The case the CA pool cannot catch: the bundle is an externally
		// operated CA, so a certificate it issued to something other than the
		// controller still chains.
		"certificate from the configured ca with another uri san": {
			clientCertificate: ca.issueClient(t, ControllerURI+":other"),
			wantErr:           true,
		},
		"certificate from the configured ca with no uri san": {
			clientCertificate: ca.issueClient(t, ""),
			wantErr:           true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })

			serverErr := make(chan error, 1)
			go func() {
				accepted, acceptErr := listener.Accept()
				if acceptErr != nil {
					serverErr <- acceptErr
					return
				}
				defer func() { _ = accepted.Close() }()
				server := tls.Server(accepted, serverCfg)
				_ = server.SetDeadline(time.Now().Add(10 * time.Second))
				serverErr <- server.Handshake()
			}()

			dialer := &net.Dialer{Timeout: 10 * time.Second}
			client, err := tls.DialWithDialer(dialer, "tcp", listener.Addr().String(), &tls.Config{
				MinVersion:         tls.VersionTLS12,
				Certificates:       []tls.Certificate{test.clientCertificate},
				InsecureSkipVerify: true, //nolint:gosec // Mirrors the controller, which does not verify the agent certificate.
			})
			if err == nil {
				t.Cleanup(func() { _ = client.Close() })
			}

			if test.wantErr {
				require.Error(t, <-serverErr)
				return
			}
			require.NoError(t, <-serverErr)
		})
	}
}

// TestVerifyControllerIdentity_RejectsAnEmptyPeerChain covers the guard the
// handshake cannot reach: RequireAndVerifyClientCert fails a certificateless
// connection before VerifyConnection runs, so the branch is only reachable if
// that setting is ever relaxed.
func TestVerifyControllerIdentity_RejectsAnEmptyPeerChain(t *testing.T) {
	t.Parallel()

	err := verifyControllerIdentity(tls.ConnectionState{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "presented no certificate")
}
