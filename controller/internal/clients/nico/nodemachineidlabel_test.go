// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_NodeMachineIDLabelValue(t *testing.T) {
	nodeWithMachineID := func(machineID string) db.Node {
		node := db.Node{ID: uuid.New()}
		node.SetNico(db.NicoNodeProvider{ForgeMachineID: machineID})
		return node
	}

	type parameters struct {
		node      db.Node
		wantValue string
		wantErr   error
	}

	tests := map[string]parameters{
		"returns the forge machine ID": {
			node:      nodeWithMachineID("machine-abc123"),
			wantValue: "machine-abc123",
		},
		"returns empty when the machine is not yet allocated": {
			node: nodeWithMachineID(""),
		},
		"errors without a node nico payload": {
			node:    db.Node{ID: uuid.New()},
			wantErr: db.ErrMissingProviderPayload,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			value, err := New().NodeMachineIDLabelValue(params.node)
			if params.wantErr != nil {
				require.ErrorIs(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, params.wantValue, value)
		})
	}
}
