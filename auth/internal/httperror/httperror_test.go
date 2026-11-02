// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httperror_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/auth/internal/httperror"
)

// TestWrite checks the body, content type and status Write sends, and that a
// header set before Write is kept.
func TestWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		status          int
		message         string
		wantBody        string
		wantContentType string
	}{
		{
			name: "a message", status: http.StatusForbidden, message: `Not "authorized"`,
			wantBody: `{"message":"Not \"authorized\""}` + "\n", wantContentType: "application/json",
		},
		{name: "no message", status: http.StatusNotFound, message: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			rec.Header().Set("WWW-Authenticate", "Bearer")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			httperror.Write(rec, req, tt.status, tt.message, nil)

			assert.Equal(t, tt.status, rec.Code)
			assert.Equal(t, tt.wantBody, rec.Body.String())
			assert.Equal(t, tt.wantContentType, rec.Header().Get("Content-Type"))
			assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"), "a header set before Write is kept")
		})
	}
}
