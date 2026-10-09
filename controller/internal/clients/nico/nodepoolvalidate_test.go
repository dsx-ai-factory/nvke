// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ValidateNodePoolPayload(t *testing.T) {
	tests := []struct {
		name    string
		nico    db.NicoNodePoolProvider
		wantErr string
	}{
		{
			name: "valid with subnet",
			nico: db.NicoNodePoolProvider{SubnetIDs: []uuid.UUID{uuid.New()}},
		},
		{
			name: "valid with vpc prefix and disk strategy",
			nico: db.NicoNodePoolProvider{
				VpcPrefixIDs:          []uuid.UUID{uuid.New()},
				DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
			},
		},
		{
			name:    "invalid disk selection strategy",
			nico:    db.NicoNodePoolProvider{SubnetIDs: []uuid.UUID{uuid.New()}, DiskSelectionStrategy: "huge"},
			wantErr: `invalid nico.diskSelectionStrategy: "huge"`,
		},
		{
			name:    "neither subnet nor vpc prefix set",
			nico:    db.NicoNodePoolProvider{},
			wantErr: "exactly one of nico.subnetIDs or nico.vpcPrefixIDs must be provided",
		},
		{
			name: "both subnet and vpc prefix set",
			nico: db.NicoNodePoolProvider{
				SubnetIDs:    []uuid.UUID{uuid.New()},
				VpcPrefixIDs: []uuid.UUID{uuid.New()},
			},
			wantErr: "exactly one of nico.subnetIDs or nico.vpcPrefixIDs must be provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nodePool db.NodePool
			nodePool.SetNico(tt.nico)

			err := New().ValidateNodePoolPayload(nodePool)

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestClient_ValidateNodePoolPayload_MissingNicoPayload(t *testing.T) {
	var nodePool db.NodePool
	nodePool.SetCapi(db.CAPINodePoolProvider{})

	err := New().ValidateNodePoolPayload(nodePool)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrMissingProviderPayload)
}
