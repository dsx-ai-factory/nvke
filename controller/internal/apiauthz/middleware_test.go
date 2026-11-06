// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/apiauthz"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// listClusters serves GET /api/clusters through the real server, as role,
// with authorization in mode, or off if mode is empty. It returns the
// response and whether the request reached the database.
func listClusters(t *testing.T, mode authn.Mode, role string) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	var opts []server.Option
	if mode != "" {
		opts = append(opts, server.WithAuthorization(apiauthz.Middleware(newClient(t, mode), logger)))
	}
	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{{Name: "c1"}}, nil).Maybe()
	srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"}, logger, mockDB, nil, nil, nil, opts...)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(caller(role), http.MethodGet, "/api/clusters", nil)
	srv.Handler.ServeHTTP(w, r)
	return w, len(mockDB.Calls) > 0
}

// Enforcing authorization refuses a caller with no role with the library's
// 403, before the handler. Permissive authorization and authorization off
// answer as the handler does.
func TestMiddleware(t *testing.T) {
	t.Parallel()

	off, _ := listClusters(t, "", "")
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())

	tests := []struct {
		name string
		mode authn.Mode
		role string
		want int
	}{
		{name: "enforcing, viewer", mode: authn.ModeEnforcing, role: "viewer", want: http.StatusOK},
		{name: "enforcing, no role", mode: authn.ModeEnforcing, want: http.StatusForbidden},
		{name: "permissive, no role", mode: authn.ModePermissive, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, handled := listClusters(t, tt.mode, tt.role)
			require.Equal(t, tt.want, w.Code)
			if tt.want == http.StatusForbidden {
				assert.False(t, handled, "a refused request reached the handler")
				assert.JSONEq(t, `{"message": "Not authorized"}`, w.Body.String())
				return
			}
			assert.Equal(t, off.Body.String(), w.Body.String())
		})
	}
}
