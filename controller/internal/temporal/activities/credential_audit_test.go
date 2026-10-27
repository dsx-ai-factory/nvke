// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type auditTestStore struct {
	events []credentialaudit.Event
	err    error
}

func (s *auditTestStore) RecordCredentialAuditEvent(_ context.Context, event credentialaudit.Event) error {
	s.events = append(s.events, event)
	return s.err
}

func TestCertificateAuditFailurePreservesSuccessAndMutationLockRelease(t *testing.T) {
	for _, outcome := range []db.CertificateRenewalOutcome{db.CertificateRenewalOutcomeRenewed, db.CertificateRenewalOutcomeUnchanged} {
		t.Run(string(outcome), func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Conditions: db.ClusterConditions{
				UpdateInProgress:   &db.UpdateInProgress{Type: db.OperationTypeCertRenewal},
				CertificateRenewal: &db.CertificateRenewalStatus{CycleID: "example-token", State: db.CertificateRenewalStateAccepted},
			}}, nil).Once()
			tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
				return cluster.Conditions.UpdateInProgress == nil && cluster.Conditions.CertificateRenewal.State == db.CertificateRenewalStateSucceeded
			})).Return(db.Cluster{}, nil).Once()
			store := &auditTestStore{err: errors.New("sink failed with example-token")}
			var observations []string
			a := &Activities{
				db: database, cfg: config.Config{CertRenewalInterval: time.Hour},
				credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), func(result string) {
					observations = append(observations, result)
				}),
			}
			_, err := a.CompleteCertificateRenewal(t.Context(), CompleteCertificateRenewalInput{
				ClusterID: clusterID, CycleID: "example-token", Outcome: outcome,
			})
			require.NoError(t, err)
			require.Equal(t, []string{credentialaudit.WriteFailed}, observations)
			require.Len(t, store.events, 1)
			event := store.events[0]
			if outcome == db.CertificateRenewalOutcomeUnchanged {
				require.Equal(t, credentialaudit.OutcomeNoop, event.Outcome)
			} else {
				require.Equal(t, credentialaudit.OutcomeSuccess, event.Outcome)
			}
			encoded, err := json.Marshal(event)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "example-token")
			database.AssertExpectations(t)
			tx.AssertExpectations(t)
		})
	}
}

func TestCredentialAuditCapturesActivityCorrelationWithoutWorkflowIO(t *testing.T) {
	store := &auditTestStore{}
	a := &Activities{credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil)}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	activityFunction := func(ctx context.Context, clusterID uuid.UUID) error {
		a.recordCredentialAudit(ctx, credentialOperationAudit(clusterID, nil, credentialaudit.ActionCheck,
			credentialaudit.TypeKubeadm, credentialaudit.OutcomeSuccess, credentialaudit.ReasonAccepted), "example-operation")
		return nil
	}
	env.RegisterActivity(activityFunction)
	_, err := env.ExecuteActivity(activityFunction, uuid.New())
	require.NoError(t, err)
	require.Len(t, store.events, 1)
	require.NotEmpty(t, store.events[0].Correlation.WorkflowHash)
	require.NotEmpty(t, store.events[0].Correlation.RunHash)
	require.NotEmpty(t, store.events[0].Correlation.ActivityHash)
	require.EqualValues(t, 1, store.events[0].Correlation.Attempt)
}

func TestSchedulerAttributionIsRestrictedToKnownCycleShape(t *testing.T) {
	clusterID := uuid.NewString()
	prefix := "renew-k8s-certs-" + clusterID + "-"
	for _, cycle := range []string{"initial", "1791158400000000000", "initial-retry-1791158400000000000", "1791158400000000000-retry-1791162000000000000"} {
		require.True(t, scheduledCredentialAuditWorkflow(prefix+cycle, clusterID))
	}
	for _, candidate := range []string{prefix + "force", "operator-renewal", prefix + "example-token", prefix + "initial-retry-invalid", prefix + "force-retry-123", prefix + "initial-retry-123-retry-456"} {
		require.False(t, scheduledCredentialAuditWorkflow(candidate, clusterID))
	}
}

func TestCertificateRenewalNotDispatchedAuditsPersistedFailure(t *testing.T) {
	node := certificateRenewalTestNode()
	database := &agentAdmissionDatabase{node: node, cluster: db.Cluster{
		ID: node.ClusterID, NodeAgentUpgrade: &db.NodeAgentUpgrade{NodeID: node.ID},
		Conditions: db.ClusterConditions{
			UpdateInProgress:   &db.UpdateInProgress{Type: db.OperationTypeCertRenewal},
			CertificateRenewal: &db.CertificateRenewalStatus{CycleID: "cycle", State: db.CertificateRenewalStateAccepted},
		},
	}}
	store := &auditTestStore{}
	a := &Activities{db: database, nodeAgentClient: &agenttest.FakeClient{}, credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil)}
	_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
		NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
	})
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, CertificateRenewalNotDispatched, appErr.Type())
	require.Empty(t, store.events, "no executor outcome exists before dispatch")
	_, err = a.FailCertificateRenewal(t.Context(), FailCertificateRenewalInput{
		ClusterID: node.ClusterID, CycleID: "cycle", FailedNodeID: &node.ID, RetainMutationLock: false,
	})
	require.NoError(t, err)
	require.Nil(t, database.cluster.Conditions.UpdateInProgress)
	require.Equal(t, db.CertificateRenewalStateFailed, database.cluster.Conditions.CertificateRenewal.State)
	require.Len(t, store.events, 1)
	require.Equal(t, credentialaudit.ActionRenew, store.events[0].Action)
	require.Equal(t, credentialaudit.OutcomeFailure, store.events[0].Outcome)
	require.Equal(t, credentialaudit.Hash("operation", "cycle"), store.events[0].Correlation.OperationHash)
}

func TestCertificateExpiryCheckAuditsOnlyUnchangedNodes(t *testing.T) {
	for _, tt := range []struct {
		name         string
		output       string
		exitCode     int32
		wantRequired bool
		wantErr      bool
		wantAudit    bool
	}{
		{name: "unchanged", output: "RENEWAL_REQUIRED=false\n", wantAudit: true},
		{name: "unchanged with diagnostics", output: "diagnostic\nRENEWAL_REQUIRED=false\nwarning\n", wantAudit: true},
		{name: "due", output: "RENEWAL_REQUIRED=true\n", wantRequired: true},
		{name: "typed intent with diagnostics", output: "diagnostic\nRENEWAL_REQUIRED=typed\n", wantRequired: true},
		{name: "conflicting markers", output: "RENEWAL_REQUIRED=false\nRENEWAL_REQUIRED=typed\n", wantErr: true},
		{name: "failed", output: "RENEWAL_REQUIRED=false\n", exitCode: 1, wantErr: true},
		{name: "malformed", output: "REDACTED", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := certificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			store := &auditTestStore{}
			fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(db.Node, string, string) (*nvkeproto.ExecuteScriptResponse, error) {
				return &nvkeproto.ExecuteScriptResponse{Output: tt.output, ExitCode: tt.exitCode}, nil
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent, credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil)}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.CheckKubernetesCertificateRenewalRequired)
			value, err := env.ExecuteActivity(a.CheckKubernetesCertificateRenewalRequired, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
			})
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				var result KubernetesCertificateRenewalCheckResult
				require.NoError(t, value.Get(&result))
				require.Equal(t, tt.wantRequired, result.Required)
			}
			if tt.wantAudit {
				require.Len(t, store.events, 1)
				event := store.events[0]
				require.Equal(t, credentialaudit.ActionRenew, event.Action)
				require.Equal(t, credentialaudit.OutcomeNoop, event.Outcome)
				require.Equal(t, credentialaudit.ReasonUnchanged, event.Reason)
				require.Equal(t, node.ID.String(), event.Target.NodeID)
				require.Equal(t, credentialaudit.Hash("operation", "stable-operation"), event.Correlation.OperationHash)
				require.NotEmpty(t, event.Correlation.WorkflowHash)
			} else {
				require.Empty(t, store.events)
			}
			database.AssertExpectations(t)
		})
	}
}

func TestLegacyCertificateAuditPersistsFailedAttemptBeforeSuccessfulRetry(t *testing.T) {
	node := pooledCertificateRenewalTestNode() // admits without a cluster reservation
	database := &testutil.MockDB{}
	// Each attempt loads the node for admission and the runbook; the successful
	// one also loads it for receipt resolution and the audit.
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Times(6)
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Twice()
	store := &auditTestStore{}
	var observations []string
	attempts := 0
	fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(_ db.Node, _, script string) (*nvkeproto.ExecuteScriptResponse, error) {
		if strings.Contains(script, "export STATUS_ONLY='true'") {
			return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_TERMINAL_OPERATION_ID=stable-operation\n"}, nil
		}
		attempts++
		if attempts == 1 {
			return &nvkeproto.ExecuteScriptResponse{ExitCode: 1, Output: "example-token", ErrorMessage: "REDACTED"}, nil
		}
		return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=renewed\n"}, nil
	}}
	a := &Activities{
		db: database, nodeAgentClient: fakeAgent, cfg: config.Config{RunbooksDir: t.TempDir()},
		credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), func(result string) {
			observations = append(observations, result)
		}),
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(a.RenewLegacyKubernetesCertificates)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 75 * time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 30 * time.Second, MaximumAttempts: 3},
		})
		var result KubernetesCertificateRenewalResult
		if err := workflow.ExecuteActivity(ctx, a.RenewLegacyKubernetesCertificates, KubernetesCertificateRenewalInput{
			NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
		}).Get(ctx, &result); err != nil {
			return err
		}
		require.Equal(t, db.CertificateRenewalOutcomeRenewed, result.Outcome)
		return nil
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 2, attempts)
	require.Equal(t, []string{credentialaudit.WriteStored, credentialaudit.WriteStored}, observations)
	require.Len(t, store.events, 2)
	failed, succeeded := store.events[0], store.events[1]
	require.Equal(t, credentialaudit.OutcomeFailure, failed.Outcome)
	require.Equal(t, credentialaudit.ReasonFailed, failed.Reason)
	require.EqualValues(t, 1, failed.Correlation.Attempt)
	require.Equal(t, credentialaudit.OutcomeSuccess, succeeded.Outcome)
	require.Equal(t, credentialaudit.ReasonCompleted, succeeded.Reason)
	require.EqualValues(t, 2, succeeded.Correlation.Attempt)
	require.NotEqual(t, failed.ID, succeeded.ID)
	require.Equal(t, failed.Correlation.OperationHash, succeeded.Correlation.OperationHash)
	require.Equal(t, failed.Correlation.WorkflowHash, succeeded.Correlation.WorkflowHash)
	require.Equal(t, failed.Correlation.RunHash, succeeded.Correlation.RunHash)
	require.Equal(t, failed.Correlation.ActivityHash, succeeded.Correlation.ActivityHash)
	for _, event := range store.events {
		require.Equal(t, credentialaudit.ActionRenew, event.Action)
		require.Equal(t, node.ID.String(), event.Target.NodeID)
		require.Equal(t, credentialaudit.Hash("operation", "stable-operation"), event.Correlation.OperationHash)
		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "example-token")
		require.NotContains(t, string(encoded), "REDACTED")
	}
	database.AssertExpectations(t)
}

func TestLegacyCertificateAuditDoesNotAssignOutcomeToTransportFailure(t *testing.T) {
	for _, agentErr := range []error{context.Canceled, status.Error(codes.Unavailable, "example-token")} {
		t.Run(status.Code(agentErr).String(), func(t *testing.T) {
			node := pooledCertificateRenewalTestNode() // admits without a cluster reservation
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
			database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Once()
			store := &auditTestStore{}
			fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(db.Node, string, string) (*nvkeproto.ExecuteScriptResponse, error) {
				return nil, agentErr
			}}
			a := &Activities{
				db: database, nodeAgentClient: fakeAgent, cfg: config.Config{RunbooksDir: t.TempDir()},
				credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil),
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.RenewLegacyKubernetesCertificates)
			_, err := env.ExecuteActivity(a.RenewLegacyKubernetesCertificates, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
			})
			require.Error(t, err)
			require.Empty(t, store.events, "transport failure cannot establish a credential mutation outcome")
			database.AssertExpectations(t)
		})
	}
}

func TestCertificateResolutionAuditPreservesActualTerminalOutcome(t *testing.T) {
	tests := []struct {
		name        string
		phase       nvkeproto.NodeAgentOperationPhase
		outcome     nvkeproto.KubernetesCertificateRenewalOperation_Outcome
		wantOutcome string
		wantReason  string
	}{
		{
			"renewed", nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, credentialaudit.OutcomeSuccess, credentialaudit.ReasonCompleted,
		},
		{
			"unchanged", nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, credentialaudit.OutcomeNoop, credentialaudit.ReasonUnchanged,
		},
		{
			"failed before restart", nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART, credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed,
		},
		{
			"failed restored", nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED, credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed,
		},
		{
			"failed not restored", nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED, credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, sinkFailure := range []bool{false, true} {
				node := pooledCertificateRenewalTestNode() // releases no cluster reservation
				database := &testutil.MockDB{}
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
				store := &auditTestStore{}
				wantWrite := credentialaudit.WriteStored
				if sinkFailure {
					store.err = errors.New("example-token")
					wantWrite = credentialaudit.WriteFailed
				}
				var observations []string
				fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, request *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{
						Operation: certificateRenewalStartResponse(request.GetOperationId(), tt.phase, tt.outcome, "example-token").GetOperation(),
					}, nil
				}}
				a := &Activities{
					db: database, nodeAgentClient: fakeAgent,
					credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), func(result string) {
						observations = append(observations, result)
					}),
				}
				var suite testsuite.WorkflowTestSuite
				env := suite.NewTestActivityEnvironment()
				env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)
				_, err := env.ExecuteActivity(a.ResolveKubernetesCertificateRenewal, ResolveKubernetesCertificateRenewalInput{
					NodeID: node.ID, OperationID: "stable-operation",
				})
				require.NoError(t, err, "terminal failure still resolves ownership, including when the audit sink fails")
				require.Equal(t, []string{wantWrite}, observations)
				require.Len(t, store.events, 1)
				event := store.events[0]
				require.Equal(t, tt.wantOutcome, event.Outcome)
				require.Equal(t, tt.wantReason, event.Reason)
				require.Equal(t, credentialaudit.ActionRenew, event.Action)
				require.Equal(t, node.ID.String(), event.Target.NodeID)
				require.Equal(t, credentialaudit.Hash("operation", "stable-operation"), event.Correlation.OperationHash)
				encoded, err := json.Marshal(event)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "example-token")
				database.AssertExpectations(t)
			}
		})
	}
}

func TestCertificateResolutionAuditRejectsUnconfirmedOutcome(t *testing.T) {
	for _, tt := range []struct {
		name     string
		id       string
		phase    nvkeproto.NodeAgentOperationPhase
		outcome  nvkeproto.KubernetesCertificateRenewalOperation_Outcome
		agentErr error
	}{
		{
			name: "mismatched operation", id: "another-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			outcome: nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED,
		},
		{name: "malformed terminal outcome", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED},
		{name: "malformed failed outcome", id: "stable-operation", phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED},
		{name: "transport failure", agentErr: status.Error(codes.Unavailable, "example-token")},
		{name: "cancellation", agentErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			store := &auditTestStore{}
			fakeAgent := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
				return &nvkeproto.GetKubernetesCertificateRenewalStatusResponse{
					Operation: certificateRenewalStartResponse(tt.id, tt.phase, tt.outcome, "").GetOperation(),
				}, tt.agentErr
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent, credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil)}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			// Transient status errors are retried until the caller's deadline.
			resolve := func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				return a.ResolveKubernetesCertificateRenewal(ctx, ResolveKubernetesCertificateRenewalInput{NodeID: node.ID, OperationID: "stable-operation"})
			}
			env.RegisterActivity(resolve)
			_, err := env.ExecuteActivity(resolve)
			require.Error(t, err)
			require.Empty(t, store.events)
			database.AssertExpectations(t)
		})
	}
}

func TestTypedCertificateRenewalAuditsOnlyConfirmedTerminalOutcomes(t *testing.T) {
	succeeded := nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	failed := nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED
	for _, tt := range []struct {
		name        string
		operation   *nvkeproto.KubernetesCertificateRenewalOperation
		wantOutcome string
	}{
		{name: "empty status", operation: &nvkeproto.KubernetesCertificateRenewalOperation{}},
		{
			name: "unspecified phase", operation: certificateRenewalStartResponse("stable-operation",
				nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED, 0, "").GetOperation(),
		},
		{name: "unknown phase", operation: certificateRenewalStartResponse("stable-operation", 99, 0, "").GetOperation()},
		{
			name: "succeeded with unspecified outcome", operation: certificateRenewalStartResponse("stable-operation", succeeded,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED, "").GetOperation(),
		},
		{
			name: "failed with unspecified outcome", operation: certificateRenewalStartResponse("stable-operation", failed,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED, "").GetOperation(),
		},
		{
			name: "failed restored", operation: certificateRenewalStartResponse("stable-operation", failed,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED, "").GetOperation(),
			wantOutcome: credentialaudit.OutcomeFailure,
		},
		{
			name: "succeeded renewed", operation: certificateRenewalStartResponse("stable-operation", succeeded,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, "").GetOperation(),
			wantOutcome: credentialaudit.OutcomeSuccess,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := pooledCertificateRenewalTestNode()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
					return &nvkeproto.StartKubernetesCertificateRenewalV2Response{Operation: tt.operation}, nil
				},
			}
			store := &auditTestStore{}
			a := &Activities{db: database, nodeAgentClient: fakeAgent, credentialAudit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil)}
			_, err := executeCertificateRenewalActivity(t, a, KubernetesCertificateRenewalInput{
				NodeID: node.ID, OperationID: "stable-operation", ThresholdDays: 14, ExpectedEtcdMembers: 3,
			})
			if tt.wantOutcome == credentialaudit.OutcomeSuccess {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if tt.wantOutcome == "" {
				require.Empty(t, store.events)
				return
			}
			require.Len(t, store.events, 1)
			require.Equal(t, credentialaudit.ActionRenew, store.events[0].Action)
			require.Equal(t, tt.wantOutcome, store.events[0].Outcome)
		})
	}
}
