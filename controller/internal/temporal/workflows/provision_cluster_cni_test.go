// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type kubeadmInitWithCNITestInput struct {
	ClusterID     uuid.UUID
	InitialNodeID uuid.UUID
}

func kubeadmInitWithCNITestWorkflow(ctx workflow.Context, input kubeadmInitWithCNITestInput) error {
	return kubeadmInitWithCNI(ctx, input.ClusterID, input.InitialNodeID)
}

func TestKubeadmInitWithCNIInstallMode(t *testing.T) {
	tests := []struct {
		name                string
		plugin              db.CniPlugin
		installCNIAfterInit bool
		wantInstallAfter    bool
		wantInstallCNICalls int
	}{
		{name: "ovn-kubernetes installs after init when enabled", plugin: db.CNIOvnKubernetes, installCNIAfterInit: true, wantInstallAfter: true, wantInstallCNICalls: 1},
		{name: "calico installs after init when enabled", plugin: db.CNICalico, installCNIAfterInit: true, wantInstallAfter: true, wantInstallCNICalls: 1},
		{name: "ovn-kubernetes leaves the install to the agent when disabled", plugin: db.CNIOvnKubernetes},
		{name: "calico leaves the install to the agent when disabled", plugin: db.CNICalico},
		{name: "a plugin with no post-init install never reaches InstallCNI", plugin: db.CniPlugin("flannel"), installCNIAfterInit: true},
		{name: "a plugin with no post-init install is left to the agent when disabled", plugin: db.CniPlugin("flannel")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterID, nodeID := uuid.New(), uuid.New()
			env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

			store := &testutil.MockDB{}
			store.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, CniPlugin: tt.plugin}, nil).Maybe()
			installMode := activities.NewActivities(config.Config{InstallCNIAfterInit: tt.installCNIAfterInit}, store, nil, nil, "", nil, "")
			env.RegisterActivity(installMode.CNIInstallMode)

			var kubeadmInit activities.KubeadmInitInput
			a := &activities.Activities{}
			env.OnActivity(a.KubeadmInit, mock.Anything, mock.MatchedBy(func(in activities.KubeadmInitInput) bool {
				kubeadmInit = in
				return true
			})).Return(nil).Once()
			env.OnActivity(a.InstallCNI, mock.Anything, mock.Anything).Return(nil)

			env.ExecuteWorkflow(kubeadmInitWithCNITestWorkflow, kubeadmInitWithCNITestInput{ClusterID: clusterID, InitialNodeID: nodeID})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, tt.wantInstallAfter, kubeadmInit.InstallCNIAfterInit)
			env.AssertNumberOfCalls(t, "InstallCNI", tt.wantInstallCNICalls)
		})
	}
}
