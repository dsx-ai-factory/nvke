// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ValidateNodePoolPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload db.LaunchLayerNodePoolProvider
		wantErr string
	}{
		{
			name:    "valid with hostOSProfile",
			payload: db.LaunchLayerNodePoolProvider{InstanceType: "vm200", HostOSProfile: "ubuntu-24.04"},
		},
		{
			name:    "valid with vmProfile",
			payload: db.LaunchLayerNodePoolProvider{InstanceType: "vm200", VMProfile: "ubuntu-24.04"},
		},
		{
			name:    "valid with neither profile",
			payload: db.LaunchLayerNodePoolProvider{InstanceType: "vm200"},
		},
		{
			name:    "missing instanceType",
			payload: db.LaunchLayerNodePoolProvider{HostOSProfile: "ubuntu-24.04"},
			wantErr: "instanceType is required",
		},
		{
			name:    "both hostOSProfile and vmProfile",
			payload: db.LaunchLayerNodePoolProvider{InstanceType: "vm200", HostOSProfile: "ubuntu-24.04", VMProfile: "ubuntu-24.04"},
			wantErr: "at most one of hostOSProfile or vmProfile may be provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nodePool db.NodePool
			nodePool.SetLaunchLayer(tt.payload)

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

func TestClient_ValidateNodePoolPayload_MissingLaunchLayerPayload(t *testing.T) {
	var nodePool db.NodePool
	nodePool.SetNico(db.NicoNodePoolProvider{})

	err := New().ValidateNodePoolPayload(nodePool)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrMissingProviderPayload)
}
