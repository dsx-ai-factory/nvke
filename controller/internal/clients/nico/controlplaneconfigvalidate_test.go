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

func TestClient_ValidateControlPlaneConfigPayload(t *testing.T) {
	tests := []struct {
		name    string
		nico    db.NicoControlPlaneProvider
		wantErr string
	}{
		{
			name: "valid",
			nico: db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{uuid.New()},
				SubnetIDs:              []uuid.UUID{uuid.New()},
			},
		},
		{
			name: "nil instance type ID",
			nico: db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{uuid.Nil},
				SubnetIDs:              []uuid.UUID{uuid.New()},
			},
			wantErr: "invalid nil instance type ID",
		},
		{
			name: "neither subnet nor vpc prefix set",
			nico: db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{uuid.New()},
			},
			wantErr: "exactly one of controlPlaneConfig.nico.subnetIDs or controlPlaneConfig.nico.vpcPrefixIDs must be provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec db.ControlPlaneConfigSpec
			spec.SetNico(tt.nico)

			err := New().ValidateControlPlaneConfigPayload(spec)

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
