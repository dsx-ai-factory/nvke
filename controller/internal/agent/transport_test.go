// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// A node with no IP address has nothing to dial: Dial returns ErrNoAddress for
// every authentication profile, before any credential setup runs.
func TestUnit_DialRejectsNodeWithNoIP(t *testing.T) {
	t.Parallel()
	tests := map[string]db.Node{
		"insecure node":      {},
		"SPIFFE node":        {SpiffeEnabled: true},
		"external mTLS node": {NodeAgentExternalMTLSEnabled: true},
	}
	for name, node := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport := &Transport{cfg: config.Config{
				NodeAgent: config.NodeAgentConfig{Port: "15005"},
			}}
			conn, cleanup, err := transport.Dial(t.Context(), node)
			require.Nil(t, conn)
			require.Nil(t, cleanup)
			require.ErrorIs(t, err, ErrNoAddress)
			require.Equal(t, ProbeCauseNoAddress, CauseOf(err))
		})
	}
}
