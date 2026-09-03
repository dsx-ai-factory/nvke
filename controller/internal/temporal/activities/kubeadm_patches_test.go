// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestEnsureKubeadmPatchesUsesRequestFilesWhenSupported(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "node-1"}
	fakeAgent := &agenttest.FakeClient{}
	a := &Activities{nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmPatchesActivity(t, a, node, db.NodePool{})

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.KubeadmFilesSupportCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
}

func TestEnsureKubeadmPatchesFallsBackToLegacyRunbook(t *testing.T) {
	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "node-1"}
	nodePool := db.NodePool{
		SystemReservedMemory: "24Gi",
		KubeReservedMemory:   "4Gi",
	}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		KubeadmFilesUnsupported: true,
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, legacyKubeadmPatchesRunbook, scriptName)
			require.Contains(t, scriptContents, "export KUBE_RESERVED_MEM='4Gi'")
			require.Contains(t, scriptContents, "export SYSTEM_RESERVED_MEM='24Gi'")
			require.Contains(t, scriptContents, "mkdir -p \"$KUBEADM_PATCHES_DIR\"")
			require.Contains(t, scriptContents, "kubeletconfiguration0+merge.yaml")
			return &nkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmPatchesActivity(t, a, node, nodePool)

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.KubeadmFilesSupportCalls)
	require.Equal(t, []string{legacyKubeadmPatchesRunbook}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestEnsureKubeadmPatchesPreservesRunbookDefaults(t *testing.T) {
	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		KubeadmFilesUnsupported: true,
		ExecuteScriptHook: func(_ db.Node, _, scriptContents string) (*nkeproto.ExecuteScriptResponse, error) {
			require.Contains(t, scriptContents, "#!/usr/bin/env bash\n# SPDX")
			require.Contains(t, scriptContents, `KUBE_RESERVED_MEM="${KUBE_RESERVED_MEM:-"2Gi"}"`)
			require.Contains(t, scriptContents, `SYSTEM_RESERVED_MEM="${SYSTEM_RESERVED_MEM:-"16Gi"}"`)
			return &nkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmPatchesActivity(t, a, node, db.NodePool{})

	require.NoError(t, err)
	database.AssertExpectations(t)
}

func TestEnsureKubeadmPatchesDoesNotFallbackOnCapabilityProbeFailure(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "node-1"}
	fakeAgent := &agenttest.FakeClient{KubeadmFilesSupportErr: errors.New("reflection unavailable")}
	a := &Activities{nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmPatchesActivity(t, a, node, db.NodePool{})

	require.ErrorContains(t, err, "checking node agent kubeadm file capability")
	require.ErrorContains(t, err, "reflection unavailable")
	require.Empty(t, fakeAgent.ExecuteScriptNames)
}

func executeEnsureKubeadmPatchesActivity(
	t *testing.T,
	a *Activities,
	node db.Node,
	nodePool db.NodePool,
) error {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ensureKubeadmPatches)
	_, err := env.ExecuteActivity(a.ensureKubeadmPatches, node, nodePool)
	return err
}
