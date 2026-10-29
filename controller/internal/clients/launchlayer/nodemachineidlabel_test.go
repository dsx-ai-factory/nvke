// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_NodeMachineIDLabelValue(t *testing.T) {
	nodeWithMachineID := func(machineID string) db.Node {
		node := db.Node{}
		node.SetLaunchLayer(db.LaunchLayerNodeProvider{InstanceID: "instance-1", MachineID: machineID})
		return node
	}

	tests := map[string]struct {
		node      db.Node
		wantValue string
		wantErr   error
	}{
		"returns the physical machine ID":           {node: nodeWithMachineID("machine-1"), wantValue: "machine-1"},
		"returns empty for a virtual machine":       {node: nodeWithMachineID("")},
		"errors without a node launchLayer payload": {node: db.Node{}, wantErr: db.ErrMissingProviderPayload},
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
