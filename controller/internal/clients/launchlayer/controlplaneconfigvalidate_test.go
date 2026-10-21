// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ValidateControlPlaneConfigPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload db.LaunchLayerControlPlaneProvider
		wantErr string
	}{
		{
			name:    "valid",
			payload: db.LaunchLayerControlPlaneProvider{InstanceType: "vm400", HostOSProfile: "ubuntu-24.04"},
		},
		{
			name:    "missing instanceType",
			payload: db.LaunchLayerControlPlaneProvider{},
			wantErr: "controlPlaneConfig.instanceType is required",
		},
		{
			name:    "both hostOSProfile and vmProfile",
			payload: db.LaunchLayerControlPlaneProvider{InstanceType: "vm400", HostOSProfile: "a", VMProfile: "b"},
			wantErr: "at most one of controlPlaneConfig.hostOSProfile or controlPlaneConfig.vmProfile may be provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec db.ControlPlaneConfigSpec
			spec.SetLaunchLayer(tt.payload)

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
