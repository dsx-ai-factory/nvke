// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

// The audit event, through the real NewServer: one per mutating request, naming
// the caller, and never carrying the token.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	auth "github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const (
	auditIssuerName = "test-idp"
	auditIssuer     = "https://idp.test.invalid"
	auditAudience   = "https://cwe.test.invalid"
	auditKeyID      = "audit-key"
	auditSubject    = "alice@example.com"
	auditClusterID  = "11111111-2222-3333-4444-555555555555"
)

var auditGroups = []string{"nvke-admins", "sre"}

type staticKeySource struct{ key crypto.PublicKey }

func (s staticKeySource) KeyFor(context.Context, string) (crypto.PublicKey, error) {
	return s.key, nil
}

// newAuditTestServer builds the production handler, admitting auditGroups[0] in
// mode, with every log line at Debug and above written to the returned buffer.
// A nil key builds it with authentication off.
func newAuditTestServer(t *testing.T, key *rsa.PrivateKey, mode auth.Mode) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var authn *auth.Authenticator
	if key != nil {
		cfg := &auth.Config{
			Audiences: []string{auditAudience},
			Issuers: []auth.Issuer{{
				Name:       auditIssuerName,
				Issuer:     auditIssuer,
				JWKS:       &auth.JWKSKeys{URI: auditIssuer + "/jwks"},
				Algorithms: []string{"RS256"},
				Admit:      &auth.Admit{Groups: auditGroups[:1]},
			}},
		}
		verifier, err := auth.NewVerifier(cfg, map[string]auth.KeySource{auditIssuerName: staticKeySource{key: &key.PublicKey}})
		require.NoError(t, err)
		authn, err = auth.NewAuthenticator(verifier, mode, logger, nil)
		require.NoError(t, err)
	}

	// The cluster writes driven here look the cluster up first; not found ends
	// them there.
	mockTx := &testutil.MockTx{}
	mockTx.On("GetClusterForUpdate", mock.Anything, mock.Anything).Return(nil, db.ErrNotFound)
	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{}, nil)

	srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"}, logger, mockDB, nil, authn, nil)
	require.NoError(t, err)
	return srv.Handler, &logged
}

func mintAuditToken(t *testing.T, key *rsa.PrivateKey, subject string, groups []string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":    auditIssuer,
		"aud":    auditAudience,
		"sub":    subject,
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": groups,
	})
	token.Header["kid"] = auditKeyID
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

// auditRecords returns the audit events in logged.
func auditRecords(t *testing.T, logged *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		if record["event"] == "api.audit" {
			records = append(records, record)
		}
	}
	return records
}

// auditEvent returns the one audit event in logged, without the fields every
// log line carries.
func auditEvent(t *testing.T, logged *bytes.Buffer) map[string]any {
	t.Helper()
	records := auditRecords(t, logged)
	require.Len(t, records, 1, "one audit event per mutating request; log:\n%s", logged)
	got := records[0]
	assert.Equal(t, "INFO", got["level"])
	for _, field := range []string{"time", "level", "msg", "event"} {
		delete(got, field)
	}
	return got
}

func TestAuditEventNamesTheCaller(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	enforcing, enforcingLog := newAuditTestServer(t, key, auth.ModeEnforcing)
	permissive, permissiveLog := newAuditTestServer(t, key, auth.ModePermissive)
	admitted := mintAuditToken(t, key, auditSubject, auditGroups)
	// Verifies, but carries no admitted group.
	unadmitted := mintAuditToken(t, key, "mallory@example.com", []string{"sre"})

	clusterPath := "/api/clusters/" + auditClusterID
	admittedCaller := map[string]any{
		"issuer": auditIssuerName, "subject": auditSubject, "groups": []any{"nvke-admins", "sre"}, "admitted": true,
	}
	unadmittedCaller := map[string]any{
		"issuer": auditIssuerName, "subject": "mallory@example.com", "groups": []any{"sre"}, "admitted": false,
	}
	tests := []struct {
		name       string
		permissive bool
		method     string
		path       string
		body       string
		token      string
		// request is the event's request fields, and caller its caller fields;
		// a nil request means no event.
		request map[string]any
		caller  map[string]any
	}{
		{
			name: "POST refused by validation", method: http.MethodPost, path: "/api/clusters", body: `{}`, token: admitted,
			request: map[string]any{"method": "POST", "path": "/api/clusters", "status": 400.0, "reason": "request_validation_failed"},
			caller:  admittedCaller,
		},
		{
			name: "PUT with no operation", method: http.MethodPut, path: clusterPath, body: `{}`, token: admitted,
			request: map[string]any{"method": "PUT", "path": clusterPath, "status": 405.0, "reason": "client_error"},
			caller:  admittedCaller,
		},
		{
			name: "PATCH", method: http.MethodPatch, path: clusterPath, body: `{}`, token: admitted,
			request: map[string]any{"method": "PATCH", "path": clusterPath, "status": 404.0, "reason": "not_found", "operation": "updateCluster"},
			caller:  admittedCaller,
		},
		{
			name: "DELETE", method: http.MethodDelete, path: clusterPath, token: admitted,
			request: map[string]any{"method": "DELETE", "path": clusterPath, "status": 404.0, "reason": "not_found", "operation": "deleteCluster"},
			caller:  admittedCaller,
		},
		{
			name: "DELETE with no token", method: http.MethodDelete, path: clusterPath,
			request: map[string]any{"method": "DELETE", "path": clusterPath, "status": 401.0, "reason": "unauthorized"},
		},
		{
			name: "DELETE refused by admission", method: http.MethodDelete, path: clusterPath, token: unadmitted,
			request: map[string]any{"method": "DELETE", "path": clusterPath, "status": 403.0, "reason": "forbidden"},
			caller:  unadmittedCaller,
		},
		{
			name: "permissive DELETE served despite admission", permissive: true, method: http.MethodDelete, path: clusterPath, token: unadmitted,
			request: map[string]any{"method": "DELETE", "path": clusterPath, "status": 404.0, "reason": "not_found", "operation": "deleteCluster"},
			caller:  unadmittedCaller,
		},
		{
			name: "GET", method: http.MethodGet, path: "/api/clusters", token: admitted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, logged := enforcing, enforcingLog
			if tt.permissive {
				handler, logged = permissive, permissiveLog
			}
			logged.Reset()
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if tt.request == nil {
				assert.Empty(t, auditRecords(t, logged), "a GET is not audited")
				return
			}
			want := maps.Clone(tt.request)
			maps.Copy(want, tt.caller)
			assert.Equal(t, want, auditEvent(t, logged))
		})
	}
}

// With authentication off there is no caller to name, so the event carries no
// caller fields.
func TestAuditEventWithAuthenticationOff(t *testing.T) {
	handler, logged := newAuditTestServer(t, nil, "")

	path := "/api/clusters/" + auditClusterID
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, http.NoBody))

	assert.Equal(t, map[string]any{
		"method": "DELETE", "path": path, "status": 404.0, "reason": "not_found", "operation": "deleteCluster",
	}, auditEvent(t, logged))
}

// The subject and groups are the caller's strings, so the event bounds them.
func TestAuditEventBoundsTheCaller(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	handler, logged := newAuditTestServer(t, key, auth.ModeEnforcing)

	subject := strings.Repeat("s", 1000)
	groups := make([]string, 0, 102)
	groups = append(groups, auditGroups[0], strings.Repeat("g", 1000))
	for i := range 100 {
		groups = append(groups, fmt.Sprintf("group-%d", i))
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/clusters/"+auditClusterID, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+mintAuditToken(t, key, subject, groups))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	got := auditEvent(t, logged)
	assert.Equal(t, auth.ElideForLog(subject), got["subject"])
	assert.Less(t, len(got["subject"].(string)), 200)
	assert.Equal(t, true, got["groups_truncated"])
	loggedGroups, ok := got["groups"].([]any)
	require.True(t, ok)
	assert.Len(t, loggedGroups, 32)
	assert.Equal(t, auditGroups[0], loggedGroups[0])
	assert.Equal(t, auth.ElideForLog(groups[1]), loggedGroups[1])
	for _, g := range loggedGroups {
		assert.Less(t, len(g.(string)), 200)
	}
}

// No part of a token reaches the log, at any level: not an admitted token, not
// a verified one that admission refuses, and not one signed by the wrong key.
func TestAuditLogCarriesNoToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	handler, logged := newAuditTestServer(t, key, auth.ModePermissive)

	tokens := map[string]string{
		"admitted":   mintAuditToken(t, key, auditSubject, auditGroups),
		"unadmitted": mintAuditToken(t, key, auditSubject, []string{"sre"}),
		"forged":     mintAuditToken(t, wrongKey, auditSubject, auditGroups),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			logged.Reset()
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				req := httptest.NewRequestWithContext(t.Context(), method, "/api/clusters/"+auditClusterID, http.NoBody)
				req.Header.Set("Authorization", "Bearer "+token)
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}
			require.Len(t, auditRecords(t, logged), 2, "the requests were audited; log:\n%s", logged)

			signature := token[strings.LastIndex(token, ".")+1:]
			require.NotEmpty(t, signature)
			assert.NotContains(t, logged.String(), token)
			assert.NotContains(t, logged.String(), signature)
		})
	}
}
