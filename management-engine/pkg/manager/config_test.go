// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/dsx-ai-factory/nke/management-engine/pkg/manager"
)

func TestLoadConfigSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	raw := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"first":  {Server: "https://first.example.invalid", TLSServerName: "first.example.invalid", CertificateAuthorityData: []byte("REDACTED")},
			"second": {Server: "https://second.example.invalid"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"first":  {Cluster: "first", AuthInfo: "exec"},
			"second": {Cluster: "second"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"exec": {Exec: &clientcmdapi.ExecConfig{
				Command: "example-auth-helper", APIVersion: "client.authentication.k8s.io/v1",
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			}},
		},
		CurrentContext: "first",
	}
	require.NoError(t, clientcmd.WriteToFile(raw, path))
	otherPath := filepath.Join(t.TempDir(), "config")
	raw.CurrentContext = "second"
	require.NoError(t, clientcmd.WriteToFile(raw, otherPath))
	missingPath := filepath.Join(t.TempDir(), "missing")
	separator := string(os.PathListSeparator)
	for _, tt := range []struct {
		name      string
		opts      manager.ConfigOptions
		env       string
		want      string
		wantError string
	}{
		{name: "explicit path beats environment", opts: manager.ConfigOptions{Kubeconfig: path}, env: "missing", want: "first"},
		{name: "environment selects kubeconfig", env: path, want: "first"},
		{name: "environment merges existing files in order", env: path + separator + otherPath, want: "first"},
		{name: "missing first environment file cannot fall back", env: missingPath + separator + path, wantError: "load management kubeconfig"},
		{name: "missing last environment file is rejected", env: path + separator + missingPath, wantError: "load management kubeconfig"},
		{name: "explicit context overrides current context", opts: manager.ConfigOptions{Context: "second"}, env: path, want: "second"},
		{name: "explicit path and context", opts: manager.ConfigOptions{Kubeconfig: path, Context: "second"}, want: "second"},
		{name: "bad explicit path cannot fall back", opts: manager.ConfigOptions{Kubeconfig: filepath.Join(t.TempDir(), "missing")}, env: path, wantError: "load management kubeconfig"},
		{name: "bad context cannot fall back", opts: manager.ConfigOptions{Context: "missing"}, env: path, wantError: "select management kubeconfig context"},
		{name: "empty environment config cannot fall back", env: missingPath, wantError: "load management kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tt.env)
			// Set cluster coordinates to catch accidental in-cluster selection.
			t.Setenv("KUBERNETES_SERVICE_HOST", "in-cluster.example.invalid")
			t.Setenv("KUBERNETES_SERVICE_PORT", "443")
			cfg, err := manager.LoadConfig(tt.opts)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, float32(-1), cfg.QPS)
			require.Equal(t, "https://"+tt.want+".example.invalid", cfg.Host)
			if tt.want == "first" {
				require.Equal(t, "first.example.invalid", cfg.ServerName)
				require.Equal(t, []byte("REDACTED"), cfg.CAData)
				require.NotNil(t, cfg.ExecProvider)
				require.Equal(t, "example-auth-helper", cfg.ExecProvider.Command)
				require.False(t, cfg.Insecure)
			}
		})
	}
}

func TestDefaultAndInClusterSelection(t *testing.T) {
	// Override client-go's global default path to avoid reading the user's kubeconfig.
	// This test must not run in parallel.
	previous := clientcmd.RecommendedHomeFile
	clientcmd.RecommendedHomeFile = filepath.Join(t.TempDir(), "config")
	t.Cleanup(func() { clientcmd.RecommendedHomeFile = previous })
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"local": {Server: "https://local.example.invalid"}},
		Contexts:       map[string]*clientcmdapi.Context{"local": {Cluster: "local"}},
		CurrentContext: "local",
	}, clientcmd.RecommendedHomeFile))
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "in-cluster.example.invalid")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Run("outside a cluster uses default kubeconfig", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")
		cfg, err := manager.LoadConfig(manager.ConfigOptions{})
		require.NoError(t, err)
		require.Equal(t, "https://local.example.invalid", cfg.Host)
		require.Equal(t, float32(-1), cfg.QPS)
	})
	t.Run("in-cluster credentials do not fall back on failure", func(t *testing.T) {
		_, inClusterErr := rest.InClusterConfig()
		cfg, err := manager.LoadConfig(manager.ConfigOptions{})
		if inClusterErr != nil {
			// Missing ServiceAccount credentials must not trigger kubeconfig fallback.
			require.ErrorContains(t, err, "load in-cluster credentials")
		} else {
			require.NoError(t, err)
			require.Equal(t, "https://in-cluster.example.invalid:443", cfg.Host)
			require.Equal(t, float32(-1), cfg.QPS)
		}
	})
	t.Run("explicit context selects default kubeconfig inside a cluster", func(t *testing.T) {
		cfg, err := manager.LoadConfig(manager.ConfigOptions{Context: "local"})
		require.NoError(t, err)
		require.Equal(t, "https://local.example.invalid", cfg.Host)
	})
}
