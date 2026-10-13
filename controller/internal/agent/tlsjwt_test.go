// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nvke/agent/pkg/nkeauth"

	"github.com/dsx-ai-factory/nvke/controller/internal/auth"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const testControllerIdentityURI = "urn:nvidia:nke:controller"

func TestUnit_NewTLSJWTTransportCredentials(t *testing.T) {
	t.Parallel()

	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	verifier := newTestTLSJWTVerifier(t, "https://identity.example.test", &signingKey.PublicKey)
	base := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})

	type parameters struct {
		base        credentials.TransportCredentials
		verifier    *auth.Verifier
		trustDomain string
		wantErr     string
	}
	tests := map[string]parameters{
		"valid": {
			base:        base,
			verifier:    verifier,
			trustDomain: "dsx.example.test",
		},
		"missing base transport": {
			verifier:    verifier,
			trustDomain: "dsx.example.test",
			wantErr:     "requires base transport credentials",
		},
		"missing verifier": {
			base:        base,
			trustDomain: "dsx.example.test",
			wantErr:     "requires a token verifier",
		},
		"invalid trust domain": {
			base:        base,
			verifier:    verifier,
			trustDomain: "https://dsx.example.test",
			wantErr:     "parsing node-agent subject trust domain",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transportCredentials, err := newTLSJWTTransportCredentials(
				params.base,
				params.verifier,
				params.trustDomain,
				db.Node{ID: uuid.New(), Name: "node-a"},
				nil,
			)
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
				assert.Nil(t, transportCredentials)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, transportCredentials)
		})
	}
}

func TestUnit_VerifyNodeSubject(t *testing.T) {
	t.Parallel()

	nodeID := uuid.NewString()
	trustDomain, err := spiffeid.TrustDomainFromString("dsx.example.test")
	require.NoError(t, err)
	type parameters struct {
		subject string
		wantErr bool
	}
	tests := map[string]parameters{
		"exact DSX bare-metal subject": {subject: "spiffe://dsx.example.test/bm/" + nodeID},
		"different node":               {subject: "spiffe://dsx.example.test/bm/" + uuid.NewString(), wantErr: true},
		"different trust domain":       {subject: "spiffe://other.example.test/bm/" + nodeID, wantErr: true},
		"wrong path":                   {subject: "spiffe://dsx.example.test/host/" + nodeID, wantErr: true},
		"query suffix":                 {subject: "spiffe://dsx.example.test/bm/" + nodeID + "?pod=other", wantErr: true},
		"fragment suffix":              {subject: "spiffe://dsx.example.test/bm/" + nodeID + "#other", wantErr: true},
		"userinfo":                     {subject: "spiffe://user@dsx.example.test/bm/" + nodeID, wantErr: true},
		"port":                         {subject: "spiffe://dsx.example.test:443/bm/" + nodeID, wantErr: true},
		"escaped path":                 {subject: "spiffe://dsx.example.test/bm%2F" + nodeID, wantErr: true},
		"not a SPIFFE URI":             {subject: nodeID, wantErr: true},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyNodeSubject(params.subject, trustDomain, nodeID)
			if params.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestUnit_VerifyNodeAudience(t *testing.T) {
	t.Parallel()

	type parameters struct {
		audience any
		wantErr  bool
	}
	tests := map[string]parameters{
		"missing": {
			wantErr: true,
		},
		"string": {
			audience: auth.NodeAgentAudience,
		},
		"legacy string": {
			audience: auth.NodeAgentAudienceLegacy,
		},
		"singleton array": {
			audience: []string{auth.NodeAgentAudience},
		},
		"legacy singleton array": {
			audience: []string{auth.NodeAgentAudienceLegacy},
		},
		"unknown audience": {
			audience: "someone-else",
			wantErr:  true,
		},
		"multiple audiences": {
			audience: []string{auth.NodeAgentAudience, "another-service"},
			wantErr:  true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyNodeAudience(jwt.MapClaims{"aud": params.audience})
			if params.wantErr {
				require.ErrorContains(t, err, `must contain only "nvke-controller" or "nke-controller"`)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestUnit_TLSJWTClientHandshake(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "node-a"}
	controllerCertPath, controllerKeyPath, controllerCAPath := writeCertificateFixture(
		t,
		testControllerIdentityURI,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		time.Now().Add(-time.Hour),
		time.Now().Add(time.Hour),
	)
	controllerCertificate, err := tls.LoadX509KeyPair(controllerCertPath, controllerKeyPath)
	require.NoError(t, err)
	controllerCAPEM, err := os.ReadFile(controllerCAPath)
	require.NoError(t, err)
	controllerRoots := x509.NewCertPool()
	require.True(t, controllerRoots.AppendCertsFromPEM(controllerCAPEM))

	serverCertPath, serverKeyPath, _ := writeCertificateFixture(
		t,
		"urn:nvidia:nke:agent",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		time.Now().Add(-time.Hour),
		time.Now().Add(time.Hour),
	)
	serverCertificate, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	require.NoError(t, err)

	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	issuer := "https://identity.example.test"
	verifier, err := auth.NewVerifier(&auth.Config{
		Audiences: auth.NodeAgentAudiences(),
		Issuers: []auth.Issuer{{
			Name:       auth.NodeAgentIssuerName,
			Issuer:     issuer,
			JWKS:       &auth.JWKSKeys{URI: issuer + "/jwks"},
			Algorithms: []string{"RS256"},
			Admit:      &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "default"}}},
		}},
	}, map[string]auth.KeySource{auth.NodeAgentIssuerName: testKeySource{key: &signingKey.PublicKey}})
	require.NoError(t, err)

	now := time.Now()
	validClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer,
			"sub": "spiffe://dsx.example.test/bm/" + node.ID.String(),
			"aud": auth.NodeAgentAudience,
			"nbf": now.Add(-time.Minute).Unix(),
			"exp": now.Add(time.Minute).Unix(),
		}
	}
	oversized := append([]byte("NKEAuth"), 0, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(oversized[len("NKEAuth")+1:], nkeauth.MaxJWTSize+1)

	type parameters struct {
		claims        jwt.MapClaims
		key           *rsa.PrivateKey
		record        []byte
		followingData []byte
		wantErr       bool
	}
	tests := map[string]parameters{
		"valid": {
			claims:        validClaims(),
			key:           signingKey,
			followingData: []byte("server-http2-bytes"),
		},
		"wrong node": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["sub"] = "spiffe://dsx.example.test/bm/" + uuid.NewString()
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"wrong trust domain": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["sub"] = "spiffe://other.example.test/bm/" + node.ID.String()
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"wrong audience": {
			claims:  func() jwt.MapClaims { claims := validClaims(); claims["aud"] = "other"; return claims }(),
			key:     signingKey,
			wantErr: true,
		},
		"multiple audiences": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["aud"] = []string{auth.NodeAgentAudience, "another-service"}
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"unknown issuer": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["iss"] = "https://other.example.test"
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"expired": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["exp"] = now.Add(-2 * time.Minute).Unix()
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"not valid yet": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["nbf"] = now.Add(2 * time.Minute).Unix()
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"missing expiration": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				delete(claims, "exp")
				return claims
			}(),
			key:     signingKey,
			wantErr: true,
		},
		"bad signature": {claims: validClaims(), key: wrongKey, wantErr: true},
		"bad framing":   {record: []byte("BadAuth0bad!"), wantErr: true},
		"oversized JWT": {record: oversized, wantErr: true},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := params.record
			if params.claims != nil {
				token := jwt.NewWithClaims(jwt.SigningMethodRS256, params.claims)
				token.Header["kid"] = "key-1"
				signed, signErr := token.SignedString(params.key)
				require.NoError(t, signErr)
				var buf bytes.Buffer
				require.NoError(t, nkeauth.WriteJWT(&buf, signed))
				record = buf.Bytes()
			}

			clientRaw, serverRaw := net.Pipe()
			t.Cleanup(func() {
				_ = clientRaw.Close()
				_ = serverRaw.Close()
			})
			type serverOutcome struct {
				received []byte
				err      error
			}
			serverResult := make(chan serverOutcome, 1)
			go func() {
				serverTLS := tls.Server(serverRaw, &tls.Config{
					Certificates: []tls.Certificate{serverCertificate},
					ClientAuth:   tls.RequireAndVerifyClientCert,
					ClientCAs:    controllerRoots,
					MinVersion:   tls.VersionTLS12,
					NextProtos:   []string{"h2"},
				})
				if handshakeErr := serverTLS.HandshakeContext(context.Background()); handshakeErr != nil {
					serverResult <- serverOutcome{err: handshakeErr}
					return
				}
				if _, writeErr := serverTLS.Write(record); writeErr != nil {
					serverResult <- serverOutcome{err: writeErr}
					return
				}
				if !params.wantErr {
					_, writeErr := serverTLS.Write(params.followingData)
					serverResult <- serverOutcome{err: writeErr}
					return
				}
				received, readErr := io.ReadAll(serverTLS)
				serverResult <- serverOutcome{received: received, err: readErr}
			}()

			transportCredentials := (&tlsJWTTransportCredentials{
				base: credentials.NewTLS(&tls.Config{
					Certificates:       []tls.Certificate{controllerCertificate},
					InsecureSkipVerify: true, // The test JWT authenticates the server.
					MinVersion:         tls.VersionTLS12,
				}),
				verifier:    verifier,
				node:        node,
				trustDomain: spiffeid.RequireTrustDomainFromString("dsx.example.test"),
				logger:      slog.Default(),
			})
			conn, _, handshakeErr := transportCredentials.ClientHandshake(context.Background(), "node-a", clientRaw)
			if !params.wantErr {
				require.NoError(t, handshakeErr)
				require.NotNil(t, conn)
				got := make([]byte, len(params.followingData))
				_, readErr := io.ReadFull(conn, got)
				require.NoError(t, readErr)
				assert.Equal(t, params.followingData, got, "authentication framing must leave following transport bytes unread")
				require.NoError(t, (<-serverResult).err)
				return
			}
			require.Error(t, handshakeErr)
			assert.Nil(t, conn)
			outcome := <-serverResult
			require.NoError(t, outcome.err)
			assert.Empty(t, outcome.received, "failed authentication must not send client traffic")
		})
	}
}

type testKeySource struct{ key crypto.PublicKey }

func (s testKeySource) KeyFor(context.Context, string) (crypto.PublicKey, error) { return s.key, nil }

func newTestTLSJWTVerifier(t *testing.T, issuer string, key crypto.PublicKey) *auth.Verifier {
	t.Helper()

	verifier, err := auth.NewVerifier(&auth.Config{
		Audiences: auth.NodeAgentAudiences(),
		Issuers: []auth.Issuer{{
			Name:       auth.NodeAgentIssuerName,
			Issuer:     issuer,
			JWKS:       &auth.JWKSKeys{URI: issuer + "/jwks"},
			Algorithms: []string{"RS256"},
			Admit:      &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "default"}}},
		}},
	}, map[string]auth.KeySource{auth.NodeAgentIssuerName: testKeySource{key: key}})
	require.NoError(t, err)
	return verifier
}

func writeCertificateFixture(
	t *testing.T,
	identityURI string,
	usages []x509.ExtKeyUsage,
	notBefore, notAfter time.Time,
) (string, string, string) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore.Add(-time.Hour),
		NotAfter:              notAfter.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	uri, err := url.Parse(identityURI)
	require.NoError(t, err)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ignored.example.test"},
		DNSNames:     []string{"localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
		URIs:         []*url.URL{uri},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	caPath := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)}), 0o600))
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	return certPath, keyPath, caPath
}
