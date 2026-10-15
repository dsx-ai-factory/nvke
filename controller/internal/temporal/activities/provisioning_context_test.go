// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestProvisioningContextKeepsAgentSnapshotThroughJoin(t *testing.T) {
	node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), ControlPlane: true, ControlPlaneConfigurationID: uuid.New()}
	configuration := db.ControlPlaneConfiguration{Config: db.ControlPlaneConfigSpec{NodeAgentConfig: db.NodeAgentConfig{Version: "v0.1.2"}}}
	cluster := db.Cluster{ID: node.ClusterID, ProviderType: db.ProviderTypeCAPI}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(cluster, nil)
	database.On("GetControlPlaneConfiguration", mock.Anything, node.ControlPlaneConfigurationID).Return(configuration, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil).Once()
	database.MockTx.On("GetControlPlaneConfiguration", mock.Anything, node.ControlPlaneConfigurationID).Return(configuration, nil).Once()
	database.MockTx.On("UpdateNode", mock.Anything, mock.Anything).Return(db.Node{}, nil).Run(func(args mock.Arguments) { node = *args.Get(1).(*db.Node) }).Once()
	a := &Activities{db: database, cfg: config.Config{NodeAgent: config.NodeAgentConfig{UpdateMethod: "apt"}}}
	first, err := a.GetNodeProvisioningContext(context.Background(), node.ID)
	require.NoError(t, err)
	require.Equal(t, "v0.1.2", first.NodeAgentVersion)
	require.Equal(t, "apt", first.NodeAgentUpdateMethod)
	require.True(t, first.ControlPlane)
	// A later desired target and site setting do not replace this node's snapshot.
	database.MockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil).Once()
	a.cfg.NodeAgent.UpdateMethod = "bundle"
	second, err := a.GetNodeProvisioningContext(context.Background(), node.ID)
	require.NoError(t, err)
	require.Equal(t, first, second)
	database.MockTx.AssertExpectations(t)
}
