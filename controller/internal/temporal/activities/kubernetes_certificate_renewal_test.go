// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestRenewKubernetesCertificatesTypedHappyPath(t *testing.T) {
	for _, tt := range []struct {
		name    string
		outcome nkeproto.KubernetesCertificateRenewalOperation_Outcome
		force   bool
		want    db.CertificateRenewalOutcome
	}{
		{name: "renewed", outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, force: true, want: db.CertificateRenewalOutcomeRenewed},
		{name: "unchanged", outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, want: db.CertificateRenewalOutcomeUnchanged},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
					require.Equal(t, "stable-operation", req.GetOperationId())
					require.Equal(t, int32(14), req.GetThresholdDays())
					require.Equal(t, tt.force, req.GetForce())
					require.Equal(t, int32(3), req.GetExpectedEtcdMembers())
					return certificateRenewalStartV2Response(
						req.GetOperationId(),
						nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
						tt.outcome,
						"",
					), nil
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			result, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, Force: tt.force, ExpectedEtcdMembers: 3,
			})

			require.NoError(t, err)
			require.Equal(t, tt.want, result.Outcome)
			require.Contains(t, result.Output, tt.outcome.String())
			require.Empty(t, result.ErrorMessage)
			require.Zero(t, result.ExitCode)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			require.Empty(t, fakeAgent.GetKubernetesCertificateRenewalCalls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

// TestRenewKubernetesCertificatesV1HistoryUsesV1RPC keeps inputs recorded before
// the membership field on V1; replay must not add V2-only caller obligations.
func TestRenewKubernetesCertificatesV1HistoryUsesV1RPC(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalHook: func(_ db.Node, req *nkeproto.StartKubernetesCertificateRenewalRequest) (*nkeproto.StartKubernetesCertificateRenewalResponse, error) {
			require.Equal(t, "v1-operation", req.GetOperationId())
			require.Equal(t, int32(14), req.GetThresholdDays())
			return certificateRenewalStartResponse(
				req.GetOperationId(),
				nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
				nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
				"",
			), nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	result, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
		NodeID: node.ID, OperationID: "v1-operation", ThresholdDays: 14,
	})

	require.NoError(t, err)
	require.Equal(t, db.CertificateRenewalOutcomeUnchanged, result.Outcome)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.StartKubernetesCertificateRenewalCalls)
	require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
	database.AssertExpectations(t)
}

// TestRenewKubernetesCertificatesFallbackBoundary allows workflow-level executor
// selection only for definitive first-start Unimplemented, never a status failure
// after acceptance that could leave typed work running beside the runbook.
func TestRenewKubernetesCertificatesFallbackBoundary(t *testing.T) {
	tests := []struct {
		name         string
		startHook    func(db.Node, *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error)
		statusHook   func(db.Node, *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
		wantError    string
		selectLegacy bool
	}{
		{
			name: "wrapped first start unimplemented requests workflow selection",
			startHook: func(db.Node, *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return nil, fmt.Errorf("wrapped: %w", status.Error(codes.Unimplemented, "unknown method"))
			},
			wantError:    "does not implement certificate renewal V2",
			selectLegacy: true,
		},
		{
			name: "supported start failure propagates",
			startHook: func(db.Node, *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return nil, status.Error(codes.Internal, "cannot persist operation")
			},
			wantError: "cannot persist operation",
		},
		{
			name: "status unimplemented after accepted start does not fall back",
			startHook: func(_ db.Node, req *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return certificateRenewalStartV2Response(
					req.GetOperationId(),
					nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
					nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
					"",
				), nil
			},
			statusHook: func(db.Node, *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				return nil, status.Error(codes.Unimplemented, "unknown status method")
			},
			wantError: "accepted certificate renewal operation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()

			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: tt.startHook,
				GetKubernetesCertificateRenewalHook:     tt.statusHook,
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 7, Force: true, ExpectedEtcdMembers: 3,
			})

			require.ErrorContains(t, err, tt.wantError)
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, tt.selectLegacy, applicationErr.Type() == CertificateRenewalLegacyRequired)
			if tt.selectLegacy {
				require.True(t, applicationErr.NonRetryable())
			}
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			database.AssertExpectations(t)
		})
	}
}

// TestRenewKubernetesCertificatesFallbackOutcomes preserves V1's diagnostic-only
// output while requiring V2's bounded outcome vocabulary and receipt confirmation.
func TestRenewKubernetesCertificatesFallbackOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		output  string
		v1      bool
		want    db.CertificateRenewalOutcome
		wantErr string
	}{
		{name: "unchanged", output: "RENEWAL_OUTCOME=unchanged\n", want: db.CertificateRenewalOutcomeUnchanged},
		{name: "timestamped renewed", output: "[2026-09-04 12:00:00] RENEWAL_OUTCOME=renewed\n", want: db.CertificateRenewalOutcomeRenewed},
		{name: "last recovered outcome wins", output: "RENEWAL_OUTCOME=renewed\nRENEWAL_OUTCOME=unchanged\n", want: db.CertificateRenewalOutcomeUnchanged},
		{name: "missing", output: "legacy success", wantErr: "outcome marker is missing"},
		{name: "unknown", output: "RENEWAL_OUTCOME=rotated", wantErr: "unknown outcome"},
		{name: "empty", output: "RENEWAL_OUTCOME=\n", wantErr: "unknown outcome"},
		{name: "v1 missing marker", output: "legacy success", v1: true},
		{name: "v1 unknown marker", output: "RENEWAL_OUTCOME=rotated", v1: true},
		{name: "v1 valid marker", output: "RENEWAL_OUTCOME=renewed\n", v1: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			if tt.v1 || tt.wantErr == "" {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
			} else {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			}
			database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID, Name: "test"}, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalHook: func(db.Node, *nkeproto.StartKubernetesCertificateRenewalRequest) (*nkeproto.StartKubernetesCertificateRenewalResponse, error) {
					return nil, status.Error(codes.Unimplemented, "unknown method")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
					if strings.Contains(script, "export STATUS_ONLY='true'") {
						require.False(t, tt.v1)
						return &nkeproto.ExecuteScriptResponse{Output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n"}, nil
					}
					require.Contains(t, script, "export THRESHOLD_DAYS='14'")
					require.Contains(t, script, "export OPERATION_ID='stable-operation'")
					if tt.v1 {
						require.Contains(t, script, "export FORCE='false'")
						require.NotContains(t, script, "export EXPECTED_ETCD_MEMBERS=")
					} else {
						require.Contains(t, script, "export FORCE='true'")
						require.Contains(t, script, "export EXPECTED_ETCD_MEMBERS='3'")
					}
					return &nkeproto.ExecuteScriptResponse{Output: tt.output, ErrorMessage: "diagnostic stderr"}, nil
				},
			}
			a := &Activities{cfg: config.Config{RunbooksDir: t.TempDir()}, db: database, nodeAgentClient: fakeAgent}
			input := KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3, Force: !tt.v1,
			}
			if tt.v1 {
				input.ExpectedEtcdMembers = 0
			}

			var result KubernetesCertificateRenewalResult
			var err error
			if tt.v1 {
				result, err = executeCertificateRenewalActivity(t, a, input)
			} else {
				suite := &testsuite.WorkflowTestSuite{}
				env := suite.NewTestActivityEnvironment()
				env.RegisterActivity(a.RenewLegacyKubernetesCertificates)
				value, activityErr := env.ExecuteActivity(a.RenewLegacyKubernetesCertificates, input)
				err = activityErr
				if err == nil {
					require.NoError(t, value.Get(&result))
				}
			}

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				require.Equal(t, "InvalidCertificateRenewalOutcome", appErr.Type())
				require.True(t, appErr.NonRetryable())
				require.NoError(t, appErr.Details(&result), "failed activities retain diagnostics in error details")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, KubernetesCertificateRenewalResult{
				Outcome: tt.want, Output: tt.output, ErrorMessage: "diagnostic stderr",
			}, result)
			wantScripts := []string{legacyKubernetesCertificateRenewalRunbook}
			if !tt.v1 && tt.wantErr == "" {
				wantScripts = append(wantScripts, legacyKubernetesCertificateRenewalRunbook)
			}
			require.Equal(t, wantScripts, fakeAgent.ExecuteScriptNames)
			if tt.v1 {
				require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.StartKubernetesCertificateRenewalCalls)
				require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			} else {
				require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
				require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			}
			database.AssertExpectations(t)
		})
	}
}

func TestCertificateRenewalTerminalFailureIncludesStructuredOutcome(t *testing.T) {
	operation := certificateRenewalStartResponse(
		"failed-operation",
		nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
		nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED,
		"manifest conflict",
	).GetOperation()

	_, done, err := certificateRenewalTerminalResult(operation)

	require.True(t, done)
	require.ErrorContains(t, err, "OUTCOME_FAILED_NOT_RESTORED")
	require.ErrorContains(t, err, "manifest conflict")
}

func executeCertificateRenewalActivity(
	t *testing.T,
	a *Activities,
	input KubernetesCertificateRenewalInput,
) (KubernetesCertificateRenewalResult, error) {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RenewKubernetesCertificates)
	value, err := env.ExecuteActivity(a.RenewKubernetesCertificates, input)
	if err != nil {
		return KubernetesCertificateRenewalResult{}, err
	}
	var result KubernetesCertificateRenewalResult
	require.NoError(t, value.Get(&result))
	return result, nil
}

func certificateRenewalStartResponse(
	operationID string,
	phase nkeproto.NodeAgentOperationPhase,
	outcome nkeproto.KubernetesCertificateRenewalOperation_Outcome,
	errorMessage string,
) *nkeproto.StartKubernetesCertificateRenewalResponse {
	return &nkeproto.StartKubernetesCertificateRenewalResponse{
		Operation: &nkeproto.KubernetesCertificateRenewalOperation{
			Operation: &nkeproto.NodeAgentOperationStatus{
				OperationId:  operationID,
				Phase:        phase,
				Step:         "test-step",
				ErrorMessage: errorMessage,
			},
			Outcome: outcome,
		},
	}
}

func certificateRenewalStartV2Response(
	operationID string,
	phase nkeproto.NodeAgentOperationPhase,
	outcome nkeproto.KubernetesCertificateRenewalOperation_Outcome,
	errorMessage string,
) *nkeproto.StartKubernetesCertificateRenewalV2Response {
	return &nkeproto.StartKubernetesCertificateRenewalV2Response{
		Operation: certificateRenewalStartResponse(operationID, phase, outcome, errorMessage).GetOperation(),
	}
}

func certificateRenewalTestNode() db.Node {
	return db.Node{
		ID: uuid.New(), ClusterID: uuid.New(), Name: "cp-0", ControlPlane: true,
	}
}

// TestRenewKubernetesCertificatesDoesNotSelectFallbackAfterAmbiguousAttempt models
// a lost acceptance response followed by a capability change. Retrying the same
// typed operation must not authorize a second executor.
func TestRenewKubernetesCertificatesDoesNotSelectFallbackAfterAmbiguousAttempt(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	attempts := 0
	fakeAgent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
			require.Equal(t, "stable-operation", req.GetOperationId())
			attempts++
			if attempts == 1 {
				return nil, status.Error(codes.Unavailable, "accepted but response lost")
			}
			return nil, status.Error(codes.Unimplemented, "agent capability changed")
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(a.RenewKubernetesCertificates)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2},
		})
		return workflow.ExecuteActivity(ctx, a.RenewKubernetesCertificates, KubernetesCertificateRenewalInput{
			NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
		}).Get(ctx, nil)
	})
	require.ErrorContains(t, env.GetWorkflowError(), "ambiguous typed attempt")
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &applicationErr)
	require.NotEqual(t, CertificateRenewalLegacyRequired, applicationErr.Type())
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	require.Equal(t, 2, attempts)
	database.AssertExpectations(t)
}

// TestResolveKubernetesCertificateRenewalRequiresMatchingTerminalStatus separates
// termination evidence from health: a failed restoration is terminal, but missing,
// malformed or another operation's status cannot release ownership.
func TestResolveKubernetesCertificateRenewalRequiresMatchingTerminalStatus(t *testing.T) {
	for _, tt := range []struct {
		name      string
		phase     nkeproto.NodeAgentOperationPhase
		outcome   nkeproto.KubernetesCertificateRenewalOperation_Outcome
		id        string
		missing   bool
		err       error
		wantError bool
	}{
		{name: "succeeded", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED},
		{name: "failed restored", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED},
		{name: "failed not restored", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED},
		{name: "failed before restart", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART},
		{name: "missing", missing: true, wantError: true},
		{name: "mismatched", id: "another-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, outcome: nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, wantError: true},
		{name: "unknown phase", id: "stable-operation", phase: 42, wantError: true},
		{name: "malformed succeeded outcome", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, wantError: true},
		{name: "malformed failed outcome", id: "stable-operation", phase: nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, wantError: true},
		{name: "not found", err: status.Error(codes.NotFound, "unknown operation"), wantError: true},
		{name: "unimplemented", err: status.Error(codes.Unimplemented, "status unsupported"), wantError: true},
		{name: "lost connectivity", err: status.Error(codes.Unavailable, "disconnected"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "stable-operation", req.GetOperationId())
				if tt.missing || tt.err != nil {
					return nil, tt.err
				}
				return &nkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(tt.id, tt.phase, tt.outcome, "").GetOperation()}, nil
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)
			_, err := env.ExecuteActivity(a.ResolveKubernetesCertificateRenewal, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation"})
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestResolveKubernetesCertificateRenewalPollsRunningWithoutStarting(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", finish), func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			polls := 0
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "stable-operation", req.GetOperationId())
				polls++
				phase, outcome := nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED
				if finish && polls == 2 {
					phase, outcome = nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED
				}
				return &nkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(req.GetOperationId(), phase, outcome, "").GetOperation()}, nil
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestActivityEnvironment()
			resolve := func(ctx context.Context) error {
				timeout := 50 * time.Millisecond
				if finish {
					timeout = 10 * time.Second
				}
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				return a.ResolveKubernetesCertificateRenewal(ctx, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation"})
			}
			env.RegisterActivity(resolve)
			_, err := env.ExecuteActivity(resolve)
			if finish {
				require.NoError(t, err)
				require.Equal(t, 2, polls)
			} else {
				require.ErrorContains(t, err, "deadline exceeded")
			}
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
		})
	}
}

// TestResolveLegacyKubernetesCertificateRenewalUsesOnlyEmbeddedStatusMode keeps
// cleanup away from configured overrides that might ignore STATUS_ONLY. Only the
// exact terminal marker from the selected legacy executor is usable evidence.
func TestResolveLegacyKubernetesCertificateRenewalUsesOnlyEmbeddedStatusMode(t *testing.T) {
	for _, tt := range []struct {
		name, output string
		exitCode     int32
		err          error
		wantError    bool
	}{
		{name: "matching", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n"},
		{name: "missing", wantError: true},
		{name: "normal outcome is insufficient", output: "RENEWAL_OUTCOME=renewed\n", wantError: true},
		{name: "mismatched", output: "RENEWAL_TERMINAL_OPERATION_ID=another-operation\n", wantError: true},
		{name: "malformed", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\nunexpected", wantError: true},
		{name: "unsuccessful script", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n", exitCode: 1, wantError: true},
		{name: "unreachable", err: errors.New("connection lost"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			overrides := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(overrides, legacyKubernetesCertificateRenewalRunbook+".sh"), []byte("unsafe override without status mode"), 0o600))
			fakeAgent := &agenttest.FakeClient{
				// A V1-only agent may support GetStatus yet have no typed record of the legacy operation.
				GetKubernetesCertificateRenewalHook: func(db.Node, *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return nil, status.Error(codes.NotFound, "no typed operation")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
					require.Contains(t, script, "export STATUS_ONLY='true'")
					require.Contains(t, script, "export OPERATION_ID='stable-operation'")
					require.Contains(t, script, "export LOCK_TIMEOUT='240'")
					require.Contains(t, script, `exec 9<"$LOCK_FILE"`)
					require.NotContains(t, script, "unsafe override")
					return &nkeproto.ExecuteScriptResponse{Output: tt.output, ExitCode: tt.exitCode}, tt.err
				},
			}
			a := &Activities{cfg: config.Config{RunbooksDir: overrides}, db: database, nodeAgentClient: fakeAgent}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)
			_, err := env.ExecuteActivity(a.ResolveKubernetesCertificateRenewal, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation", Legacy: true})
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			require.Empty(t, fakeAgent.GetKubernetesCertificateRenewalCalls)
			require.Len(t, fakeAgent.ExecuteScriptNames, 1)
		})
	}
}

// TestRenewLegacyKubernetesCertificatesRejectsReceiptUnawareSuccess prevents an
// override's success marker from bypassing V2 fencing. Missing receipt evidence
// fails without retrying that override and retains its original diagnostics.
func TestRenewLegacyKubernetesCertificatesRejectsReceiptUnawareSuccess(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Once()
	overrides := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(overrides, legacyKubernetesCertificateRenewalRunbook+".sh"),
		[]byte("#!/bin/bash\necho RENEWAL_OUTCOME=renewed\n"), 0o600))
	fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(_ db.Node, _, script string) (*nkeproto.ExecuteScriptResponse, error) {
		if strings.Contains(script, "export STATUS_ONLY='true'") {
			return &nkeproto.ExecuteScriptResponse{ExitCode: 1, ErrorMessage: "no terminal receipt"}, nil
		}
		return &nkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=renewed\n", ErrorMessage: "original diagnostic"}, nil
	}}
	a := &Activities{cfg: config.Config{RunbooksDir: overrides}, db: database, nodeAgentClient: fakeAgent}
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RenewLegacyKubernetesCertificates)

	_, err := env.ExecuteActivity(a.RenewLegacyKubernetesCertificates, KubernetesCertificateRenewalInput{
		NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
	})

	require.Error(t, err, "an old override's success must not bypass the fencing contract")
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.Equal(t, "UnconfirmedLegacyCertificateRenewal", applicationErr.Type())
	require.True(t, applicationErr.NonRetryable(), "do not rerun an override that cannot prove fencing")
	var result KubernetesCertificateRenewalResult
	require.NoError(t, applicationErr.Details(&result))
	require.Equal(t, "RENEWAL_OUTCOME=renewed\n", result.Output)
	require.Equal(t, "original diagnostic", result.ErrorMessage)
	require.Equal(t, db.CertificateRenewalOutcomeRenewed, result.Outcome)
	require.Len(t, fakeAgent.ExecuteScriptNames, 2)
	database.AssertExpectations(t)
}
