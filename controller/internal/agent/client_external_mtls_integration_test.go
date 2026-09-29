// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const controllerClientURI = "urn:nvidia:nke:controller"

// TestIntegration_NodeAgentClientExternalMTLS exercises the production client
// over a real TCP, TLS, and gRPC connection. Run it with:
//
//	go test -tags=integration ./internal/agent -run TestIntegration_NodeAgentClient
func TestIntegration_NodeAgentClientExternalMTLS(t *testing.T) {
	now := time.Now().UTC()
	trustedCA := newExternalMTLSTestCA(t, now, "trusted controller CA")
	untrustedCA := newExternalMTLSTestCA(t, now, "untrusted controller CA")
	trustedClient := trustedCA.issueClientCertificate(t, now, controllerClientURI)
	wrongIdentityClient := trustedCA.issueClientCertificate(t, now, "urn:nvidia:nke:controller:other")
	untrustedClient := untrustedCA.issueClientCertificate(t, now, controllerClientURI)
	serverCertificate := newExternalMTLSSelfSignedServerCertificate(t, now)
	serverLeaf, err := x509.ParseCertificate(serverCertificate.Certificate[0])
	require.NoError(t, err)
	require.True(t, now.After(serverLeaf.NotAfter), "the dummy agent certificate must be expired to prove server verification is disabled")
	require.Equal(t, serverLeaf.RawSubject, serverLeaf.RawIssuer, "the dummy agent certificate must be self-signed")

	type parameters struct {
		clientCertificate tls.Certificate
		spiffeEnabled     bool
		wantError         bool
	}
	tests := map[string]parameters{
		"external mTLS on a pinned node with SPIFFE disabled": {
			clientCertificate: trustedClient,
		},
		"external mTLS on a pinned node with SPIFFE enabled": {
			clientCertificate: trustedClient,
			spiffeEnabled:     true,
		},
		"agent rejects a controller certificate from an untrusted CA": {
			clientCertificate: untrustedClient,
			wantError:         true,
		},
		"agent rejects a trusted controller certificate with the wrong URI SAN": {
			clientCertificate: wrongIdentityClient,
			wantError:         true,
		},
		"agent rejects a missing controller certificate": {
			clientCertificate: tls.Certificate{},
			wantError:         true,
		},
	}

	for name, params := range tests {
		params := params
		t.Run(name, func(t *testing.T) {
			recorder := &externalMTLSTestNodeAgent{peerURIs: make(chan string, 1)}
			host, port := startExternalMTLSTestNodeAgent(t, serverCertificate, trustedCA.certPEM, recorder)
			client := &NodeAgentClient{
				transport: &Transport{
					cfg:                       config.Config{NodeAgent: config.NodeAgentConfig{Port: port}},
					externalClientCertificate: &params.clientCertificate,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, err := client.GetKubeadmStatus(ctx, db.Node{
				IP: host, SpiffeEnabled: params.spiffeEnabled, NodeAgentExternalMTLSEnabled: true,
			})
			if params.wantError {
				require.Error(t, err)
				assert.Zero(t, recorder.calls.Load(), "an unauthenticated connection must not reach the RPC handler")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, "dummy-agent", response.GetAgentVersion())
			assert.Equal(t, int32(1), recorder.calls.Load())
			select {
			case peerURI := <-recorder.peerURIs:
				assert.Equal(t, controllerClientURI, peerURI)
			default:
				t.Fatal("dummy agent did not record the authenticated controller URI SAN")
			}
		})
	}
}

func TestIntegration_NodeAgentClientInsecureTransportRegression(t *testing.T) {
	recorder := &externalMTLSTestNodeAgent{peerURIs: make(chan string, 1)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	nkeproto.RegisterNodeAgentServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	client := &NodeAgentClient{
		transport: &Transport{
			cfg:                       config.Config{NodeAgent: config.NodeAgentConfig{Port: port}},
			externalClientCertificate: &tls.Certificate{}, // Available credentials must not override the node's pinned transport.
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	response, err := client.GetKubeadmStatus(ctx, db.Node{IP: host, SpiffeEnabled: false})
	require.NoError(t, err)
	assert.Equal(t, "dummy-agent", response.GetAgentVersion())
	assert.Equal(t, int32(1), recorder.calls.Load())
}

type externalMTLSTestNodeAgent struct {
	nkeproto.UnimplementedNodeAgentServer
	calls    atomic.Int32
	peerURIs chan string
}

func (s *externalMTLSTestNodeAgent) GetKubeadmStatus(ctx context.Context, _ *nkeproto.KubeadmStatusRequest) (*nkeproto.KubeadmStatusResponse, error) {
	s.calls.Add(1)
	if remotePeer, ok := peer.FromContext(ctx); ok {
		if tlsInfo, tlsOK := remotePeer.AuthInfo.(credentials.TLSInfo); tlsOK && len(tlsInfo.State.PeerCertificates) > 0 {
			peerCertificate := tlsInfo.State.PeerCertificates[0]
			if len(peerCertificate.URIs) > 0 {
				s.peerURIs <- peerCertificate.URIs[0].String()
			}
		}
	}
	return &nkeproto.KubeadmStatusResponse{AgentVersion: "dummy-agent"}, nil
}

func startExternalMTLSTestNodeAgent(
	t *testing.T,
	serverCertificate tls.Certificate,
	trustedControllerCAPEM []byte,
	service nkeproto.NodeAgentServer,
) (string, string) {
	t.Helper()

	clientCAs := x509.NewCertPool()
	require.True(t, clientCAs.AppendCertsFromPEM(trustedControllerCAPEM))
	serverTLS := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("controller certificate is required")
			}
			for _, uri := range state.PeerCertificates[0].URIs {
				if uri.String() == controllerClientURI {
					return nil
				}
			}
			return fmt.Errorf("controller certificate must contain URI SAN %q", controllerClientURI)
		},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	nkeproto.RegisterNodeAgentServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return host, port
}

type externalMTLSTestCA struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	certPEM     []byte
}

func newExternalMTLSTestCA(t *testing.T, now time.Time, commonName string) externalMTLSTestCA {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          randomExternalMTLSTestSerial(t),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(certificateDER)
	require.NoError(t, err)
	return externalMTLSTestCA{
		certificate: certificate,
		privateKey:  privateKey,
		certPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
	}
}

func (ca externalMTLSTestCA) issueClientCertificate(t *testing.T, now time.Time, uriSAN string) tls.Certificate {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	uri, err := url.Parse(uriSAN)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: randomExternalMTLSTestSerial(t),
		Subject:      pkix.Name{CommonName: "controller"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{uri},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &privateKey.PublicKey, ca.privateKey)
	require.NoError(t, err)
	return externalMTLSTestKeyPair(t, certificateDER, privateKey)
}

func newExternalMTLSSelfSignedServerCertificate(t *testing.T, now time.Time) tls.Certificate {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: randomExternalMTLSTestSerial(t),
		Subject:      pkix.Name{CommonName: "untrusted-agent.example.invalid"},
		NotBefore:    now.Add(-2 * time.Hour),
		NotAfter:     now.Add(-time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"untrusted-agent.example.invalid"},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	return externalMTLSTestKeyPair(t, certificateDER, privateKey)
}

func externalMTLSTestKeyPair(t *testing.T, certificateDER []byte, privateKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()

	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
	)
	require.NoError(t, err)
	return certificate
}

func randomExternalMTLSTestSerial(t *testing.T) *big.Int {
	t.Helper()

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	return serial
}
