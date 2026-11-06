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
	"log/slog"
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

// newAuditTestServer builds the production handler, enforcing and admitting
// auditGroups[0], with every log line at Debug and above written to the
// returned buffer. A nil key builds it with authentication off.
func newAuditTestServer(t *testing.T, key *rsa.PrivateKey) (http.Handler, *bytes.Buffer) {
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
		authn, err = auth.NewAuthenticator(verifier, auth.ModeEnforcing, logger, nil)
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

func mintAuditToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":    auditIssuer,
		"aud":    auditAudience,
		"sub":    auditSubject,
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": auditGroups,
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

func TestAuditEventNamesTheCaller(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	handler, logged := newAuditTestServer(t, key)
	token := mintAuditToken(t, key)

	clusterPath := "/api/clusters/" + auditClusterID
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		// token is false for a request that authentication refuses.
		token bool
		// want is the audit event, without the always-present "event"; nil
		// means none.
		want map[string]any
	}{
		{
			name: "POST refused by validation", method: http.MethodPost, path: "/api/clusters", body: `{}`, token: true,
			want: map[string]any{
				"method": "POST", "path": "/api/clusters", "status": 400.0, "reason": "request_validation_failed",
				"issuer": auditIssuerName, "subject": auditSubject, "groups": []any{"nvke-admins", "sre"},
			},
		},
		{
			name: "PUT with no operation", method: http.MethodPut, path: clusterPath, body: `{}`, token: true,
			want: map[string]any{
				"method": "PUT", "path": clusterPath, "status": 405.0, "reason": "client_error",
				"issuer": auditIssuerName, "subject": auditSubject, "groups": []any{"nvke-admins", "sre"},
			},
		},
		{
			name: "PATCH", method: http.MethodPatch, path: clusterPath, body: `{}`, token: true,
			want: map[string]any{
				"method": "PATCH", "path": clusterPath, "status": 404.0, "reason": "not_found", "operation": "updateCluster",
				"issuer": auditIssuerName, "subject": auditSubject, "groups": []any{"nvke-admins", "sre"},
			},
		},
		{
			name: "DELETE", method: http.MethodDelete, path: clusterPath, token: true,
			want: map[string]any{
				"method": "DELETE", "path": clusterPath, "status": 404.0, "reason": "not_found", "operation": "deleteCluster",
				"issuer": auditIssuerName, "subject": auditSubject, "groups": []any{"nvke-admins", "sre"},
			},
		},
		{
			name: "DELETE refused by authentication", method: http.MethodDelete, path: clusterPath,
			want: map[string]any{
				"method": "DELETE", "path": clusterPath, "status": 401.0, "reason": "unauthorized",
			},
		},
		{
			name: "GET", method: http.MethodGet, path: "/api/clusters", token: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logged.Reset()
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.token {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			records := auditRecords(t, logged)
			if tt.want == nil {
				assert.Empty(t, records, "a GET is not audited")
				return
			}
			require.Len(t, records, 1, "one audit event per mutating request; log:\n%s", logged)
			got := records[0]
			assert.Equal(t, "INFO", got["level"])
			for _, field := range []string{"time", "level", "msg", "event"} {
				delete(got, field)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// With authentication off there is no caller to name, so the event carries no
// issuer, subject or groups field.
func TestAuditEventWithAuthenticationOff(t *testing.T) {
	handler, logged := newAuditTestServer(t, nil)

	path := "/api/clusters/" + auditClusterID
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, http.NoBody))
	require.Equal(t, http.StatusNotFound, rr.Code)

	records := auditRecords(t, logged)
	require.Len(t, records, 1)
	got := records[0]
	for _, field := range []string{"time", "level", "msg", "event"} {
		delete(got, field)
	}
	assert.Equal(t, map[string]any{
		"method": "DELETE", "path": path, "status": 404.0, "reason": "not_found", "operation": "deleteCluster",
	}, got)
}

// No part of a token reaches the log, at any level: not the admitted token, and
// not one signed by the wrong key that authentication refuses.
func TestAuditLogCarriesNoToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	handler, logged := newAuditTestServer(t, key)

	tokens := map[string]string{
		"admitted": mintAuditToken(t, key),
		"forged":   mintAuditToken(t, wrongKey),
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
