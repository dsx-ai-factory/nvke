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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeControllerCA returns the path to a PEM bundle holding a freshly minted
// CA, along with a client certificate it signed.
func writeControllerCA(t *testing.T) (string, tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	clientTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, &clientTemplate, caCert, &clientKey.PublicKey, caKey)
	require.NoError(t, err)

	bundlePath := filepath.Join(t.TempDir(), "controller-ca.crt")
	require.NoError(t, os.WriteFile(bundlePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))

	return bundlePath, tls.Certificate{Certificate: [][]byte{clientDER}, PrivateKey: clientKey}
}

func TestServerConfig_RequiresAndVerifiesTheControllerCertificate(t *testing.T) {
	t.Parallel()

	bundlePath, _ := writeControllerCA(t)

	cfg, err := ServerConfig(bundlePath)
	require.NoError(t, err)

	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.NotNil(t, cfg.ClientCAs)
	// The agent serves a certificate the controller does not verify, but it
	// still has to present one for the handshake to complete.
	require.Len(t, cfg.Certificates, 1)
	require.NotNil(t, cfg.Certificates[0].Leaf)
	assert.Equal(t, serverCertificateSubject, cfg.Certificates[0].Leaf.Subject.CommonName)
}

func TestServerConfig_GeneratesADistinctCertificateEachTime(t *testing.T) {
	t.Parallel()

	bundlePath, _ := writeControllerCA(t)

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
// handshake, so the pool wiring is proven rather than asserted on.
func TestServerConfig_HandshakeAcceptsSignedAndRejectsUnsigned(t *testing.T) {
	t.Parallel()

	bundlePath, signed := writeControllerCA(t)
	_, foreign := writeControllerCA(t)

	serverCfg, err := ServerConfig(bundlePath)
	require.NoError(t, err)

	tests := map[string]struct {
		clientCertificate tls.Certificate
		wantErr           bool
	}{
		"certificate from the configured ca": {clientCertificate: signed, wantErr: false},
		"certificate from another ca":        {clientCertificate: foreign, wantErr: true},
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
