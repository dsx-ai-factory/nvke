// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nke/management-engine/pkg/manager"
)

func TestNewDoesNotDiscoverAPIs(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	opts := manager.DefaultOptions()
	opts.LeaderElection = false
	opts.MetricsBindAddress, opts.HealthProbeBindAddress = "0", "0"
	_, err := manager.New(&rest.Config{Host: server.URL, Transport: server.Client().Transport}, opts)
	require.NoError(t, err)
	require.Zero(t, requests.Load(), "construction must not require any APIs to be served")
}

func TestClientRejectsUntrustedTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	opts := manager.DefaultOptions()
	opts.LeaderElection = false
	opts.MetricsBindAddress, opts.HealthProbeBindAddress = "0", "0"
	mgr, err := manager.New(&rest.Config{Host: server.URL}, opts)
	require.NoError(t, err)
	response, err := mgr.GetHTTPClient().Get(server.URL)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorContains(t, err, "certificate")
}

func TestStartupRejectsPlaintextBeforeSendingCredentials(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	opts := manager.DefaultOptions()
	opts.LeaderElection = false
	_, err := manager.New(&rest.Config{Host: server.URL, BearerToken: "REDACTED"}, opts)
	require.ErrorContains(t, err, "management API must use HTTPS")
	require.Zero(t, requests.Load(), "plaintext configuration must fail before any request")
}

func TestStartupRequiresElectionNamespace(t *testing.T) {
	_, err := manager.New(&rest.Config{Host: "https://unused.example.invalid"}, manager.DefaultOptions())
	require.ErrorContains(t, err, "leader-election namespace")
}
