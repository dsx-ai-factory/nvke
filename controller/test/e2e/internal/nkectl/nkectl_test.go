// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nkectl

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requireClosed fails unless ch closes promptly. done closing is what tells
// ensureForward the forward is gone, so a path that leaves it open is the bug
// these tests exist to catch.
func requireClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("done was never closed")
	}
}

func TestAwaitForwardPort(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		wantPort string
		wantErr  bool
	}{
		{
			name:     "reports the bound port",
			stdout:   "Forwarding from 127.0.0.1:54321 -> 8080\n",
			wantPort: "54321",
		},
		{
			// kubectl keeps logging after the line this waits for, so the
			// goroutine has to go on reading or the pipe fills and blocks the
			// forward it is meant to be serving.
			name:     "drains the output that follows",
			stdout:   "Forwarding from 127.0.0.1:1234 -> 8080\nHandling connection for 1234\nHandling connection for 1234\n",
			wantPort: "1234",
		},
		{
			name:    "errors when the port is never announced",
			stdout:  "error: unable to listen on any of the requested ports\n",
			wantErr: true,
		},
		{
			name:    "errors on empty output",
			stdout:  "",
			wantErr: true,
		},
		{
			// An IPv6 or non-loopback bind is not the local listener this
			// expects, so it must not be read as one.
			name:    "ignores a forward on another address",
			stdout:  "Forwarding from [::1]:54321 -> 8080\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{})
			port, err := awaitForwardPort(io.NopCloser(strings.NewReader(tt.stdout)), done)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantPort, port)
			}
			requireClosed(t, done)
		})
	}
}

// A reader that blocks forever stands in for kubectl starting but never
// printing the line. The timeout has to fire, and done must still close once
// the pipe is shut, which is what startForward waits on before reaping.
func TestAwaitForwardPortClosesDoneAfterTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	errCh := make(chan error, 1)

	// The error comes back over a channel rather than being asserted in here:
	// require calls FailNow, which is only defined on the test's own goroutine.
	go func() { _, err := awaitForwardPort(reader, done); errCh <- err }()

	// The forward is never announced, so nothing should arrive on done while
	// the pipe is open.
	select {
	case <-done:
		t.Fatal("done closed while the pipe was still open")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, writer.Close())
	requireClosed(t, done)
	require.Error(t, <-errCh)
}

func TestResolveBinary(t *testing.T) {
	t.Run("rejects a path that does not exist", func(t *testing.T) {
		_, err := resolveBinary(t.TempDir() + "/absent-nkectl")
		require.Error(t, err)
	})

	t.Run("reports a missing binary as ErrBinaryNotFound", func(t *testing.T) {
		// An empty PATH and no override leaves nothing to find. Callers branch
		// on this to skip rather than fail, so the sentinel has to survive the
		// wrapping resolveBinary does.
		t.Setenv("PATH", t.TempDir())
		t.Setenv(BinaryEnv, "")

		_, err := resolveBinary("")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrBinaryNotFound)
	})
}
