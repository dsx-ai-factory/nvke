// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowSourceSubnet(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.168.64.0/24")
	require.NoError(t, err)
	handler := allowSourceSubnet(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), subnet)

	tests := []struct {
		name       string
		remoteAddr string
		want       int
	}{
		{name: "guest in the vmnet subnet", remoteAddr: "192.168.64.52:40000", want: http.StatusOK},
		{name: "host on another interface", remoteAddr: "10.0.0.5:40000", want: http.StatusForbidden},
		{name: "loopback", remoteAddr: "127.0.0.1:40000", want: http.StatusForbidden},
		{name: "unparseable remote address", remoteAddr: "garbage", want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/", nil)
			req.RemoteAddr = tt.remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, tt.want, rec.Code)
		})
	}
}
