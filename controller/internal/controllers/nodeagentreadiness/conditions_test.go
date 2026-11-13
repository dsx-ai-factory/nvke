// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_NormalizeProbeErrorMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "read error after connect drops the local address",
			message: "error reading server preface: read tcp 192.0.2.10:41234->198.51.100.20:15005: read: connection reset by peer",
			want:    "error reading server preface: read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "handshake read error drops the local address",
			message: "transport: authentication handshake failed: read tcp 192.0.2.10:41235->198.51.100.20:15005: read: connection reset by peer",
			want:    "transport: authentication handshake failed: read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "write error drops the local address",
			message: "transport: authentication handshake failed: write tcp 192.0.2.10:41236->198.51.100.20:15005: write: broken pipe",
			want:    "transport: authentication handshake failed: write tcp ->198.51.100.20:15005: write: broken pipe",
		},
		{
			name:    "bracketed IPv6 local address is dropped",
			message: "read tcp [fd00::5]:41234->198.51.100.20:15005: read: connection reset by peer",
			want:    "read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "bracketed IPv6 remote address is kept",
			message: "read tcp 192.0.2.10:41234->[fd00::9]:15005: read: connection reset by peer",
			want:    "read tcp ->[fd00::9]:15005: read: connection reset by peer",
		},
		{
			name:    "connection refused has no local address",
			message: "dial tcp 198.51.100.20:15005: connect: connection refused",
			want:    "dial tcp 198.51.100.20:15005: connect: connection refused",
		},
		{
			name:    "dial timeout has no local address",
			message: "dial tcp 198.51.100.20:15005: i/o timeout",
			want:    "dial tcp 198.51.100.20:15005: i/o timeout",
		},
		{
			name:    "deadline waiting on connections is stable",
			message: "context deadline exceeded while waiting for connections to become ready",
			want:    "context deadline exceeded while waiting for connections to become ready",
		},
		{
			name:    "certificate rejection is stable",
			message: "remote error: tls: certificate required",
			want:    "remote error: tls: certificate required",
		},
		{
			name:    "clean close EOF is stable",
			message: "error reading server preface: EOF",
			want:    "error reading server preface: EOF",
		},
		{
			name:    "message without a tcp address pair is unchanged",
			message: "connection reset by peer",
			want:    "connection reset by peer",
		},
		{
			name:    "empty message is unchanged",
			message: "",
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, normalizeProbeErrorMessage(tc.message))
		})
	}
}

func TestUnit_NormalizeProbeErrorMessageStableAcrossPorts(t *testing.T) {
	first := normalizeProbeErrorMessage("read tcp 192.0.2.10:40001->198.51.100.20:15005: read: connection reset by peer")
	second := normalizeProbeErrorMessage("read tcp 192.0.2.10:40002->198.51.100.20:15005: read: connection reset by peer")
	require.Equal(t, first, second)
}
