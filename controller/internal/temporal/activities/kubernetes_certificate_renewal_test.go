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

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestRenewKubernetesCertificatesTypedHappyPath(t *testing.T) {
	for _, tt := range []struct {
		name    string
		outcome nvkeproto.KubernetesCertificateRenewalOperation_Outcome
		force   bool
		want    db.CertificateRenewalOutcome
	}{
		{name: "renewed", outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, force: true, want: db.CertificateRenewalOutcomeRenewed},
		{name: "unchanged", outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, want: db.CertificateRenewalOutcomeUnchanged},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
					require.Equal(t, "stable-operation", req.GetOperationId())
					require.Equal(t, int32(14), req.GetThresholdDays())
					require.Equal(t, tt.force, req.GetForce())
					require.Equal(t, int32(3), req.GetExpectedEtcdMembers())
					return certificateRenewalStartV2Response(
						req.GetOperationId(),
						nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
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
	node := pooledCertificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
			require.Equal(t, "v1-operation", req.GetOperationId())
			require.Equal(t, int32(14), req.GetThresholdDays())
			return certificateRenewalStartResponse(
				req.GetOperationId(),
				nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
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
		startHook    func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error)
		statusHook   func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
		wantError    string
		selectLegacy bool
	}{
		{
			name: "wrapped first start unimplemented requests workflow selection",
			startHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return nil, fmt.Errorf("wrapped: %w", status.Error(codes.Unimplemented, "unknown method"))
			},
			wantError:    "does not implement certificate renewal V2",
			selectLegacy: true,
		},
		{
			name: "supported start failure propagates",
			startHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return nil, status.Error(codes.Internal, "cannot persist operation")
			},
			wantError: "cannot persist operation",
		},
		{
			name: "status unimplemented after accepted start does not fall back",
			startHook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
				return certificateRenewalStartV2Response(
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
			node := pooledCertificateRenewalTestNode()
			database := &testutil.MockDB{}
			if tt.v1 || tt.wantErr == "" {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
			} else {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			}
			database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID, Name: "test"}, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
					return nil, status.Error(codes.Unimplemented, "unknown method")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
					if strings.Contains(script, "export STATUS_ONLY='true'") {
						require.False(t, tt.v1)
						return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n"}, nil
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
					return &nvkeproto.ExecuteScriptResponse{Output: tt.output, ErrorMessage: "diagnostic stderr"}, nil
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

func certificateRenewalStartV2Response(
	operationID string,
	phase nvkeproto.NodeAgentOperationPhase,
	outcome nvkeproto.KubernetesCertificateRenewalOperation_Outcome,
	errorMessage string,
) *nvkeproto.StartKubernetesCertificateRenewalV2Response {
	return &nvkeproto.StartKubernetesCertificateRenewalV2Response{
		Operation: certificateRenewalStartResponse(operationID, phase, outcome, errorMessage).GetOperation(),
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

// TestRenewalReservationAfterFailedStart keeps #1692's release only where the
// node's termination is confirmed: a legacy script that exited. A failed or empty
// Start may still have been accepted, so its retries reattach to the reservation.
func TestRenewalReservationAfterFailedStart(t *testing.T) {
	for _, test := range []struct {
		name         string
		response     *nvkeproto.StartKubernetesCertificateRenewalResponse
		err          error
		script       func() (*nvkeproto.ExecuteScriptResponse, error)
		wantError    string
		wantReleased bool
	}{
		{name: "start RPC fails", err: status.Error(codes.Unavailable, "connection refused"), wantError: "failed to start certificate renewal"},
		{name: "start returns no operation", response: &nvkeproto.StartKubernetesCertificateRenewalResponse{}, wantError: "empty certificate renewal operation"},
		{
			name: "legacy runbook exits non-zero",
			err:  status.Error(codes.Unimplemented, "unknown method"),
			script: func() (*nvkeproto.ExecuteScriptResponse, error) {
				return &nvkeproto.ExecuteScriptResponse{ExitCode: 1, ErrorMessage: "kubeadm failed"}, nil
			},
			wantError:    "failed to execute legacy certificate renewal",
			wantReleased: true,
		},
		{
			name: "legacy runbook RPC fails",
			err:  status.Error(codes.Unimplemented, "unknown method"),
			script: func() (*nvkeproto.ExecuteScriptResponse, error) {
				return nil, status.Error(codes.Unavailable, "connection reset")
			},
			wantError: "failed to execute legacy certificate renewal",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &runbookAdmissionDatabase{agentAdmissionDatabase: &agentAdmissionDatabase{node: node, cluster: db.Cluster{ID: node.ClusterID}}}
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
					require.NotNil(t, database.cluster.CertificateRenewal, "the reservation is held while the RPC runs")
					return test.response, test.err
				},
				ExecuteScriptHook: func(db.Node, string, string) (*nvkeproto.ExecuteScriptResponse, error) {
					return test.script()
				},
			}
			a := &Activities{cfg: config.Config{RunbooksDir: t.TempDir()}, db: database, nodeAgentClient: fakeAgent}

			_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "new-operation", ThresholdDays: 14})

			require.ErrorContains(t, err, test.wantError)
			if test.wantReleased {
				require.Nil(t, database.cluster.CertificateRenewal, "a terminated legacy script must not keep the cluster's renewal reservation")
			} else {
				require.Equal(t, &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "new-operation"}, database.cluster.CertificateRenewal,
					"unconfirmed termination keeps the reservation for the retry to reattach")
			}
		})
	}
}

// runbookAdmissionDatabase also serves the cluster lookup that labels runbook metrics.
type runbookAdmissionDatabase struct {
	*agentAdmissionDatabase
}

func (d *runbookAdmissionDatabase) GetCluster(context.Context, uuid.UUID) (db.Cluster, error) {
	return d.cluster, nil
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

// TestRenewKubernetesCertificatesDoesNotSelectFallbackAfterAmbiguousAttempt models
// a lost acceptance response followed by a capability change. Retrying the same
// typed operation must not authorize a second executor.
func TestRenewKubernetesCertificatesDoesNotSelectFallbackAfterAmbiguousAttempt(t *testing.T) {
	node := pooledCertificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	attempts := 0
	fakeAgent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
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
		phase     nvkeproto.NodeAgentOperationPhase
		outcome   nvkeproto.KubernetesCertificateRenewalOperation_Outcome
		id        string
		missing   bool
		err       error
		wantError bool
	}{
		{name: "succeeded", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED},
		{name: "failed restored", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED},
		{name: "failed not restored", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED},
		{name: "failed before restart", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART},
		{name: "missing", missing: true, wantError: true},
		{name: "mismatched", id: "another-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, wantError: true},
		{name: "unknown phase", id: "stable-operation", phase: 42, wantError: true},
		{name: "malformed succeeded outcome", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, wantError: true},
		{name: "malformed failed outcome", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, wantError: true},
		{name: "not found", err: status.Error(codes.NotFound, "unknown operation"), wantError: true},
		{name: "unimplemented", err: status.Error(codes.Unimplemented, "status unsupported"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode() // releases no cluster reservation
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "stable-operation", req.GetOperationId())
				if tt.missing || tt.err != nil {
					return nil, tt.err
				}
				return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(tt.id, tt.phase, tt.outcome, "").GetOperation()}, nil
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
			node := pooledCertificateRenewalTestNode() // releases no cluster reservation
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			polls := 0
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "stable-operation", req.GetOperationId())
				polls++
				phase, outcome := nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED
				if finish && polls == 2 {
					phase, outcome = nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED
				}
				return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(req.GetOperationId(), phase, outcome, "").GetOperation()}, nil
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

// TestResolveKubernetesCertificateRenewalRetriesTransientStatusErrors keeps an
// agent restart or network blip from retaining the lock: transient status errors
// are retried within the caller's budget, and ownership is retained only when no
// terminal status arrives before the deadline.
func TestResolveKubernetesCertificateRenewalRetriesTransientStatusErrors(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		persistent bool
	}{
		{name: "unavailable then terminal failure", err: status.Error(codes.Unavailable, "agent restarting")},
		{name: "deadline exceeded then terminal failure", err: fmt.Errorf("wrapped: %w", status.Error(codes.DeadlineExceeded, "slow agent"))},
		{name: "transport error then terminal failure", err: errors.New("failed to connect to node agent")},
		{name: "persistently unavailable retains ownership", err: status.Error(codes.Unavailable, "disconnected"), persistent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode() // releases no cluster reservation
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			polls := 0
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "stable-operation", req.GetOperationId())
				polls++
				if tt.persistent || polls == 1 {
					return nil, tt.err
				}
				return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(
					req.GetOperationId(),
					nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
					nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
					"",
				).GetOperation()}, nil
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestActivityEnvironment()
			resolve := func(ctx context.Context) error {
				timeout := 10 * time.Second
				if tt.persistent {
					timeout = 50 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				return a.ResolveKubernetesCertificateRenewal(ctx, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation"})
			}
			env.RegisterActivity(resolve)
			_, err := env.ExecuteActivity(resolve)
			if tt.persistent {
				require.ErrorContains(t, err, "deadline exceeded")
				require.ErrorContains(t, err, "disconnected")
			} else {
				require.NoError(t, err)
				require.Equal(t, 2, polls)
			}
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
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
		{name: "abandoned resolves as terminal failure", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\nRENEWAL_TERMINAL_OUTCOME=abandoned\n"},
		{name: "unknown terminal outcome", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\nRENEWAL_TERMINAL_OUTCOME=renewed\n", wantError: true},
		{name: "missing", wantError: true},
		{name: "normal outcome is insufficient", output: "RENEWAL_OUTCOME=renewed\n", wantError: true},
		{name: "mismatched", output: "RENEWAL_TERMINAL_OPERATION_ID=another-operation\n", wantError: true},
		{name: "malformed", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\nunexpected", wantError: true},
		{name: "unsuccessful script", output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n", exitCode: 1, wantError: true},
		{name: "unreachable", err: errors.New("connection lost"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode() // releases no cluster reservation
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			overrides := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(overrides, legacyKubernetesCertificateRenewalRunbook+".sh"), []byte("unsafe override without status mode"), 0o600))
			fakeAgent := &agenttest.FakeClient{
				// A V1-only agent may support GetStatus yet have no typed record of the legacy operation.
				GetKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return nil, status.Error(codes.NotFound, "no typed operation")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
					require.Contains(t, script, "export STATUS_ONLY='true'")
					require.Contains(t, script, "export OPERATION_ID='stable-operation'")
					require.Contains(t, script, "export LOCK_TIMEOUT='240'")
					require.Contains(t, script, `exec 9<"$LOCK_FILE"`)
					require.NotContains(t, script, "unsafe override")
					return &nvkeproto.ExecuteScriptResponse{Output: tt.output, ExitCode: tt.exitCode}, tt.err
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
// override's success marker from bypassing V2 fencing. Missing or abandoned
// receipt evidence fails without retrying that override and retains its original
// diagnostics.
func TestRenewLegacyKubernetesCertificatesRejectsReceiptUnawareSuccess(t *testing.T) {
	for name, statusResponse := range map[string]*nvkeproto.ExecuteScriptResponse{
		"missing receipt":   {ExitCode: 1, ErrorMessage: "no terminal receipt"},
		"abandoned receipt": {Output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\nRENEWAL_TERMINAL_OUTCOME=abandoned\n"},
	} {
		t.Run(name, func(t *testing.T) {
			testRenewLegacyKubernetesCertificatesRejectsReceiptUnawareSuccess(t, statusResponse)
		})
	}
}

func testRenewLegacyKubernetesCertificatesRejectsReceiptUnawareSuccess(t *testing.T, statusResponse *nvkeproto.ExecuteScriptResponse) {
	t.Helper()
	node := certificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Once()
	overrides := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(overrides, legacyKubernetesCertificateRenewalRunbook+".sh"),
		[]byte("#!/bin/bash\necho RENEWAL_OUTCOME=renewed\n"), 0o600))
	fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(_ db.Node, _, script string) (*nvkeproto.ExecuteScriptResponse, error) {
		if strings.Contains(script, "export STATUS_ONLY='true'") {
			return statusResponse, nil
		}
		return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=renewed\n", ErrorMessage: "original diagnostic"}, nil
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

func TestRenewKubernetesCertificatesNotDispatchedRequiresNoPriorReservation(t *testing.T) {
	for _, reason := range []string{"upgrade", "repair", "recycling", "delete", "another node"} {
		for _, reserved := range []bool{false, true} {
			if reason == "another node" && reserved {
				continue
			}
			t.Run(fmt.Sprintf("%s/reserved=%t", reason, reserved), func(t *testing.T) {
				node := certificateRenewalTestNode()
				database := &agentAdmissionDatabase{cluster: db.Cluster{ID: node.ClusterID}, node: node}
				switch reason {
				case "upgrade":
					database.cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: node.ID}
				case "repair":
					database.node.Conditions.MarkedForRepair = db.NewCondition(true, "")
				case "recycling":
					database.node.Conditions.MarkedForRecycling = db.NewCondition(true, "")
				case "delete":
					database.node.Conditions.DeleteRequested = db.NewCondition(true, "")
				case "another node":
					database.cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: uuid.New(), OperationID: "other"}
				}
				if reserved {
					database.cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "accepted"}
				}
				fake := &agenttest.FakeClient{}
				a := &Activities{db: database, nodeAgentClient: fake}
				_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
					NodeID: node.ID, OperationID: "new", ThresholdDays: 14, ExpectedEtcdMembers: 3,
				})
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				require.Equal(t, !reserved, appErr.Type() == CertificateRenewalNotDispatched)
				if !reserved {
					require.True(t, appErr.NonRetryable())
				}
				require.Empty(t, fake.StartKubernetesCertificateRenewalV2Calls)
				require.Empty(t, fake.StartKubernetesCertificateRenewalCalls)
			})
		}
	}
}

// A lost Start response followed by an admission failure is not first-dispatch
// proof. It must still take the workflow's status-only resolution path.
func TestRenewKubernetesCertificatesRetryAdmissionFailureRemainsUnconfirmed(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &agentAdmissionDatabase{cluster: db.Cluster{ID: node.ClusterID}, node: node}
	fake := &agenttest.FakeClient{StartKubernetesCertificateRenewalV2Hook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
		database.cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: node.ID}
		return nil, status.Error(codes.Unavailable, "Start response lost")
	}}
	a := &Activities{db: database, nodeAgentClient: fake}
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(a.RenewKubernetesCertificates)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2},
		})
		return workflow.ExecuteActivity(ctx, a.RenewKubernetesCertificates, KubernetesCertificateRenewalInput{
			NodeID: node.ID, OperationID: "stable", ThresholdDays: 14, ExpectedEtcdMembers: 3,
		}).Get(ctx, nil)
	})
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &appErr)
	require.NotEqual(t, CertificateRenewalNotDispatched, appErr.Type())
	require.NotNil(t, database.cluster.CertificateRenewal)
	require.Len(t, fake.StartKubernetesCertificateRenewalV2Calls, 1)
}

func TestRenewKubernetesCertificatesPollsAcceptedOperationWithoutRestarting(t *testing.T) {
	node := pooledCertificateRenewalTestNode()
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	polls := 0
	fake := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
			return certificateRenewalStartV2Response(req.GetOperationId(), nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED, ""), nil
		},
		GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
			require.Equal(t, "stable", req.GetOperationId())
			polls++
			phase, outcome := nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED
			if polls == 2 {
				phase, outcome = nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED
			}
			return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(req.GetOperationId(), phase, outcome, "").GetOperation()}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fake}
	result, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
		NodeID: node.ID, OperationID: "stable", ThresholdDays: 14, ExpectedEtcdMembers: 3,
	})
	require.NoError(t, err)
	require.Equal(t, db.CertificateRenewalOutcomeUnchanged, result.Outcome)
	require.Equal(t, 2, polls)
	require.Len(t, fake.StartKubernetesCertificateRenewalV2Calls, 1)
	require.Empty(t, fake.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestCheckKubernetesCertificateRenewalRequiredUsesEmbeddedReadOnlyMode(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *nvkeproto.ExecuteScriptResponse
		rpcErr   error
		want     KubernetesCertificateRenewalCheckResult
		wantErr  bool
	}{
		{name: "unchanged", response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=false\n"}},
		{name: "due", response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=true\n"}, want: KubernetesCertificateRenewalCheckResult{Required: true}},
		{
			name:     "pending legacy intent",
			response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=legacy\n"},
			want:     KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true},
		},
		{
			name:     "pending intent in both executors",
			response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=legacy_then_typed\n"},
			want:     KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true, Typed: true},
		},
		{name: "unknown", response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=unknown\n"}, wantErr: true},
		{name: "normal renewal marker is insufficient", response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=unchanged\n"}, wantErr: true},
		{name: "failed check", response: &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=false\n", ExitCode: 1}, wantErr: true},
		{name: "missing", wantErr: true},
		{name: "unreachable", rpcErr: errors.New("disconnected"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			overrides := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(overrides, legacyKubernetesCertificateRenewalRunbook+".sh"), []byte("unsafe override"), 0o600))
			fake := &agenttest.FakeClient{ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
				require.Equal(t, legacyKubernetesCertificateRenewalRunbook, name)
				require.Contains(t, script, "export CHECK_ONLY='true'")
				require.Contains(t, script, "export THRESHOLD_DAYS='14'")
				require.NotContains(t, script, "unsafe override")
				return test.response, test.rpcErr
			}}
			a := &Activities{cfg: config.Config{RunbooksDir: overrides}, db: database, nodeAgentClient: fake}
			result, err := a.CheckKubernetesCertificateRenewalRequired(t.Context(), KubernetesCertificateRenewalInput{NodeID: node.ID, ThresholdDays: 14})
			require.Equal(t, test.wantErr, err != nil)
			require.Equal(t, test.want, result)
			require.Empty(t, fake.StartKubernetesCertificateRenewalCalls)
			require.Empty(t, fake.StartKubernetesCertificateRenewalV2Calls)
			require.Len(t, fake.ExecuteScriptNames, 1)
			database.AssertExpectations(t)
		})
	}
}

// TestResolveKubernetesCertificateRenewalReleasesMatchingReservation lets a later
// cycle pass admission once the operation's termination is confirmed, without
// clearing another operation's reservation or an unconfirmed one.
func TestResolveKubernetesCertificateRenewalReleasesMatchingReservation(t *testing.T) {
	for _, tt := range []struct {
		name         string
		reservedOp   string
		statusErr    error
		wantError    bool
		wantReleased bool
	}{
		{name: "terminal status releases the matching reservation", reservedOp: "stable-operation", wantReleased: true},
		{name: "terminal status keeps another operation's reservation", reservedOp: "another-operation"},
		{name: "missing status keeps the reservation", reservedOp: "stable-operation", statusErr: status.Error(codes.NotFound, "unknown operation"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			reservation := &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: tt.reservedOp, Fenced: true}
			database := &agentAdmissionDatabase{node: node, cluster: db.Cluster{ID: node.ClusterID, CertificateRenewal: reservation}}
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				if tt.statusErr != nil {
					return nil, tt.statusErr
				}
				return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{Operation: certificateRenewalStartResponse(
					req.GetOperationId(),
					nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
					nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
					"",
				).GetOperation()}, nil
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)

			_, err := env.ExecuteActivity(a.ResolveKubernetesCertificateRenewal, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation"})

			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantReleased {
				require.Nil(t, database.cluster.CertificateRenewal)
			} else {
				require.Equal(t, reservation, database.cluster.CertificateRenewal)
			}
		})
	}
}

// TestReserveCertificateRenewalFencesOnlyV2 marks reservations that a cycle owns
// under the cluster mutation lock, which the reconciler may later clear.
func TestReserveCertificateRenewalFencesOnlyV2(t *testing.T) {
	for _, tt := range []struct {
		name                string
		expectedEtcdMembers int
		wantFenced          bool
	}{
		{name: "V1 reservation is not fenced"},
		{name: "V2 reservation is fenced", expectedEtcdMembers: 3, wantFenced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &agentAdmissionDatabase{node: node, cluster: db.Cluster{ID: node.ClusterID}}
			a := &Activities{db: database}
			input := KubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "new-operation", ThresholdDays: 14, ExpectedEtcdMembers: tt.expectedEtcdMembers}

			require.NoError(t, a.reserveCertificateRenewal(t.Context(), node, &input))

			require.Equal(t, &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "new-operation", Fenced: tt.wantFenced}, database.cluster.CertificateRenewal)
		})
	}
}
