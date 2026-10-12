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

func TestClient_ValidateControlPlaneConfigPayload(t *testing.T) {
	tests := []struct {
		name    string
		prefs   []json.RawMessage
		wantErr string
	}{
		{
			name: "no preferences is valid",
		},
		{
			name: "valid first preference",
			prefs: []json.RawMessage{
				json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate"}`),
			},
		},
		{
			name: "ignores invalid trailing merge-history entries",
			prefs: []json.RawMessage{
				json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate"}`),
				json.RawMessage(`{"kind":"Stale"}`),
			},
		},
		{
			name: "invalid first preference",
			prefs: []json.RawMessage{
				json.RawMessage(`{"kind":"DockerMachineTemplate"}`),
			},
			wantErr: "apiVersion is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec db.ControlPlaneConfigSpec
			spec.SetCapi(db.CAPIControlPlaneProvider{InfraMachineTemplatePreferences: tt.prefs})

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
