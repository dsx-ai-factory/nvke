// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmdistro_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchRetriesConnectionResets(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "stat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	if err := exec.Command("stat", "--version").Run(); err != nil {
		t.Skip("bundle fetch requires GNU stat")
	}
	data, err := os.ReadFile(bundleScript)
	require.NoError(t, err)
	// Load the script's functions without starting the bundle CLI.
	functions, _, found := strings.Cut(string(data), "\nWRITE_LOCK=0\n")
	require.True(t, found)
	for _, tc := range []struct {
		name     string
		resets   int64
		notFound bool
		requests int64
		wantErr  bool
	}{
		{name: "two resets then success", resets: 2, requests: 3},
		{name: "persistent resets stop after three attempts", resets: 3, requests: 3, wantErr: true},
		{name: "404 fails on the first attempt", notFound: true, requests: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := requests.Add(1)
				if attempt <= tc.resets {
					hijacker, ok := w.(http.Hijacker)
					if !ok {
						t.Error("fixture server does not support connection hijacking")
						return
					}
					conn, _, hijackErr := hijacker.Hijack()
					if hijackErr != nil {
						t.Error(hijackErr)
						return
					}
					// Send a TCP reset instead of a normal end of response.
					if tcp, ok := conn.(*net.TCPConn); ok {
						assert.NoError(t, tcp.SetLinger(0))
					}
					assert.NoError(t, conn.Close())
					return
				}
				if tc.notFound {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte("verified fixture bytes"))
			}))
			defer server.Close()
			dir := t.TempDir()
			script := filepath.Join(dir, "functions.sh")
			destination := filepath.Join(dir, "artifact")
			require.NoError(t, os.WriteFile(script, []byte(functions), 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", `source "$1"; fetch "$2" "$3"`, "bash", script, server.URL, destination)
			out, runErr := cmd.CombinedOutput()
			if tc.wantErr {
				require.Error(t, runErr, string(out))
				assert.Contains(t, string(out), "download "+server.URL+" failed")
			} else {
				require.NoError(t, runErr, string(out))
				body, readErr := os.ReadFile(destination)
				require.NoError(t, readErr)
				assert.Equal(t, "verified fixture bytes", string(body))
			}
			assert.Equal(t, tc.requests, requests.Load())
		})
	}
}

func TestImageDigestRetriesRegistryErrors(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	data, err := os.ReadFile(bundleScript)
	require.NoError(t, err)
	functions, _, found := strings.Cut(string(data), "\nWRITE_LOCK=0\n")
	require.True(t, found)
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name     string
		failures int
		calls    string
		wantErr  bool
	}{
		{name: "two timeouts then success", failures: 2, calls: "3"},
		{name: "persistent timeouts stop after three attempts", failures: 3, calls: "3", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "functions.sh")
			require.NoError(t, os.WriteFile(script, []byte(functions), 0o600))
			counter := filepath.Join(dir, "calls")
			// The fake crane fails the first FAILURES digest calls the way a registry timeout does.
			crane := filepath.Join(dir, "crane")
			require.NoError(t, os.WriteFile(crane, []byte(`#!/usr/bin/env bash
n=$(( $(cat "$COUNTER" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$COUNTER"
if (( n <= FAILURES )); then
  echo 'Error: Get "https://registry.k8s.io/v2/": dial tcp 34.96.108.209:443: i/o timeout' >&2
  exit 1
fi
echo `+digest+`
`), 0o700))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", `source "$1"; CRANE="$2" ARCH=amd64 image_digest registry.k8s.io/kube-apiserver:v1.36.3`, "bash", script, crane)
			cmd.Env = append(os.Environ(), "COUNTER="+counter, "FAILURES="+strconv.Itoa(tc.failures))
			out, runErr := cmd.CombinedOutput()
			if tc.wantErr {
				require.Error(t, runErr, string(out))
			} else {
				require.NoError(t, runErr, string(out))
				assert.Contains(t, string(out), digest)
			}
			calls, readErr := os.ReadFile(counter)
			require.NoError(t, readErr)
			assert.Equal(t, tc.calls, strings.TrimSpace(string(calls)))
		})
	}
}
