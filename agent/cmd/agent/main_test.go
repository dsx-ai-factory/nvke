// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}

func TestAuditLoggerMirrorsStructuredRecords(t *testing.T) {
	t.Parallel()

	var fileOutput bytes.Buffer
	var serviceOutput bytes.Buffer
	logger := newAuditLogger(&fileOutput, &serviceOutput, slog.New(slog.DiscardHandler))

	interceptor := newAuditLogUnaryInterceptor(logger)
	_, err := interceptor(
		context.Background(),
		struct{}{},
		&grpc.UnaryServerInfo{FullMethod: "/nvke.NodeAgent/NodePreflight"},
		func(context.Context, any) (any, error) { return struct{}{}, nil },
	)
	require.NoError(t, err)

	if !bytes.Equal(fileOutput.Bytes(), serviceOutput.Bytes()) {
		t.Fatalf("audit outputs differ:\nfile: %s\nservice: %s", fileOutput.Bytes(), serviceOutput.Bytes())
	}

	var record map[string]any
	if err := json.Unmarshal(fileOutput.Bytes(), &record); err != nil {
		t.Fatalf("audit output is not JSON: %v", err)
	}
	if got := record["msg"]; got != "grpc request received" {
		t.Errorf("msg = %v, want %q", got, "grpc request received")
	}
	if got := record["method"]; got != "/nvke.NodeAgent/NodePreflight" {
		t.Errorf("method = %v, want %q", got, "/nvke.NodeAgent/NodePreflight")
	}
}

func TestFanoutWriterAttemptsEveryOutput(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("audit file unavailable")
	var serviceOutput bytes.Buffer
	var diagnosticOutput bytes.Buffer
	errorWriter := writerFunc(func([]byte) (int, error) { return 0, wantErr })
	writer := fanoutWriter{
		outputs: []fanoutOutput{
			{name: "file", writer: errorWriter},
			{name: "service", writer: &serviceOutput},
		},
		diagnostics: slog.New(slog.NewJSONHandler(&diagnosticOutput, nil)),
	}
	input := []byte("audit record")

	n, err := writer.Write(input)

	if n != len(input) {
		t.Errorf("Write() count = %d, want %d", n, len(input))
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Write() error = %v, want an error wrapping %v", err, wantErr)
	}
	if !bytes.Equal(serviceOutput.Bytes(), input) {
		t.Errorf("service output = %q, want %q", serviceOutput.Bytes(), input)
	}

	var diagnostic map[string]any
	require.NoError(t, json.Unmarshal(diagnosticOutput.Bytes(), &diagnostic))
	assert.Equal(t, "failed to write audit output", diagnostic["msg"])
	assert.Equal(t, "file", diagnostic["output"])
	assert.Equal(t, float64(0), diagnostic["output_index"])
	assert.Contains(t, diagnostic["err"], wantErr.Error())
}

func TestFanoutWriterReportsShortWrite(t *testing.T) {
	t.Parallel()

	shortWriter := writerFunc(func([]byte) (int, error) { return 0, nil })
	writer := fanoutWriter{
		outputs: []fanoutOutput{
			{name: "file", writer: io.Discard},
			{name: "service", writer: shortWriter},
		},
		diagnostics: slog.New(slog.DiscardHandler),
	}
	input := []byte("audit record")

	_, err := writer.Write(input)

	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("Write() error = %v, want an error wrapping %v", err, io.ErrShortWrite)
	}
}

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	writes chan struct{}
}

func newSynchronizedBuffer() *synchronizedBuffer {
	return &synchronizedBuffer{writes: make(chan struct{}, 16)}
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.Buffer.Write(p)
	b.mu.Unlock()
	b.writes <- struct{}{}
	return n, err
}

func (b *synchronizedBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Read(p)
}

func (b *synchronizedBuffer) waitForWrite(t *testing.T) {
	t.Helper()
	select {
	case <-b.writes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit record")
	}
}

// The authorizer is the whole of this agent's access control, so these pin what it
// refuses as much as what it admits.
func TestAuthorizeClient(t *testing.T) {
	t.Parallel()

	const agentTrustDomain = "example.org"

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "renamed tenant path", id: "spiffe://example.org/tenant/nvke/service/cwe/bm/cwe"},
		{name: "renamed service path", id: "spiffe://example.org/nvke-service/cwe"},
		{name: "renamed prefix must end at slash", id: "spiffe://example.org/tenant/nvke/service-evil/cwe", wantErr: true},
		{name: "renamed caller in foreign trust domain", id: "spiffe://attacker.test/tenant/nvke/service/cwe", wantErr: true},
		{name: "legacy service path", id: "spiffe://example.org/nke-service/cloud-workflow-engine"},
		{name: "tenant-grammar path", id: "spiffe://example.org/tenant/nke/service/cloud-workflow-engine/bm/cwe"},
		{name: "tenant-grammar path, other service", id: "spiffe://example.org/tenant/nke/service/capi-manager/bm/cwe"},

		// Different paths that merely share leading characters.
		{name: "legacy prefix is not a substring match", id: "spiffe://example.org/nke-service-evil/cwe", wantErr: true},
		{name: "tenant prefix is not a substring match", id: "spiffe://example.org/tenant/nke/service-evil/cwe", wantErr: true},

		// The tenant segment is inside the prefix, so other tenants are refused.
		{name: "other tenant", id: "spiffe://example.org/tenant/acme/service/cloud-workflow-engine/bm/x", wantErr: true},

		{name: "unrelated path", id: "spiffe://example.org/nke-e2e/probe", wantErr: true},
		{name: "foreign trust domain", id: "spiffe://attacker.test/nke-service/cloud-workflow-engine", wantErr: true},
		{name: "trust domain is not a prefix match", id: "spiffe://example.org.attacker.test/nke-service/cwe", wantErr: true},
	}

	authorize := authorizeClient(agentTrustDomain)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := spiffeid.FromString(tc.id)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.id, err)
			}

			err = authorize(id)
			if tc.wantErr && err == nil {
				t.Fatalf("authorizeClient(%q) = nil, want an error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizeClient(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

// A misconfigured authentication mode must stop the agent at startup. The
// alternative is a systemd unit that restarts forever while every handshake
// fails, which is what an unrecognized flag already produces.
func TestValidateAuthConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     AgentConfig
		wantErr string
	}{
		{
			name: "default mode needs no exchange configuration",
			cfg:  AgentConfig{GRPCAuthMode: authModeSPIFFEMTLS},
		},
		{
			name: "default mode tolerates disabled mTLS for development",
			cfg:  AgentConfig{GRPCAuthMode: authModeSPIFFEMTLS, DisableMTLS: true},
		},
		{
			name: "tls-jwt with an exchange endpoint",
			cfg: AgentConfig{
				GRPCAuthMode:           authModeTLSJWT,
				DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
			},
		},
		{
			name: "tls-jwt cannot run on plaintext gRPC",
			cfg: AgentConfig{
				GRPCAuthMode:           authModeTLSJWT,
				DisableMTLS:            true,
				DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
			},
			wantErr: "cannot be combined with --disable-mtls",
		},
		{
			name:    "tls-jwt without an exchange endpoint",
			cfg:     AgentConfig{GRPCAuthMode: authModeTLSJWT},
			wantErr: "requires --dsx-identity-exchange-url",
		},
		{
			name:    "unknown mode",
			cfg:     AgentConfig{GRPCAuthMode: "mtls"},
			wantErr: `--grpc-auth-mode must be "spiffe-mtls" or "tls-jwt", got "mtls"`,
		},
		{
			name:    "empty mode",
			cfg:     AgentConfig{},
			wantErr: `got ""`,
		},
		{
			name: "external mTLS alongside tls-jwt",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeTLSJWT,
				DSXIdentityExchangeURL:   "https://identity.example.test/v1/exchange",
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
		},
		{
			// The controller does not verify the certificate the agent serves
			// in this mode, so the identity token is the only thing that
			// authenticates the agent.
			name: "external mTLS without tls-jwt",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeSPIFFEMTLS,
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
			wantErr: "--nvke-controller-client-ca-bundle requires --grpc-auth-mode=tls-jwt",
		},
		{
			name: "external mTLS cannot run on plaintext gRPC",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeTLSJWT,
				DisableMTLS:              true,
				DSXIdentityExchangeURL:   "https://identity.example.test/v1/exchange",
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
			wantErr: "--nvke-controller-client-ca-bundle cannot be combined with --disable-mtls",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateAuthConfig(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAuthConfig() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAuthConfig() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateAuthConfig() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestControllerAudienceCompatibility(t *testing.T) {
	for _, audience := range []string{"", "nke-controller", "nvke-controller"} {
		if err := validateAuthConfig(AgentConfig{ControllerAudience: audience, GRPCAuthMode: authModeSPIFFEMTLS}); err != nil {
			t.Errorf("audience %q: %v", audience, err)
		}
	}
	if err := validateAuthConfig(AgentConfig{ControllerAudience: "other-service", GRPCAuthMode: authModeSPIFFEMTLS}); err == nil {
		t.Fatal("unsupported audience accepted")
	}
}

func TestControllerClientCABundleName(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		want    string
		wantErr string
	}{
		{name: "unset", want: ""},
		{name: "flag", args: []string{"--nvke-controller-client-ca-bundle=/nvke.crt"}, want: "/nvke.crt"},
		{name: "env", env: map[string]string{"NVKE_CONTROLLER_CLIENT_CA_BUNDLE": "/nvke.crt"}, want: "/nvke.crt"},
		{
			name: "flag overrides env",
			env:  map[string]string{"NVKE_CONTROLLER_CLIENT_CA_BUNDLE": "/env.crt"},
			args: []string{"--nvke-controller-client-ca-bundle=/flag.crt"},
			want: "/flag.crt",
		},
		{
			name: "empty flag overrides env",
			env:  map[string]string{"NVKE_CONTROLLER_CLIENT_CA_BUNDLE": "/env.crt", "NKE_CONTROLLER_CLIENT_CA_BUNDLE": "/other.crt"},
			args: []string{"--nvke-controller-client-ca-bundle="},
			want: "",
		},
		{
			name:    "--controller-client-ca-bundle is not a flag",
			args:    []string{"--controller-client-ca-bundle=/other.crt"},
			wantErr: "flag provided but not defined: -controller-client-ca-bundle",
		},
		{
			// An unread setting leaves external mTLS off, so the agent stays on SPIFFE mTLS.
			name: "NKE_CONTROLLER_CLIENT_CA_BUNDLE is not read",
			env:  map[string]string{"NKE_CONTROLLER_CLIENT_CA_BUNDLE": "/other.crt"},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NVKE_CONTROLLER_CLIENT_CA_BUNDLE", "")
			t.Setenv("NKE_CONTROLLER_CLIENT_CA_BUNDLE", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			fs := flag.NewFlagSet("agent", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			bundle := registerControllerClientCABundleFlag(fs)
			err := fs.Parse(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse(%v) = %v, want error containing %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v) = %v", tc.args, err)
			}
			if got := *bundle; got != tc.want {
				t.Fatalf("controller client CA bundle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAuditLogCarriesIncomingTraceContext(t *testing.T) {
	auditLog := newSynchronizedBuffer()
	server := grpc.NewServer(newGRPCServerOptions(slog.New(slog.NewJSONHandler(auditLog, nil)))...)
	healthpb.RegisterHealthServer(server, health.NewServer())

	listener := bufconn.Listen(1024 * 1024)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := healthpb.NewHealthClient(conn)

	const traceID = "0123456789abcdef0123456789abcdef"
	const parentSpanID = "0123456789abcdef"
	tracedCtx := metadata.AppendToOutgoingContext(
		context.Background(),
		"traceparent", "00-"+traceID+"-"+parentSpanID+"-01",
	)
	_, err = client.Check(tracedCtx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	malformedCtx := metadata.AppendToOutgoingContext(
		context.Background(),
		"traceparent", "not-a-valid-traceparent",
	)
	_, err = client.Check(malformedCtx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)

	decoder := json.NewDecoder(auditLog)
	var tracedRecord map[string]any
	auditLog.waitForWrite(t)
	require.NoError(t, decoder.Decode(&tracedRecord))
	assert.Equal(t, traceID, tracedRecord["trace_id"])
	assert.Equal(t, parentSpanID, tracedRecord["span_id"])
	assert.Equal(t, "/grpc.health.v1.Health/Check", tracedRecord["method"])
	assert.GreaterOrEqual(t, tracedRecord["duration_ms"], float64(0))

	var untracedRecord map[string]any
	auditLog.waitForWrite(t)
	require.NoError(t, decoder.Decode(&untracedRecord))
	assert.NotContains(t, untracedRecord, "trace_id")
	assert.NotContains(t, untracedRecord, "span_id")

	var malformedRecord map[string]any
	auditLog.waitForWrite(t)
	require.NoError(t, decoder.Decode(&malformedRecord))
	assert.NotContains(t, malformedRecord, "trace_id")
	assert.NotContains(t, malformedRecord, "span_id")

	tracedStreamCtx, cancelTracedStream := context.WithCancel(tracedCtx)
	tracedStream, err := client.Watch(tracedStreamCtx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = tracedStream.Recv()
	require.NoError(t, err)
	cancelTracedStream()
	_, err = tracedStream.Recv()
	require.Error(t, err)

	var tracedStreamRecord map[string]any
	auditLog.waitForWrite(t)
	require.NoError(t, decoder.Decode(&tracedStreamRecord))
	assert.Equal(t, traceID, tracedStreamRecord["trace_id"])
	assert.Equal(t, parentSpanID, tracedStreamRecord["span_id"])
	assert.Equal(t, "/grpc.health.v1.Health/Watch", tracedStreamRecord["method"])
	assert.GreaterOrEqual(t, tracedStreamRecord["duration_ms"], float64(0))

	untracedStreamCtx, cancelUntracedStream := context.WithCancel(context.Background())
	untracedStream, err := client.Watch(untracedStreamCtx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = untracedStream.Recv()
	require.NoError(t, err)
	cancelUntracedStream()
	_, err = untracedStream.Recv()
	require.Error(t, err)

	var untracedStreamRecord map[string]any
	auditLog.waitForWrite(t)
	require.NoError(t, decoder.Decode(&untracedStreamRecord))
	assert.NotContains(t, untracedStreamRecord, "trace_id")
	assert.NotContains(t, untracedStreamRecord, "span_id")
	assert.Equal(t, "/grpc.health.v1.Health/Watch", untracedStreamRecord["method"])
}
