// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/dsx-ai-factory/nke/management-engine/pkg/manager"
)

func TestInsecureConfigCannotStart(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	for _, source := range []string{"caller", "kubeconfig"} {
		t.Run(source, func(t *testing.T) {
			cfg := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
			if source == "kubeconfig" {
				path := filepath.Join(t.TempDir(), "config")
				require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
					Clusters:       map[string]*clientcmdapi.Cluster{"test": {Server: server.URL, InsecureSkipTLSVerify: true}},
					Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test"}},
					CurrentContext: "test",
				}, path))
				var err error
				cfg, err = manager.LoadConfig(manager.ConfigOptions{Kubeconfig: path})
				require.NoError(t, err)
			}
			opts := manager.DefaultOptions()
			opts.LeaderElection = false
			_, err := manager.New(cfg, opts)
			require.ErrorContains(t, err, "TLS certificate verification must be enabled")
			require.Zero(t, requests.Load(), "insecure configuration must fail before any request")
		})
	}
}

func TestRedirectsStayOnAPIOrigin(t *testing.T) {
	var escaped atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		escaped.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	httpTarget := httptest.NewServer(handler)
	defer httpTarget.Close()
	httpsTarget := httptest.NewTLSServer(handler)
	defer httpsTarget.Close()
	for _, target := range []struct {
		name string
		url  string
	}{
		{name: "same origin"},
		{name: "HTTP downgrade", url: httpTarget.URL},
		{name: "different HTTPS origin", url: httpsTarget.URL},
	} {
		t.Run(target.name, func(t *testing.T) {
			var redirects atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/redirected/") {
					redirects.Add(1)
					http.Redirect(w, r, target.url+"/redirected"+r.URL.Path, http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg := &rest.Config{Host: server.URL, Transport: server.Client().Transport, BearerToken: "REDACTED"}
			opts := manager.DefaultOptions()
			opts.LeaderElection = false
			opts.MetricsBindAddress, opts.HealthProbeBindAddress = "0", "0"
			mgr, err := manager.New(cfg, opts)
			require.NoError(t, err)
			require.Nil(t, cfg.WrapTransport, "construction must not change the caller's transport configuration")
			response, err := mgr.GetHTTPClient().Get(server.URL + "/probe")
			if response != nil {
				require.NoError(t, response.Body.Close())
			}
			if target.url == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "request must stay on its HTTPS origin")
			}
			require.Positive(t, redirects.Load())
			require.Zero(t, escaped.Load(), "redirect destination must never receive a request")
		})
	}
}
