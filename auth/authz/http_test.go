// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

const (
	forbiddenBody = `{"message":"Not authorized"}` + "\n"
	internalBody  = `{"message":"Internal Server Error"}` + "\n"

	// secret stands for error text a response must never carry.
	secret = "dial tcp db.internal.example:5432"
)

// TestWriteError checks each error's status and body, and that no response
// carries the error's text.
func TestWriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
		wantLogged bool
	}{
		{name: "not found", err: authz.ErrNotFound, wantStatus: http.StatusNotFound},
		{name: "a wrapped not found", err: fmt.Errorf("get cluster: %s: %w", secret, authz.ErrNotFound), wantStatus: http.StatusNotFound},
		{name: "denied", err: authz.ErrDenied, wantStatus: http.StatusForbidden, wantBody: forbiddenBody},
		{name: "no tenant", err: authz.ErrNoTenant, wantStatus: http.StatusForbidden, wantBody: forbiddenBody},
		{
			name: "failing closed", err: fmt.Errorf("%w: stub: %s", authz.ErrDenied, secret),
			wantStatus: http.StatusForbidden, wantBody: forbiddenBody,
		},
		{
			name: "no authorized caller", err: fmt.Errorf("%w: %s", authz.ErrNoCaller, secret),
			wantStatus: http.StatusInternalServerError, wantBody: internalBody, wantLogged: true,
		},
		{
			name: "an internal error", err: fmt.Errorf("look up the tenant: %w", errors.New(secret)),
			wantStatus: http.StatusInternalServerError, wantBody: internalBody, wantLogged: true,
		},
		{name: "nil", err: nil, wantStatus: http.StatusInternalServerError, wantBody: internalBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/clusters/c-1", http.NoBody)
			authz.WriteError(rec, req, tt.err, slog.New(slog.NewTextHandler(&logs, nil)))

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantBody, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), secret)
			if tt.wantBody == "" {
				assert.Empty(t, rec.Header().Get("Content-Type"))
			} else {
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			}
			assert.Equal(t, tt.wantLogged, bytes.Contains(logs.Bytes(), []byte(secret)), "the error is logged only for a 500")
		})
	}
}
