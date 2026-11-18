// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestUnit_NodeAgentConditionNormalizesProbeError(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "IPv4 read reset",
			message: "error reading server preface: read tcp 192.0.2.10:41234->198.51.100.20:15005: read: connection reset by peer",
			want:    "error reading server preface: read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "same reset with a different local port",
			message: "error reading server preface: read tcp 192.0.2.10:41235->198.51.100.20:15005: read: connection reset by peer",
			want:    "error reading server preface: read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "TLS handshake reset",
			message: "transport: authentication handshake failed: read tcp 192.0.2.10:41235->198.51.100.20:15005: read: connection reset by peer",
			want:    "transport: authentication handshake failed: read tcp ->198.51.100.20:15005: read: connection reset by peer",
		},
		{
			name:    "IPv4 write",
			message: "write tcp 192.0.2.10:41236->198.51.100.20:15005: write: broken pipe",
			want:    "write tcp ->198.51.100.20:15005: write: broken pipe",
		},
		{
			name:    "bracketed IPv6 write",
			message: "write tcp [2001:db8::10]:41236->[2001:db8::20]:15005: write: broken pipe",
			want:    "write tcp ->[2001:db8::20]:15005: write: broken pipe",
		},
		{
			name:    "bracketed IPv6 read with a zone",
			message: "read tcp [fe80::10%eth0]:41234->[fe80::20%eth0]:15005: read: connection reset by peer",
			want:    "read tcp ->[fe80::20%eth0]:15005: read: connection reset by peer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			condition := nodeAgentCondition(db.Condition{}, nodeAgentProbe{err: errors.New(tc.message)})
			require.Equal(t, db.ConditionStatusFalse, condition.Status)
			require.Equal(t, tc.want, condition.Message)
		})
	}
}

func TestUnit_NodeAgentConditionPreservesStableProbeErrors(t *testing.T) {
	for _, message := range []string{
		"dial tcp 198.51.100.20:15005: connect: connection refused",
		"dial tcp 198.51.100.20:15005: i/o timeout",
		"context deadline exceeded while waiting for connections to become ready",
		"remote error: tls: certificate required",
		"error reading server preface: EOF",
		"read tcp 198.51.100.20:15005: i/o timeout",
		"read tcp ->198.51.100.20:15005: read: connection reset by peer",
	} {
		t.Run(message, func(t *testing.T) {
			condition := nodeAgentCondition(db.Condition{}, nodeAgentProbe{err: errors.New(message)})
			require.Equal(t, message, condition.Message)
		})
	}
}
