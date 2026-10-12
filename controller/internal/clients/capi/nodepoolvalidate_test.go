// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ValidateNodePoolPayload(t *testing.T) {
	tests := []struct {
		name    string
		raw     json.RawMessage
		wantErr string
	}{
		{
			name: "empty manifest is valid",
			raw:  nil,
		},
		{
			name: "valid manifest",
			raw:  json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate"}`),
		},
		{
			name:    "missing apiVersion",
			raw:     json.RawMessage(`{"kind":"DockerMachineTemplate"}`),
			wantErr: "apiVersion is required",
		},
		{
			name:    "wrong group",
			raw:     json.RawMessage(`{"apiVersion":"example.com/v1","kind":"Foo"}`),
			wantErr: "must be a CAPI infrastructure provider group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nodePool db.NodePool
			nodePool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: tt.raw})

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

func TestClient_ValidateNodePoolPayload_MissingCAPIPayload(t *testing.T) {
	var nodePool db.NodePool
	nodePool.SetNico(db.NicoNodePoolProvider{})

	err := New().ValidateNodePoolPayload(nodePool)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrMissingProviderPayload)
}
