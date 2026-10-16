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
	cluster := db.Cluster{ID: uuid.New()}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: uuid.New(), VpcID: uuid.New()})
	nodeWithMachineID := func(machineID string) db.Node {
		node := db.Node{ID: uuid.New()}
		node.SetNico(db.NicoNodeProvider{ForgeMachineID: machineID})
		return node
	}

	type parameters struct {
		cluster   db.Cluster
		node      db.Node
		wantValue string
		wantErr   error
	}

	tests := map[string]parameters{
		"returns the forge machine ID": {
			cluster:   cluster,
			node:      nodeWithMachineID("machine-abc123"),
			wantValue: "machine-abc123",
		},
		"returns empty when the machine is not yet allocated": {
			cluster: cluster,
			node:    nodeWithMachineID(""),
		},
		"errors without a cluster nico payload": {
			cluster: db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeNico},
			node:    nodeWithMachineID("machine-abc123"),
			wantErr: db.ErrMissingProviderPayload,
		},
		"errors without a node nico payload": {
			cluster: cluster,
			node:    db.Node{ID: uuid.New()},
			wantErr: db.ErrMissingProviderPayload,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			value, err := New().NodeMachineIDLabelValue(params.cluster, params.node)
			if params.wantErr != nil {
				require.ErrorIs(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, params.wantValue, value)
		})
	}
}
