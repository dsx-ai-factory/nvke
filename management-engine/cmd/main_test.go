// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestCommandFlags(t *testing.T) {
	// Keep these cases before startup; SetupSignalHandler is process-global.
	t.Setenv("KUBECONFIG", "/missing/config")
	// Help and version must work without a usable kubeconfig.
	require.ErrorIs(t, run([]string{"--help"}), flag.ErrHelp)
	require.NoError(t, run([]string{"--version"}))
	require.NoError(t, run([]string{"--zap-log-level=debug", "--zap-encoder=json", "--version"}))
	err := run([]string{"--zap-log-level=invalid"})
	require.ErrorIs(t, err, errFlagParse)
	require.ErrorContains(t, err, "invalid")
	require.ErrorContains(t, run([]string{"unexpected"}), "positional")
	require.ErrorContains(t, run([]string{"--kubeconfig=/missing/config"}), "load management kubeconfig")
}

func TestCommandKubeconfigFlags(t *testing.T) {
	// controller-runtime's kubeconfig flag is process-global; do not run in parallel.
	t.Cleanup(func() { ctrl.RegisterFlags(flag.NewFlagSet("cleanup", flag.ContinueOnError)) })
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "host-cluster.example.invalid")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"datastore": {Server: "https://datastore.example.invalid"}},
		Contexts:       map[string]*clientcmdapi.Context{"datastore": {Cluster: "datastore"}},
		CurrentContext: "datastore",
	}, path))

	// Parse our command's flags without starting a process, then verify the upstream
	// loader received the explicit datastore path instead of the environment path.
	require.NoError(t, run([]string{"--kubeconfig=" + path, "--version"}))
	cfg, err := ctrl.GetConfig()
	require.NoError(t, err)
	require.Equal(t, "https://datastore.example.invalid", cfg.Host)

	// An invalid override must reach the loader, rather than silently using the
	// valid current context. Loading fails before installing process signal handlers.
	err = run([]string{"--kubeconfig=" + path, "--context=missing"})
	require.ErrorContains(t, err, `context "missing" does not exist`)
}

func TestDeploymentFlags(t *testing.T) {
	t.Setenv("KUBECONFIG", "/missing/config")
	data, err := os.ReadFile("../config/manager/manager.yaml")
	require.NoError(t, err)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var deployment appsv1.Deployment
	for deployment.Kind != "Deployment" {
		require.NoError(t, decoder.Decode(&deployment), "manager manifest must contain a Deployment")
	}
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	container := deployment.Spec.Template.Spec.Containers[0]
	// Parse every deployed flag without contacting an API server.
	require.NoError(t, run(append(container.Args, "--version")))
}

func TestNewDoesNotDiscoverAPIs(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	opts := defaultManagerOptions()
	opts.LeaderElection = false
	opts.Metrics.BindAddress, opts.HealthProbeBindAddress = "0", "0"
	_, err := newManager(&rest.Config{Host: server.URL, Transport: server.Client().Transport}, opts)
	require.NoError(t, err)
	require.Zero(t, requests.Load(), "construction must not require any APIs to be served")
}

func TestStartupRejectsUnsafeTLS(t *testing.T) {
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	tls := httptest.NewTLSServer(handler)
	defer tls.Close()
	for _, tt := range []struct {
		name, host, wantError string
		insecure              bool
	}{
		{name: "plaintext", host: plain.URL, wantError: "management API must use HTTPS"},
		{name: "verification disabled", host: tls.URL, insecure: true, wantError: "TLS certificate verification must be enabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultManagerOptions()
			opts.LeaderElection = false
			_, err := newManager(&rest.Config{
				Host: tt.host, BearerToken: "REDACTED",
				TLSClientConfig: rest.TLSClientConfig{Insecure: tt.insecure},
			}, opts)
			require.ErrorContains(t, err, tt.wantError)
			require.Zero(t, requests.Load(), "unsafe configuration must fail before any request")
		})
	}
}

func TestStartupRequiresElectionNamespace(t *testing.T) {
	_, err := newManager(&rest.Config{Host: "https://unused.example.invalid"}, defaultManagerOptions())
	require.ErrorContains(t, err, "leader-election namespace")
}
