// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	"github.com/NVIDIA/nke/agent/pkg/nkeauth"

	"github.com/NVIDIA/nke/controller/internal/auth"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func readCertificatePool(t *testing.T, path string) *x509.CertPool {
	t.Helper()

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(contents))
	return pool
}

// TestIntegration_TLSJWTGRPC is an executable example of the complete
// pre-gRPC authentication sequence over a real TCP connection. It is excluded
// from the standard unit suite because it binds a loopback socket. Run it with:
//
//	go test -tags=integration ./internal/agent -run TestIntegration_TLSJWTGRPC
//
// The node-agent TLS/JWT server implementation should follow the sequence in
// dummyAgentTransportCredentials.ServerHandshake: establish the selected base
// TLS transport, write one NKEAuth JWT record, and only then return the
// connection to gRPC so HTTP/2 processing can begin.
func TestIntegration_TLSJWTGRPC(t *testing.T) {
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
	controllerRoots := readCertificatePool(t, controllerCAPath)

	serverCertPath, serverKeyPath, serverCAPath := writeCertificateFixture(
		t,
		"urn:nvidia:nke:agent",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		time.Now().Add(-time.Hour),
		time.Now().Add(time.Hour),
	)
	serverCertificate, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	require.NoError(t, err)
	serverRoots := readCertificatePool(t, serverCAPath)

	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	issuer := "https://identity.example.test"
	verifier := newTestTLSJWTVerifier(t, issuer, &signingKey.PublicKey)
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

	type parameters struct {
		claims      jwt.MapClaims
		key         *rsa.PrivateKey
		wantSuccess bool
	}
	tests := map[string]parameters{
		"valid JWT reaches gRPC handler": {
			claims:      validClaims(),
			key:         signingKey,
			wantSuccess: true,
		},
		"wrong node is rejected before gRPC handler": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["sub"] = "spiffe://dsx.example.test/bm/" + uuid.NewString()
				return claims
			}(),
			key: signingKey,
		},
		"wrong audience is rejected before gRPC handler": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["aud"] = "other-service"
				return claims
			}(),
			key: signingKey,
		},
		"expired JWT is rejected before gRPC handler": {
			claims: func() jwt.MapClaims {
				claims := validClaims()
				claims["exp"] = now.Add(-2 * time.Minute).Unix()
				return claims
			}(),
			key: signingKey,
		},
		"bad signature is rejected before gRPC handler": {
			claims: validClaims(),
			key:    wrongKey,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			token := jwt.NewWithClaims(jwt.SigningMethodRS256, params.claims)
			token.Header["kid"] = "key-1"
			signed, err := token.SignedString(params.key)
			require.NoError(t, err)

			serverBase := credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{serverCertificate},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    controllerRoots,
				MinVersion:   tls.VersionTLS12,
			})
			serverTransport := &dummyAgentTransportCredentials{base: serverBase, token: signed}
			dummyAgent := &dummyNodeAgentServer{}
			grpcServer := grpc.NewServer(grpc.Creds(serverTransport))
			nkeproto.RegisterNodeAgentServer(grpcServer, dummyAgent)

			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(func() {
				grpcServer.Stop()
				_ = listener.Close()
			})

			clientBase := credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{controllerCertificate},
				RootCAs:      serverRoots,
				ServerName:   "localhost",
				MinVersion:   tls.VersionTLS12,
			})
			clientTransport, err := newTLSJWTTransportCredentials(
				clientBase,
				verifier,
				"dsx.example.test",
				node,
				slog.Default(),
			)
			require.NoError(t, err)

			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(clientTransport))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			response, callErr := nkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(
				ctx,
				&nkeproto.KubeadmStatusRequest{},
			)
			if params.wantSuccess {
				require.NoError(t, callErr)
				assert.Equal(t, "dummy-agent", response.GetAgentVersion())
				assert.EqualValues(t, 1, dummyAgent.calls.Load())
				return
			}
			require.Error(t, callErr)
			assert.EqualValues(t, 0, dummyAgent.calls.Load(), "gRPC handler ran before authentication succeeded")
		})
	}
}

type dummyAgentTransportCredentials struct {
	base  credentials.TransportCredentials
	token string
}

func (c *dummyAgentTransportCredentials) ClientHandshake(
	context.Context,
	string,
	net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("dummy agent credentials support server handshakes only")
}

func (c *dummyAgentTransportCredentials) ServerHandshake(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, authInfo, err := c.base.ServerHandshake(rawConn)
	if err != nil {
		return nil, nil, err
	}
	if err := nkeauth.WriteJWT(conn, c.token); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, authInfo, nil
}

func (c *dummyAgentTransportCredentials) Info() credentials.ProtocolInfo {
	return c.base.Info()
}

func (c *dummyAgentTransportCredentials) Clone() credentials.TransportCredentials {
	clone := *c
	clone.base = c.base.Clone()
	return &clone
}

func (c *dummyAgentTransportCredentials) OverrideServerName(serverNameOverride string) error {
	//nolint:staticcheck // Required by the gRPC TransportCredentials interface.
	return c.base.OverrideServerName(serverNameOverride)
}

type dummyNodeAgentServer struct {
	nkeproto.UnimplementedNodeAgentServer
	calls atomic.Int32
}

func (s *dummyNodeAgentServer) GetKubeadmStatus(
	context.Context,
	*nkeproto.KubeadmStatusRequest,
) (*nkeproto.KubeadmStatusResponse, error) {
	s.calls.Add(1)
	return &nkeproto.KubeadmStatusResponse{AgentVersion: "dummy-agent"}, nil
}
