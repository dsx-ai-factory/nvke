// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestKubeadmInitRendersControlPlaneExtraArgs(t *testing.T) {
	pinned := db.ControlPlaneConfigSpec{SchedulerExtraArgs: map[string]string{"feature-gates": "SchedulerAsyncAPICalls=true"}}
	current := db.ControlPlaneConfigSpec{SchedulerExtraArgs: map[string]string{"feature-gates": "SELinuxMount=true"}}

	tests := []struct {
		name     string
		pinnedID uuid.UUID
		want     string
		notWant  string
	}{
		{
			name:     "from the snapshot the node is pinned to",
			pinnedID: uuid.New(),
			want:     "SchedulerAsyncAPICalls=true",
			notWant:  "SELinuxMount=true",
		},
		{
			name: "from the cluster's current snapshot when the node isn't pinned",
			want: "SELinuxMount=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
			cluster.ControlPlaneConfig.ControlPlaneConfigSpec = current
			initialNode.ControlPlaneConfigurationID = tt.pinnedID
			database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
			if tt.pinnedID != uuid.Nil {
				row := db.ControlPlaneConfiguration{ID: tt.pinnedID, ClusterID: cluster.ID, Config: pinned}
				database.On("GetControlPlaneConfiguration", mock.Anything, tt.pinnedID).Return(row, nil)
			}
			agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
			a := snapshotActivities(database, agent, snapshotSharedConfig())

			require.NoError(t, executeSnapshotInit(t, a, KubeadmInitInput{ClusterID: cluster.ID, InitialNodeID: initialNode.ID}))

			initConfig := requireSingleInitRequest(t, agent).GetInitConfig()
			assert.Contains(t, initConfig, tt.want)
			if tt.notWant != "" {
				assert.NotContains(t, initConfig, tt.notWant)
			}
			database.AssertExpectations(t)
		})
	}
}
