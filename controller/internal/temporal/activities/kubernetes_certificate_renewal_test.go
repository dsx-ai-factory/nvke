// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	node := pooledCertificateRenewalTestNode()
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
			node := pooledCertificateRenewalTestNode()
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

// pooledCertificateRenewalTestNode is not managed by the control-plane controller,
// so the renewal skips cluster admission and the test exercises the agent RPC only.
func pooledCertificateRenewalTestNode() db.Node {
	node := certificateRenewalTestNode()
	node.NodePoolID = uuid.New()
	return node
}

func TestRenewalWaitsForAnotherNodesReservation(t *testing.T) {
	for _, test := range []struct {
		name      string
		release   bool
		wantError string
	}{
		{name: "reserves once the other node releases", release: true},
		{name: "gives up after the wait", wantError: "waited"},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			other := uuid.New()
			database := &agentAdmissionDatabase{node: node, cluster: db.Cluster{
				ID:                 node.ClusterID,
				CertificateRenewal: &db.NodeAgentCertificateRenewal{NodeID: other, OperationID: "other-operation"},
			}}
			if test.release {
				go func() {
					time.Sleep(20 * time.Millisecond)
					database.mu.Lock()
					database.cluster.CertificateRenewal = nil
					database.mu.Unlock()
				}()
			}
			a := &Activities{db: database}
			input := KubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "new-operation", ThresholdDays: 14}

			err := a.awaitCertificateRenewalReservation(t.Context(), node, &input, time.Millisecond, 2*time.Second)

			if test.wantError == "" {
				require.NoError(t, err)
				require.Equal(t, node.ID, database.cluster.CertificateRenewal.NodeID)
				return
			}
			require.ErrorIs(t, err, errCertificateRenewalBusy)
			require.ErrorContains(t, err, test.wantError)
			require.Equal(t, other, database.cluster.CertificateRenewal.NodeID)
		})
	}
}

func TestRenewalAdmissionAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name         string
		upgrade      bool
		accepted     bool
		statusError  bool
		phase        nvkeproto.NodeAgentOperationPhase
		wantRetained bool
		wantError    string
	}{
		{name: "upgrade blocks renewal before its first RPC", upgrade: true, wantError: "installation is in progress"},
		{name: "completed renewal releases admission", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED},
		{name: "lost response retains accepted work", statusError: true, phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, wantRetained: true, wantError: "connection lost"},
		{name: "later workflow reattaches to accepted work", accepted: true, phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &agentAdmissionDatabase{cluster: db.Cluster{ID: node.ClusterID}, node: node}
			if test.upgrade {
				database.cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: node.ID, TargetVersion: "v0.1.2", Method: "apt", Phase: "Running"}
			}
			if test.accepted {
				database.cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "accepted-operation"}
			}
			fake := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
					require.False(t, test.upgrade, "renewal must not call the agent during an upgrade")
					require.True(t, database.mu.TryLock(), "renewal RPC must not hold the database transaction")
					require.NotNil(t, database.cluster.CertificateRenewal, "record admission before the RPC")
					require.Equal(t, req.GetOperationId(), database.cluster.CertificateRenewal.OperationID)
					if test.accepted {
						require.Equal(t, "accepted-operation", req.GetOperationId())
					}
					database.mu.Unlock()
					return certificateRenewalStartResponse(req.GetOperationId(), test.phase, nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, ""), nil
				},
				GetKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return nil, status.Error(codes.Unavailable, "connection lost")
				},
			}
			a := &Activities{cfg: config.Config{}, db: database, nodeAgentClient: fake}
			_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "new-operation", ThresholdDays: 14})
			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantError)
			}
			require.Equal(t, test.wantRetained, database.cluster.CertificateRenewal != nil)
		})
	}
}
