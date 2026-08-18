// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAsForgeAPIError(t *testing.T) {
	baseErr := errors.New("boom")

	tests := []struct {
		name    string
		status  int
		body    string
		nilResp bool
		wantIs  error
	}{
		{
			name:   "forbidden maps to tenant not authorized",
			status: http.StatusForbidden,
			body:   `{"source":"forge","message":"Forbidden"}`,
			wantIs: ErrTenantNotAuthorized,
		},
		{
			name:   "not found maps to instance not found",
			status: http.StatusNotFound,
			body:   `{"message":"Could not find Instance"}`,
			wantIs: ErrInstanceNotFound,
		},
		{
			name:   "already exists is detected from body",
			status: http.StatusConflict,
			body:   `{"message":"An Instance with specified name already exists"}`,
			wantIs: ErrInstanceAlreadyExists,
		},
		{
			name:   "unmapped status falls through to original error",
			status: http.StatusInternalServerError,
			body:   `{"message":"kaboom"}`,
			wantIs: baseErr,
		},
		{
			name:    "nil response falls through to original error",
			nilResp: true,
			wantIs:  baseErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res *http.Response
			if !tt.nilResp {
				// asForgeAPIError reads and closes the body itself.
				res = &http.Response{
					StatusCode: tt.status,
					Body:       io.NopCloser(strings.NewReader(tt.body)),
				}
			}

			got := asForgeAPIError(res, baseErr)
			if !errors.Is(got, tt.wantIs) {
				t.Fatalf("asForgeAPIError() = %v, want errors.Is %v", got, tt.wantIs)
			}
		})
	}
}
