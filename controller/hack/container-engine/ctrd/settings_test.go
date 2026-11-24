// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := settingsFromEnv(func(string) string { return "" })
		assert.Equal(t, "/var/lib/ctrd", cfg.Root)
		assert.Equal(t, "ctrd", cfg.Namespace)
		assert.Equal(t, "/run/containerd/containerd.sock", cfg.Address)
		assert.Equal(t, "unix:///run/buildkit/buildkitd.sock", cfg.BuildkitHost)
		assert.Contains(t, cfg.CNIDirs, "/usr/libexec/cni")
		assert.Empty(t, cfg.DNS)
	})
	t.Run("every setting has its variable", func(t *testing.T) {
		env := map[string]string{
			"CTRD_ROOT": "/r", "CTRD_NAMESPACE": "n", "CONTAINERD_ADDRESS": "/s", "BUILDKIT_HOST": "unix:///b",
			"CNI_PATH": "/a:/b", "CTRD_DNS": "1.1.1.1, 8.8.8.8",
		}
		cfg := settingsFromEnv(func(name string) string { return env[name] })
		assert.Equal(t, "/r", cfg.Root)
		assert.Equal(t, "n", cfg.Namespace)
		assert.Equal(t, "/s", cfg.Address)
		assert.Equal(t, "unix:///b", cfg.BuildkitHost)
		assert.Equal(t, []string{"/a", "/b"}, cfg.CNIDirs)
		assert.Equal(t, []string{"1.1.1.1", "8.8.8.8"}, cfg.DNS)
	})
}

func TestResolvConf(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		return path
	}
	tests := []struct {
		name    string
		cfg     func(t *testing.T) settings
		want    string
		wantErr string
	}{
		{
			name: "the override replaces everything",
			cfg: func(t *testing.T) settings {
				t.Helper()
				return settings{DNS: []string{"1.1.1.1", "8.8.8.8"}, ResolvConfs: []string{write(t, "nameserver 192.0.2.1\n")}}
			},
			want: "nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
		},
		{
			name: "the upstream servers of the host, with its search and options lines, without a loopback stub or IPv6",
			cfg: func(t *testing.T) settings {
				t.Helper()
				return settings{ResolvConfs: []string{write(t, "# comment\nnameserver 127.0.0.53\nnameserver 192.0.2.1\nnameserver fe80::1%eth0\nnameserver 2001:db8::1\nsearch lan example.org\noptions edns0 trust-ad\n")}}
			},
			want: "nameserver 192.0.2.1\nsearch lan example.org\noptions edns0 trust-ad\n",
		},
		{
			name: "the first file with a usable server wins; a stub-only file is skipped",
			cfg: func(t *testing.T) settings {
				t.Helper()
				return settings{ResolvConfs: []string{write(t, "nameserver 127.0.0.53\n"), write(t, "nameserver 192.0.2.7\n")}}
			},
			want: "nameserver 192.0.2.7\n",
		},
		{
			name: "no usable server is an error that names the setting",
			cfg: func(t *testing.T) settings {
				t.Helper()
				return settings{ResolvConfs: []string{write(t, "nameserver 127.0.0.53\n"), filepath.Join(t.TempDir(), "missing")}}
			},
			wantErr: "set CTRD_DNS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg(t).resolvConf()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
