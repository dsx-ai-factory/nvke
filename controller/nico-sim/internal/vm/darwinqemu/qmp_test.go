// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newPipeQMPClient returns a client wired to an in-memory server end. The
// server goroutine reads one command line, then writes each chunk in its own
// Write call, so tests control exactly how bytes are framed on the wire.
func newPipeQMPClient(t *testing.T, chunks ...string) *QMPClient {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	go func() {
		if _, err := bufio.NewReader(server).ReadString('\n'); err != nil {
			return
		}
		for _, chunk := range chunks {
			if _, err := server.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}()
	return &QMPClient{conn: client, dec: json.NewDecoder(client)}
}

func TestQMPSendCommandFraming(t *testing.T) {
	tests := []struct {
		name       string
		chunks     []string
		wantReturn string
		wantErr    string
	}{
		{
			name:       "single object",
			chunks:     []string{`{"return":{"status":"running"}}` + "\n"},
			wantReturn: `{"status":"running"}`,
		},
		{
			name:       "event and return joined in one write",
			chunks:     []string{`{"event":"RESET"}` + "\n" + `{"return":{"ok":true}}` + "\n"},
			wantReturn: `{"ok":true}`,
		},
		{
			name:       "return split across two writes",
			chunks:     []string{`{"return":{"sta`, `tus":"running"}}` + "\n"},
			wantReturn: `{"status":"running"}`,
		},
		{
			name:    "error response joined after event",
			chunks:  []string{`{"event":"RESET"}{"error":{"class":"GenericError","desc":"boom"}}` + "\n"},
			wantErr: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPipeQMPClient(t, tt.chunks...)
			got, err := c.sendCommand("query-status")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, tt.wantReturn, string(got))
		})
	}
}

func TestQMPGracefulShutdownFraming(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{
			name:   "return and SHUTDOWN event joined in one write",
			chunks: []string{`{"return":{}}` + "\n" + `{"event":"SHUTDOWN","data":{"guest":true}}` + "\n"},
		},
		{
			name:   "SHUTDOWN event split across two writes",
			chunks: []string{`{"return":{}}` + "\n" + `{"event":"SHUT`, `DOWN","data":{"guest":true}}` + "\n"},
		},
		{
			name:   "unrelated event before SHUTDOWN in the same write",
			chunks: []string{`{"return":{}}{"event":"POWERDOWN"}{"event":"SHUTDOWN"}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPipeQMPClient(t, tt.chunks...)
			require.NoError(t, c.GracefulShutdown(2*time.Second))
		})
	}
}

func TestQMPReadMessageTimeout(t *testing.T) {
	c := newPipeQMPClient(t)
	_, err := c.readMessage(50 * time.Millisecond)
	require.Error(t, err)
}
