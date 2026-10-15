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
