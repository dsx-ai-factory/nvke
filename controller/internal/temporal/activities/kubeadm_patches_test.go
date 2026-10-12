// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestEnsureKubeadmFilesUsesRequestFilesWhenSupported(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "node-1"}
	fakeAgent := &agenttest.FakeClient{}
	a := &Activities{nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmFilesActivity(t, a, node, []kubeadmconfig.File{{Path: "/etc/kubernetes/patches/kubelet.yaml"}})

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.KubeadmFilesSupportCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
}

func TestEnsureKubeadmFilesFallsBackWithEveryRenderedFile(t *testing.T) {
	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "node-1"}
	files := []kubeadmconfig.File{
		{
			Path:    "/etc/kubernetes/patches/kubeletconfiguration0+merge.yaml",
			Content: []byte("kubelet patch\n"),
			Mode:    0o644,
			Owner:   "root:root",
		},
		{
			Path:    "/etc/kubernetes/patches/kube-apiserver-grace+merge.yaml",
			Content: []byte("termination grace patch\n"),
			Mode:    0o640,
			Owner:   "root:root",
		},
		{
			Path:    "/etc/kubernetes/policies/audit-policy.yaml",
			Content: []byte("audit policy with 'quoted' content\n"),
			Mode:    0o600,
			Owner:   "root:root",
		},
	}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		KubeadmFilesUnsupported: true,
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nvkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, legacyKubeadmFilesRunbook, scriptName)
			require.Contains(t, scriptContents, "stage_file")
			return &nvkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmFilesActivity(t, a, node, files)

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.KubeadmFilesSupportCalls)
	require.Equal(t, []string{legacyKubeadmFilesRunbook}, fakeAgent.ExecuteScriptNames)
	require.Equal(t, [][]string{{
		files[0].Path, base64.StdEncoding.EncodeToString(files[0].Content), "644", files[0].Owner,
		files[1].Path, base64.StdEncoding.EncodeToString(files[1].Content), "640", files[1].Owner,
		files[2].Path, base64.StdEncoding.EncodeToString(files[2].Content), "600", files[2].Owner,
	}}, fakeAgent.ExecuteScriptArgs)
	database.AssertExpectations(t)
}

func TestEnsureKubeadmFilesSkipsCapabilityProbeWhenThereAreNoFiles(t *testing.T) {
	fakeAgent := &agenttest.FakeClient{}
	a := &Activities{nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmFilesActivity(t, a, db.Node{}, nil)

	require.NoError(t, err)
	require.Empty(t, fakeAgent.KubeadmFilesSupportCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
}

func TestEnsureKubeadmFilesDoesNotFallbackOnCapabilityProbeFailure(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "node-1"}
	fakeAgent := &agenttest.FakeClient{KubeadmFilesSupportErr: errors.New("reflection unavailable")}
	a := &Activities{nodeAgentClient: fakeAgent}

	err := executeEnsureKubeadmFilesActivity(t, a, node, []kubeadmconfig.File{{Path: "/etc/kubernetes/file"}})

	require.ErrorContains(t, err, "checking node agent kubeadm file capability")
	require.ErrorContains(t, err, "reflection unavailable")
	require.Empty(t, fakeAgent.ExecuteScriptNames)
}

func executeEnsureKubeadmFilesActivity(
	t *testing.T,
	a *Activities,
	node db.Node,
	files []kubeadmconfig.File,
) error {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ensureKubeadmFiles)
	_, err := env.ExecuteActivity(a.ensureKubeadmFiles, node, files)
	return err
}
