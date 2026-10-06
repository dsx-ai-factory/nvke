// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPOutcome(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusOK, "succeeded"},
		{http.StatusCreated, "succeeded"},
		{http.StatusNotModified, "succeeded"},
		{http.StatusBadRequest, "rejected"},
		{http.StatusNotFound, "rejected"},
		{http.StatusConflict, "rejected"},
		{http.StatusInternalServerError, "failed"},
		{http.StatusBadGateway, "failed"},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			require.Equal(t, tt.want, httpOutcome(tt.status))
		})
	}
}
