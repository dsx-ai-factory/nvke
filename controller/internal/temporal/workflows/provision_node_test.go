// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// labelNodeTestWorkflow is a thin wrapper so the unexported labelNode helper can
// be driven through the Temporal test environment.
func labelNodeTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return labelNode(ctx, nodeID)
}

// cleanBeforeControlPlaneRejoinTestWorkflow exposes the unexported cleanup
// helper to Temporal's deterministic workflow test environment.
func cleanBeforeControlPlaneRejoinTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	cleanBeforeControlPlaneRejoin(ctx, nodeID)
	return nil
}

func provisionCAPINodeTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return provisionCAPINode(ctx, nodeID)
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

	a := activities.NewActivities(config.Config{ProfilesDir: profilesDir}, database, nil, nil, "", nil, "")

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

	// mockedLabelNodeEnv serves cluster and node from mocked activities, so
	// a case can use provider payloads the seeded test database cannot.
	mockedLabelNodeEnv := func(cluster db.Cluster, node db.Node) labelNodeEnv {
		testSuite := &testsuite.WorkflowTestSuite{}
		env := testSuite.NewTestWorkflowEnvironment()
		env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
		env.RegisterWorkflow(labelNodeTestWorkflow)

		var a *activities.Activities
		captured := &activities.LabelNodeInput{}
		wasCalled := false
		env.OnActivity(a.GetClusterForNode, mock.Anything, node.ID).Return(cluster, nil)
		env.OnActivity(a.GetNode, mock.Anything, node.ID).Return(node, nil)
		env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).
			Return(activities.LabelNodeResult{}, nil).
			Run(func(args mock.Arguments) {
				wasCalled = true
				*captured = args.Get(1).(activities.LabelNodeInput)
			}).Maybe()

		return labelNodeEnv{env: env, captured: captured, called: &wasCalled, nodeID: node.ID}
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
			wantLabels: []string{keys.MachineID.String() + "=machine-abc123"},
		},
		"adds machine ID label from nico node provider": {
			setup: func(t *testing.T) labelNodeEnv {
				t.Helper()

				clusterID := uuid.MustParse("50000000-0000-0000-0000-000000000002")
				return mockedLabelNodeEnv(
					db.Cluster{
						ID:           clusterID,
						ProviderType: db.ProviderTypeNico,
						Nico: &db.NicoClusterProvider{
							OrgID:  "org-1",
							SiteID: uuid.MustParse("50000000-0000-0000-0000-000000000003"),
							VpcID:  uuid.MustParse("50000000-0000-0000-0000-000000000004"),
						},
					},
					db.Node{
						ID:        uuid.MustParse("50000000-0000-0000-0000-000000000001"),
						ClusterID: clusterID,
						Name:      "worker-0",
						Nico: &db.NicoNodeProvider{
							ForgeMachineID: "payload-machine",
						},
					},
				)
			},
			wantCalled:     true,
			wantLabels:     []string{keys.MachineID.String() + "=payload-machine"},
			unwantedLabels: []string{keys.MachineID.String() + "=legacy-machine"},
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
		"capi adds no machine ID label": {
			setup: func(t *testing.T) labelNodeEnv {
				t.Helper()

				clusterID := uuid.MustParse("50000000-0000-0000-0000-000000000022")
				return mockedLabelNodeEnv(
					db.Cluster{
						ID:           clusterID,
						ProviderType: db.ProviderTypeCAPI,
						Capi:         &db.CAPIClusterProvider{InfraCluster: []byte(`{"kind":"DockerCluster"}`)},
						CniPlugin:    db.CNIOvnKubernetes,
					},
					db.Node{
						ID:        uuid.MustParse("50000000-0000-0000-0000-000000000021"),
						ClusterID: clusterID,
						Name:      "worker-0",
						Nico:      &db.NicoNodeProvider{ForgeMachineID: "stray-machine"},
					},
				)
			},
			wantCalled:     true,
			wantLabels:     []string{"k8s.ovn.org/zone-name=worker-0"},
			unwantedLabels: []string{keys.MachineID.String() + "=stray-machine"},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			testEnv := params.setup(t)

			testEnv.env.ExecuteWorkflow(labelNodeTestWorkflow, testEnv.nodeID)

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

func TestCleanBeforeControlPlaneRejoinUsesTypedEtcdRemoval(t *testing.T) {
	nodeID := uuid.MustParse("50000000-0000-0000-0000-000000000011")
	clusterID := uuid.MustParse("50000000-0000-0000-0000-000000000012")
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "control-plane-1",
		ControlPlane: true,
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
	env.RegisterWorkflow(cleanBeforeControlPlaneRejoinTestWorkflow)

	var a *activities.Activities
	env.OnActivity(a.GetNode, mock.Anything, nodeID).Return(node, nil).Once()
	env.OnActivity(a.RemoveEtcdMember, mock.Anything, mock.MatchedBy(func(input activities.EtcdMemberRemovalInput) bool {
		return input.ClusterID == clusterID &&
			input.TargetNodeID == nodeID &&
			input.TargetMemberName == node.Name
	})).Return(nil).Once()
	env.OnActivity(a.KubeadmReset, mock.Anything, mock.MatchedBy(func(input activities.KubeadmResetInput) bool {
		return input.NodeID == nodeID &&
			len(input.SkipPhases) == 1 &&
			input.SkipPhases[0] == removeEtcdMember
	})).Return(activities.KubeadmResetResult{}, nil).Once()

	env.ExecuteWorkflow(cleanBeforeControlPlaneRejoinTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestProvisionCAPINodeClaimsNodeBeforeCreatingInfraMachine(t *testing.T) {
	nodeID := uuid.MustParse("50000000-0000-0000-0000-000000000021")

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
	env.RegisterWorkflow(provisionCAPINodeTestWorkflow)

	var a *activities.Activities
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.MatchedBy(func(update activities.NodeConditionsUpdate) bool {
		return update.NodeID == nodeID &&
			update.Conditions.Pending != nil &&
			update.Conditions.Pending.Status == db.ConditionStatusTrue &&
			update.Conditions.ProvisioningClaimed != nil &&
			update.Conditions.ProvisioningClaimed.Status == db.ConditionStatusTrue &&
			update.Conditions.Provisioning != nil &&
			update.Conditions.Provisioning.Status == db.ConditionStatusFalse
	})).Return(nil).Once()
	env.OnActivity(a.CreateCAPIInfraMachineForNode, mock.Anything, activities.CreateCAPIInfraMachineForNodeInput{
		NodeID: nodeID,
	}).Return(nil).Once()

	env.ExecuteWorkflow(provisionCAPINodeTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func applyNodeMetadataTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return applyNodeMetadata(ctx, nodeID)
}

func TestApplyNodeMetadataVersionGate(t *testing.T) {
	for _, test := range []struct {
		name           string
		historyVersion workflow.Version
		wantApplied    bool
	}{
		{name: "new workflow applies node labels and taints", historyVersion: 1, wantApplied: true},
		{name: "existing workflow keeps its recorded steps", historyVersion: workflow.DefaultVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(applyNodeMetadataTestWorkflow)
			env.OnGetVersion("apply-node-metadata", workflow.DefaultVersion, 1).Return(test.historyVersion)

			var a *activities.Activities
			nodeID := uuid.New()
			applied := false
			env.OnActivity(a.ApplyNodeMetadata, mock.Anything, activities.ApplyNodeMetadataInput{
				NodeID:                           nodeID,
				NodeProvisioningConditionMessage: "Applying node labels and taints",
			}).Return(activities.ApplyNodeMetadataResult{}, nil).Run(func(mock.Arguments) { applied = true }).Maybe()

			env.ExecuteWorkflow(applyNodeMetadataTestWorkflow, nodeID)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, test.wantApplied, applied)
		})
	}
}

func TestApplyNodeMetadataFailsWorkflowOnNonRetryableError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(applyNodeMetadataTestWorkflow)

	var a *activities.Activities
	errMessage := `unsupported taint effect "invalid-taint-effect"`
	invalidMetadata := temporal.NewNonRetryableApplicationError(errMessage, "InvalidNodeMetadata", nil)
	env.OnActivity(a.ApplyNodeMetadata, mock.Anything, mock.Anything).Return(activities.ApplyNodeMetadataResult{}, invalidMetadata).Once()

	env.ExecuteWorkflow(applyNodeMetadataTestWorkflow, uuid.New())

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), errMessage)
	env.AssertExpectations(t)
}

func verifyControlPlaneRoleLabelTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return verifyControlPlaneRoleLabel(ctx, nodeID)
}

func TestVerifyControlPlaneRoleLabelVersionGate(t *testing.T) {
	for _, test := range []struct {
		name           string
		historyVersion workflow.Version
		wantVerified   bool
	}{
		{name: "new workflow verifies the role label", historyVersion: 1, wantVerified: true},
		{name: "existing workflow keeps its recorded steps", historyVersion: workflow.DefaultVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(verifyControlPlaneRoleLabelTestWorkflow)
			env.OnGetVersion("apply-node-metadata", workflow.DefaultVersion, 1).Return(test.historyVersion)

			var a *activities.Activities
			nodeID := uuid.New()
			verified := false
			env.OnActivity(a.VerifyControlPlaneRoleLabel, mock.Anything, nodeID).Return(nil).Run(func(mock.Arguments) { verified = true }).Maybe()

			env.ExecuteWorkflow(verifyControlPlaneRoleLabelTestWorkflow, nodeID)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, test.wantVerified, verified)
		})
	}
}

func TestVerifyControlPlaneRoleLabelFailsWorkflowAfterRetries(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(verifyControlPlaneRoleLabelTestWorkflow)

	var a *activities.Activities
	const errMessage = "control-plane node/cp-0 is missing expected label node-role.kubernetes.io/control-plane"
	env.OnActivity(a.VerifyControlPlaneRoleLabel, mock.Anything, mock.Anything).Return(errors.New(errMessage)).Times(3)

	env.ExecuteWorkflow(verifyControlPlaneRoleLabelTestWorkflow, uuid.New())

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), errMessage)
	env.AssertExpectations(t)
}

func agentBeforePreflightTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return provisionNode(ctx, ProvisionNodeInput{NodeID: nodeID}, nodeMetricLabels{}, activities.NodeProvisioningContext{NodeID: nodeID, NodeAgentVersion: "v0.1.2", NodeAgentUpdateMethod: "apt"})
}

func TestProvisioningConvergesAgentBeforePreflight(t *testing.T) {
	for _, test := range []struct {
		name           string
		historyVersion workflow.Version
		wantUpgrade    bool
	}{
		{name: "new workflow upgrades before preflight", historyVersion: 1, wantUpgrade: true},
		{name: "existing workflow keeps its recorded steps", historyVersion: workflow.DefaultVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(agentBeforePreflightTestWorkflow)
			env.OnGetVersion("node-agent-before-preflight", workflow.DefaultVersion, 1).Return(test.historyVersion)
			var a *activities.Activities
			nodeID := uuid.New()
			completed := false
			env.OnActivity(a.WaitForCAPIMachineReady, mock.Anything, mock.Anything).Return("", nil).Once()
			if test.wantUpgrade {
				env.OnActivity(a.EnsureNodeAgentVersion, mock.Anything, activities.EnsureNodeAgentVersionInput{NodeID: nodeID, Version: "v0.1.2", Method: "apt"}).Return(nil).Run(func(mock.Arguments) { completed = true }).Once()
			}
			env.OnActivity(a.NodePreflight, mock.Anything, mock.Anything).Return(nil).Run(func(mock.Arguments) { require.Equal(t, test.wantUpgrade, completed) }).Once()
			// Stop after preflight so this test does not replace the join workflow tests.
			env.OnActivity(a.GetNodeConfigProfile, mock.Anything, mock.Anything).Return(activities.ConfigProfile{}, temporal.NewNonRetryableApplicationError("stop after preflight", "test", nil)).Once()
			env.ExecuteWorkflow(agentBeforePreflightTestWorkflow, nodeID)
			require.ErrorContains(t, env.GetWorkflowError(), "stop after preflight")
			env.AssertExpectations(t)
		})
	}
}
