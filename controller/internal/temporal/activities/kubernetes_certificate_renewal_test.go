// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestRenewKubernetesCertificatesTypedHappyPath(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
			require.Equal(t, "stable-operation", req.GetOperationId())
			require.Equal(t, int32(14), req.GetThresholdDays())
			return certificateRenewalStartResponse(
				req.GetOperationId(),
				nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED,
				"",
			), nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	result, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
		NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14,
	})

	require.NoError(t, err)
	require.Contains(t, result.Output, "OUTCOME_RENEWED")
	require.Zero(t, result.ExitCode)
	require.Empty(t, fakeAgent.GetKubernetesCertificateRenewalCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestRenewKubernetesCertificatesFallbackBoundary(t *testing.T) {
	tests := []struct {
		name         string
		startHook    func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error)
		statusHook   func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
		wantFallback bool
		wantError    string
	}{
		{
			name: "wrapped start unimplemented uses legacy runbook",
			startHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
				return nil, fmt.Errorf("wrapped: %w", status.Error(codes.Unimplemented, "unknown method"))
			},
			wantFallback: true,
		},
		{
			name: "supported start failure propagates",
			startHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
				return nil, status.Error(codes.Internal, "cannot persist operation")
			},
			wantError: "cannot persist operation",
		},
		{
			name: "status unimplemented after accepted start does not fall back",
			startHook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
				return certificateRenewalStartResponse(
					req.GetOperationId(),
					nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
					nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
					"",
				), nil
			},
			statusHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
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
			cfg := config.Config{}
			if tt.wantFallback {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
				database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID, Name: "test"}, nil).Once()
				runbooksDir := t.TempDir()
				require.NoError(t, os.WriteFile(
					filepath.Join(runbooksDir, legacyKubernetesCertificateRenewalRunbook+".sh"),
					[]byte("#!/bin/bash\nexit 0\n"),
					0o600,
				))
				cfg.RunbooksDir = runbooksDir
			}
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalHook: tt.startHook,
				GetKubernetesCertificateRenewalHook:   tt.statusHook,
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
					require.Contains(t, script, "export THRESHOLD_DAYS='7'")
					return &nvkeproto.ExecuteScriptResponse{Output: "legacy success"}, nil
				},
			}
			a := &Activities{cfg: cfg, db: database, nodeAgentClient: fakeAgent}

			result, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 7,
			})

			if tt.wantError == "" {
				require.NoError(t, err)
				require.Equal(t, "legacy success", result.Output)
			} else {
				require.ErrorContains(t, err, tt.wantError)
			}
			require.Equal(t, tt.wantFallback, len(fakeAgent.ExecuteScriptNames) == 1)
			database.AssertExpectations(t)
		})
	}
}

func TestCertificateRenewalTerminalFailureIncludesStructuredOutcome(t *testing.T) {
	operation := certificateRenewalStartResponse(
		"failed-operation",
		nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
		nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED,
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
	phase nvkeproto.NodeAgentOperationPhase,
	outcome nvkeproto.KubernetesCertificateRenewalOperation_Outcome,
	errorMessage string,
) *nvkeproto.StartKubernetesCertificateRenewalResponse {
	return &nvkeproto.StartKubernetesCertificateRenewalResponse{
		Operation: &nvkeproto.KubernetesCertificateRenewalOperation{
			Operation: &nvkeproto.NodeAgentOperationStatus{
				OperationId:  operationID,
				Phase:        phase,
				Step:         "test-step",
				ErrorMessage: errorMessage,
			},
			Outcome: outcome,
		},
	}
}

func certificateRenewalTestNode() db.Node {
	return db.Node{
		ID: uuid.New(), ClusterID: uuid.New(), Name: "cp-0", ControlPlane: true,
	}
}
