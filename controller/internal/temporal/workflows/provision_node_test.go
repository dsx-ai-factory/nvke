// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

// labelNodeTestWorkflow is a thin wrapper so the unexported labelNode helper can
// be driven through the Temporal test environment.
func labelNodeTestWorkflow(ctx workflow.Context, nodeID uuid.UUID, addConfigProfileLabels bool) error {
	return labelNode(ctx, nodeID, addConfigProfileLabels)
}

// setupLabelNodeEnv builds a focused test env that registers the real DB
// activities labelNode needs and a capturing mock for LabelNode.
func setupLabelNodeEnv(t *testing.T, database db.DB, profilesDir string) (
	env *testsuite.TestWorkflowEnvironment,
	captured *activities.LabelNodeInput,
	called *bool,
) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env = testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})

	a := activities.NewActivities(config.Config{ProfilesDir: profilesDir}, database, nil, nil, nil, "", nil)

	env.RegisterActivity(a.GetNode)
	env.RegisterActivity(a.GetClusterForNode)
	env.RegisterWorkflow(labelNodeTestWorkflow)

	captured = &activities.LabelNodeInput{}
	wasCalled := false
	called = &wasCalled
	env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).
		Return(activities.LabelNodeResult{}, nil).
		Run(func(args mock.Arguments) {
			*called = true
			*captured = args.Get(1).(activities.LabelNodeInput)
		}).Maybe()

	return env, captured, called
}

func TestLabelNode(t *testing.T) {
	type labelNodeEnv struct {
		env      *testsuite.TestWorkflowEnvironment
		captured *activities.LabelNodeInput
		called   *bool
		nodeID   uuid.UUID
	}
	type parameters struct {
		setup          func(t *testing.T) labelNodeEnv
		wantCalled     bool
		wantLabels     []string
		unwantedLabels []string
	}

	tests := map[string]parameters{
		"adds machine ID label": {
			setup: func(t *testing.T) labelNodeEnv {
				t.Helper()

				database, res := setupTestDB(t, seedOpts{forgeMachineID: "machine-abc123"})
				env, captured, called := setupLabelNodeEnv(t, database, res.profilesDir)
				return labelNodeEnv{env: env, captured: captured, called: called, nodeID: res.nodeID}
			},
			wantCalled: true,
			wantLabels: []string{constants.LabelKeyMachineID + "=machine-abc123"},
		},
		"adds machine ID label from nico node provider": {
			setup: func(t *testing.T) labelNodeEnv {
				t.Helper()

				nodeID := uuid.MustParse("50000000-0000-0000-0000-000000000001")
				clusterID := uuid.MustParse("50000000-0000-0000-0000-000000000002")
				siteID := uuid.MustParse("50000000-0000-0000-0000-000000000003")
				vpcID := uuid.MustParse("50000000-0000-0000-0000-000000000004")
				node := db.Node{
					ID:             nodeID,
					ClusterID:      clusterID,
					Name:           "worker-0",
					ForgeMachineID: "legacy-machine",
					Nico: &db.NicoNodeProvider{
						ForgeMachineID: "payload-machine",
					},
				}
				cluster := db.Cluster{
					ID:           clusterID,
					ProviderType: db.ProviderTypeNico,
					Nico: &db.NicoClusterProvider{
						OrgID:  "org-1",
						SiteID: siteID,
						VpcID:  vpcID,
					},
				}
				testSuite := &testsuite.WorkflowTestSuite{}
				env := testSuite.NewTestWorkflowEnvironment()
				env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
				env.RegisterWorkflow(labelNodeTestWorkflow)

				var a *activities.Activities
				captured := &activities.LabelNodeInput{}
				wasCalled := false
				env.OnActivity(a.GetClusterForNode, mock.Anything, nodeID).Return(cluster, nil)
				env.OnActivity(a.GetNode, mock.Anything, nodeID).Return(node, nil)
				env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).
					Return(activities.LabelNodeResult{}, nil).
					Run(func(args mock.Arguments) {
						wasCalled = true
						*captured = args.Get(1).(activities.LabelNodeInput)
					})

				return labelNodeEnv{env: env, captured: captured, called: &wasCalled, nodeID: nodeID}
			},
			wantCalled:     true,
			wantLabels:     []string{constants.LabelKeyMachineID + "=payload-machine"},
			unwantedLabels: []string{constants.LabelKeyMachineID + "=legacy-machine"},
		},
		"skips machine ID label when empty": {
			setup: func(t *testing.T) labelNodeEnv {
				t.Helper()

				database, res := setupTestDB(t, seedOpts{forgeMachineID: ""})
				env, captured, called := setupLabelNodeEnv(t, database, res.profilesDir)
				return labelNodeEnv{env: env, captured: captured, called: called, nodeID: res.nodeID}
			},
			wantCalled: false,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			testEnv := params.setup(t)

			testEnv.env.ExecuteWorkflow(labelNodeTestWorkflow, testEnv.nodeID, false)

			require.True(t, testEnv.env.IsWorkflowCompleted())
			require.NoError(t, testEnv.env.GetWorkflowError())
			require.Equal(t, params.wantCalled, *testEnv.called)
			for _, label := range params.wantLabels {
				require.Contains(t, testEnv.captured.Labels, label)
			}
			for _, label := range params.unwantedLabels {
				require.NotContains(t, testEnv.captured.Labels, label)
			}
		})
	}
}
