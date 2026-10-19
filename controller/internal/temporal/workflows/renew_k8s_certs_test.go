// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestRenewK8sCertsSuccessIsSequentialAndDeterministic(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnv(t)
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).
		Return([]db.Node{nodeB, nodeA}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()

	var executionOrder []uuid.UUID
	var operationIDs []string
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID && !input.Force &&
			input.ThresholdDays == 14 && input.ExpectedEtcdMembers == 2
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		executionOrder = append(executionOrder, input.NodeID)
		operationIDs = append(operationIDs, input.OperationID)
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeB.ID
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		executionOrder = append(executionOrder, input.NodeID)
		operationIDs = append(operationIDs, input.OperationID)
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil
	}).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeB.ID).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.CompleteCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && input.CycleID == cycleID() && input.Outcome == db.CertificateRenewalOutcomeRenewed
	})).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RenewK8sCertsResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, db.CertificateRenewalOutcomeRenewed, result.Outcome)
	require.Equal(t, []uuid.UUID{nodeA.ID, nodeB.ID}, executionOrder)
	require.Len(t, operationIDs, 2)
	require.NotEqual(t, operationIDs[0], operationIDs[1])
	env.AssertExpectations(t)
}

// TestRenewK8sCertsSingleControlPlaneRenewsWithOneExpectedVoter covers the N=1
// cycle: one node is renewed with one expected etcd member between health gates.
func TestRenewK8sCertsSingleControlPlaneRenewsWithOneExpectedVoter(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	env, a := newCertificateRenewalWorkflowEnv(t)
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.ExpectedEtcdMembers == 1
	})).Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.CompleteCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && input.CycleID == cycleID() && input.Outcome == db.CertificateRenewalOutcomeRenewed
	})).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

// TestRenewK8sCertsCancellationWaitsForAcceptedCycle models a committed lock whose
// activity response is delayed. Cancellation must resolve acceptance and clean up
// that ownership without dispatching node work.
func TestRenewK8sCertsCancellationWaitsForAcceptedCycle(t *testing.T) {
	clusterID := uuid.New()
	env, a := newCertificateRenewalWorkflowEnv(t)
	var acceptedCycleID string
	var beginTaskToken []byte
	env.OnActivity(a.BeginCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.BeginCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && !input.Forced
	})).Return(func(ctx context.Context, input activities.BeginCertificateRenewalInput) (activities.BeginCertificateRenewalResult, error) {
		// Acceptance and its mutation lock have committed, but the workflow has not received the result.
		acceptedCycleID = input.CycleID
		beginTaskToken = activity.GetInfo(ctx).TaskToken
		return activities.BeginCertificateRenewalResult{}, activity.ErrResultPending
	}).Once()
	env.RegisterDelayedCallback(func() {
		require.NotEmpty(t, acceptedCycleID)
		env.CancelWorkflow()
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		require.False(t, env.IsWorkflowCompleted(), "cancellation must wait for acceptance to resolve")
		require.NoError(t, env.CompleteActivity(beginTaskToken, activities.BeginCertificateRenewalResult{}, nil))
	}, 2*time.Second)
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && input.CycleID == acceptedCycleID && input.FailedNodeID == nil &&
			!input.RetainMutationLock && input.Phase == "preflight" && strings.Contains(input.Message, "canceled")
	})).Return(func(ctx context.Context, _ activities.FailCertificateRenewalInput) (activities.FailCertificateRenewalResult, error) {
		require.NoError(t, ctx.Err(), "failure cleanup must not inherit workflow cancellation")
		return activities.FailCertificateRenewalResult{}, nil
	}).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.True(t, env.IsWorkflowCompleted())
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()), "workflow must return the original cancellation")
	env.AssertNotCalled(t, "ListControlPlaneNodes", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ValidateCertificateRenewalHealth", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsRetriesSameNodeBeforeContinuing preserves the operation ID on
// retry and keeps later nodes behind the retried node's success and health gate.
func TestRenewK8sCertsRetriesSameNodeBeforeContinuing(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, true)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()

	var operationIDs []string
	var order []uuid.UUID
	firstAttempt := env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID && input.Force
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		operationIDs = append(operationIDs, input.OperationID)
		order = append(order, input.NodeID)
		return activities.KubernetesCertificateRenewalResult{}, errors.New("transient agent failure")
	}).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		operationIDs = append(operationIDs, input.OperationID)
		order = append(order, input.NodeID)
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once().NotBefore(firstAttempt)
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeB.ID
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		order = append(order, input.NodeID)
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeB.ID).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).
		Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, Force: true})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []uuid.UUID{nodeA.ID, nodeA.ID, nodeB.ID}, order)
	require.Len(t, operationIDs, 2)
	require.Equal(t, operationIDs[0], operationIDs[1], "activity retries must reuse the node operation ID")
	env.AssertExpectations(t)
}

func TestRenewK8sCertsStopsAfterNodeFailure(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnv(t)
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()

	attempts := 0
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID
	})).Return(func(context.Context, activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		attempts++
		return activities.KubernetesCertificateRenewalResult{}, errors.New("node renewal failed")
	}).Times(3)
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).
		Return(errors.New("etcd remains unhealthy")).Times(3)
	env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.Anything).
		Return(errors.New("node still running")).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && input.CycleID == cycleID() && input.FailedNodeID != nil &&
			*input.FailedNodeID == nodeA.ID && input.Phase == "renew-node" &&
			strings.Contains(input.Message, "node renewal failed") && strings.Contains(input.Message, "etcd remains unhealthy")
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "node renewal failed")
	require.Equal(t, 3, attempts)
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeB.ID
	}))
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsPostflightFailureStopsBeforeNextNode ensures a successful node
// result alone cannot advance rollout. Since that work is terminal, a failed
// health gate records failure without retaining uncertain-operation ownership.
func TestRenewK8sCertsPostflightFailureStopsBeforeNextNode(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID
	})).Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).
		Return(errors.New("etcd unhealthy")).Times(3)
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.FailedNodeID != nil && *input.FailedNodeID == nodeA.ID && input.Phase == "postflight" && !input.RetainMutationLock
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "etcd unhealthy")
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeB.ID
	}))
	env.AssertExpectations(t)
}

func TestRenewK8sCertsMalformedOutcomeRunsDiagnosticPostflightWithoutRetry(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeA.ID
	})).Return(activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError(
		"invalid legacy certificate renewal result", "InvalidCertificateRenewalOutcome", errors.New("renewal outcome marker is missing"),
	)).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).Once()
	env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.Anything).
		Return(errors.New("no matching terminal evidence")).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.FailedNodeID != nil && *input.FailedNodeID == nodeA.ID &&
			input.Phase == "renew-node" && strings.Contains(input.Message, "outcome marker is missing")
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "outcome marker is missing")
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == nodeB.ID
	}))
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsUsesTypedActivitySequentially protects the original V1 activity
// inputs, ordering and result shape separately from V2's ownership protocol.
func TestRenewK8sCertsUsesTypedActivitySequentially(t *testing.T) {
	clusterID := uuid.New()
	nodes := []db.Node{
		{ID: uuid.New(), Name: "cp-0", ClusterID: clusterID, ControlPlane: true},
		{ID: uuid.New(), Name: "cp-1", ClusterID: clusterID, ControlPlane: true},
	}
	env, a := newCertificateRenewalWorkflowEnv(t)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return(nodes, nil).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{Output: "typed success"}, nil).
		Twice()

	var inputs []activities.KubernetesCertificateRenewalInput
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, values converter.EncodedValues) {
		if info.ActivityType.Name != "RenewKubernetesCertificates" {
			return
		}
		var input activities.KubernetesCertificateRenewalInput
		require.NoError(t, values.Get(&input))
		inputs = append(inputs, input)
	})

	env.ExecuteWorkflow(RenewK8sCerts, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 7})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, inputs, 2)
	for index, input := range inputs {
		require.Equal(t, nodes[index].ID, input.NodeID)
		require.Equal(t, 7, input.ThresholdDays)
		require.True(t, strings.HasPrefix(input.OperationID, "k8s-cert-renewal/"))
		require.True(t, strings.HasSuffix(input.OperationID, "/"+nodes[index].ID.String()))
	}
	require.NotEqual(t, inputs[0].OperationID, inputs[1].OperationID)

	var result RenewK8sCertsResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, []RenewK8sCertsNodeResult{
		{NodeID: nodes[0].ID, NodeName: nodes[0].Name, Output: "typed success"},
		{NodeID: nodes[1].ID, NodeName: nodes[1].Name, Output: "typed success"},
	}, result.Results)
	env.AssertExpectations(t)
}

func TestRenewK8sCertsZeroControlPlaneNodesFails(t *testing.T) {
	clusterID := uuid.New()
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node(nil), nil).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.FailedNodeID == nil && input.Phase == "preflight"
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "no control-plane nodes")
	env.AssertExpectations(t)
}

// newCertificateRenewalWorkflowEnv accepts every kube-vip guard activity so tests
// that are not about the drain need not mock it.
func newCertificateRenewalWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{}).Maybe()
	return env, a
}

func newCertificateRenewalWorkflowEnvWithoutGuardMocks(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	a := &activities.Activities{}
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalCheckResult{Required: true}, nil).Maybe()
	return env, a
}

// kubeVipGuardEvents records guard and renewal activity order for one workflow run.
type kubeVipGuardEvents struct {
	mu      sync.Mutex
	entries []string
	// postApply holds the requests PostApply received, in order.
	postApply []*rollouttemporal.NodeGuardPostApplyRequest
}

func (e *kubeVipGuardEvents) add(entry string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.entries = append(e.entries, entry)
}

func (e *kubeVipGuardEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.entries...)
}

// kubeVipGuardBehavior selects which guard activity fails. The zero value accepts everything.
type kubeVipGuardBehavior struct {
	checkPreApplyErr  string
	postApplyErr      error
	statelessNoChecks bool
}

// expectKubeVipGuard mocks the four rollout node-guard activities. It returns the
// last registered call so callers can relax it with Maybe.
func expectKubeVipGuard(env *testsuite.TestWorkflowEnvironment, events *kubeVipGuardEvents, behavior kubeVipGuardBehavior) *testsuite.MockCallWrapper {
	env.OnActivity(rollouttemporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *rollouttemporal.NodeGuardPreApplyRequest) (*rollouttemporal.NodeGuardPreApplyResponse, error) {
			events.add("drain-prepared " + req.NodeID.String())
			state := rollout.GuardState{GuardName: req.GuardName, Payload: []byte(`{"node":"` + req.NodeID.String() + `"}`)}
			if behavior.statelessNoChecks {
				return &rollouttemporal.NodeGuardPreApplyResponse{State: rollout.StatelessGuardState(req.GuardName)}, nil
			}
			return &rollouttemporal.NodeGuardPreApplyResponse{State: state, CheckPreApplyRequired: true, CheckPostApplyRequired: true}, nil
		}).Maybe()
	env.OnActivity(rollouttemporal.ActivityNodeGuardCheckPreApply, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *rollouttemporal.NodeGuardCheckPreApplyRequest) (*rollouttemporal.NodeGuardCheckPreApplyResponse, error) {
			events.add("drain " + req.NodeID.String())
			return &rollouttemporal.NodeGuardCheckPreApplyResponse{Done: behavior.checkPreApplyErr == "", CheckErr: behavior.checkPreApplyErr}, nil
		}).Maybe()
	env.OnActivity(rollouttemporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(func(ctx context.Context, req *rollouttemporal.NodeGuardPostApplyRequest) error {
			events.add("restore " + req.NodeID.String())
			events.mu.Lock()
			events.postApply = append(events.postApply, req)
			events.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return behavior.postApplyErr
		}).Maybe()
	return env.OnActivity(rollouttemporal.ActivityNodeGuardCheckPostApply, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *rollouttemporal.NodeGuardCheckPostApplyRequest) (*rollouttemporal.NodeGuardCheckPostApplyResponse, error) {
			events.add("restored " + req.NodeID.String())
			return &rollouttemporal.NodeGuardCheckPostApplyResponse{Done: true}, nil
		}).Maybe()
}

func certificateRenewalWorkflowNode(clusterID uuid.UUID, name string) db.Node {
	return db.Node{ID: uuid.New(), ClusterID: clusterID, Name: name, ControlPlane: true}
}

func expectCertificateRenewalBegin(
	t *testing.T,
	env *testsuite.TestWorkflowEnvironment,
	a *activities.Activities,
	clusterID uuid.UUID,
	forced bool,
) func() string {
	t.Helper()
	cycleID := ""
	env.OnActivity(a.BeginCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.BeginCertificateRenewalInput) bool {
		return input.ClusterID == clusterID && input.Forced == forced
	})).Return(func(_ context.Context, input activities.BeginCertificateRenewalInput) (activities.BeginCertificateRenewalResult, error) {
		cycleID = input.CycleID
		return activities.BeginCertificateRenewalResult{}, nil
	}).Once()
	return func() string { return cycleID }
}

func expectCertificateRenewalHealth(
	t *testing.T,
	env *testsuite.TestWorkflowEnvironment,
	a *activities.Activities,
	clusterID uuid.UUID,
	expectedNodeIDs []uuid.UUID,
	postflightNodeID *uuid.UUID,
) *testsuite.MockCallWrapper {
	t.Helper()
	return env.OnActivity(a.ValidateCertificateRenewalHealth, mock.Anything, mock.MatchedBy(func(input activities.ValidateCertificateRenewalHealthInput) bool {
		if input.ClusterID != clusterID || !equalUUIDSlices(input.ExpectedNodeIDs, expectedNodeIDs) {
			return false
		}
		if postflightNodeID == nil || input.PostflightNodeID == nil {
			return postflightNodeID == nil && input.PostflightNodeID == nil
		}
		return *input.PostflightNodeID == *postflightNodeID
	})).Return(nil)
}

func equalUUIDSlices(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestRenewK8sCertsFailureResolvesOwnershipDespiteHealthyDiagnostics rejects health
// as a substitute for terminal evidence. Both executors require bounded cleanup
// independent of cancellation, with ownership retained when resolution fails.
func TestRenewK8sCertsFailureResolvesOwnershipDespiteHealthyDiagnostics(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			for _, resolution := range []string{"terminal", "unreachable", "timeout"} {
				t.Run(fmt.Sprintf("legacy=%t/canceled=%t/%s", legacy, canceled, resolution), func(t *testing.T) {
					clusterID := uuid.New()
					node := certificateRenewalWorkflowNode(clusterID, "cp-a")
					env, a := newCertificateRenewalWorkflowEnv(t)
					cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
					env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
					expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
					expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
					renew := a.RenewKubernetesCertificates
					if legacy {
						env.OnActivity(renew, mock.Anything, mock.Anything).Return(activities.KubernetesCertificateRenewalResult{},
							temporal.NewNonRetryableApplicationError("unsupported", activities.CertificateRenewalLegacyRequired, nil)).Once()
						renew = a.RenewLegacyKubernetesCertificates
					}
					var dispatchedOperationID string
					env.OnActivity(renew, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
						dispatchedOperationID = input.OperationID
						if canceled {
							return activities.KubernetesCertificateRenewalResult{}, activity.ErrResultPending
						}
						return activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError("lost connection to running work", "TestFailure", nil)
					}).Once()
					if canceled {
						env.RegisterDelayedCallback(func() { require.NotEmpty(t, dispatchedOperationID); env.CancelWorkflow() }, time.Second)
					}
					env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.ResolveKubernetesCertificateRenewalInput) bool {
						return input.NodeID == node.ID && input.OperationID == dispatchedOperationID &&
							input.OperationID == certificateRenewalOperationID(cycleID(), node.ID) && input.Legacy == legacy
					})).Return(func(ctx context.Context, _ activities.ResolveKubernetesCertificateRenewalInput) error {
						require.NoError(t, ctx.Err(), "resolution must be disconnected from cancellation")
						info := activity.GetInfo(ctx)
						require.LessOrEqual(t, info.Deadline.Sub(info.ScheduledTime), 5*time.Minute)
						switch resolution {
						case "terminal":
							return nil
						case "timeout":
							return temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)
						default:
							return errors.New("remote work may still be running")
						}
					}).Once()
					env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.Anything).Return(func(ctx context.Context, input activities.FailCertificateRenewalInput) (activities.FailCertificateRenewalResult, error) {
						require.NoError(t, ctx.Err())
						require.Equal(t, resolution != "terminal", input.RetainMutationLock)
						require.Equal(t, cycleID(), input.CycleID)
						require.Equal(t, &node.ID, input.FailedNodeID)
						if resolution != "terminal" {
							require.Contains(t, input.Message, "cluster mutation lock retained")
							require.Contains(t, input.Message, dispatchedOperationID)
						}
						return activities.FailCertificateRenewalResult{}, nil
					}).Once()

					env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

					require.Error(t, env.GetWorkflowError())
					if canceled {
						require.True(t, temporal.IsCanceledError(env.GetWorkflowError()))
					}
					env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
					env.AssertExpectations(t)
				})
			}
		}
	}
}

// TestRenewK8sCertsCancellationAroundFallbackSelectionRetainsAmbiguousOwnership
// covers cancellation racing executor selection and dispatch. Cleanup must follow
// the workflow's recorded executor even when no activity completion was observed.
func TestRenewK8sCertsCancellationAroundFallbackSelectionRetainsAmbiguousOwnership(t *testing.T) {
	for _, selectionDelivered := range []bool{false, true} {
		t.Run(fmt.Sprintf("selection-delivered=%t", selectionDelivered), func(t *testing.T) {
			clusterID := uuid.New()
			node := certificateRenewalWorkflowNode(clusterID, "cp-a")
			env, a := newCertificateRenewalWorkflowEnv(t)
			expectCertificateRenewalBegin(t, env, a, clusterID, false)
			env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
			env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).Return(func(context.Context, activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
				if !selectionDelivered {
					return activities.KubernetesCertificateRenewalResult{}, activity.ErrResultPending
				}
				return activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError("unsupported", activities.CertificateRenewalLegacyRequired, nil)
			}).Once()
			if selectionDelivered {
				env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, _ error) {
					if info.ActivityType.Name == "RenewKubernetesCertificates" {
						env.CancelWorkflow()
					}
				})
			} else {
				env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
			}
			// If selection was delivered first, dispatch can race cancellation.
			// Resolve only the executor recorded by that dispatch, never guess.
			env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.ResolveKubernetesCertificateRenewalInput) bool {
				return input.Legacy == selectionDelivered && input.NodeID == node.ID
			})).Return(errors.New("termination unknown")).Once()
			env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.FailCertificateRenewalInput) (activities.FailCertificateRenewalResult, error) {
				require.True(t, input.RetainMutationLock)
				return activities.FailCertificateRenewalResult{}, nil
			}).Once()

			env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

			require.True(t, temporal.IsCanceledError(env.GetWorkflowError()))
			env.AssertNotCalled(t, "RenewLegacyKubernetesCertificates", mock.Anything, mock.Anything)
			env.AssertExpectations(t)
		})
	}
}

// TestRenewK8sCertsLegacySuccessDoesNotChangeExecutorsOnRetry models a lost legacy
// response: retry must keep the selected executor and inputs, not probe typed
// renewal again after the runbook may already have mutated the node.
func TestRenewK8sCertsLegacySuccessDoesNotChangeExecutorsOnRetry(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
	var selectedInput activities.KubernetesCertificateRenewalInput
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		selectedInput = input
		return activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError("unsupported", activities.CertificateRenewalLegacyRequired, nil)
	}).Once()
	attempts := 0
	env.OnActivity(a.RenewLegacyKubernetesCertificates, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		require.Equal(t, selectedInput, input)
		attempts++
		if attempts == 1 {
			return activities.KubernetesCertificateRenewalResult{}, errors.New("response lost")
		}
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil
	}).Twice()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.NoError(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsBracketsEachNodeWithKubeVipDrain proves the drain completes
// before node renewal and the restore completes before the next node is touched.
func TestRenewK8sCertsBracketsEachNodeWithKubeVipDrain(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeB.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
			events.add("renew " + input.NodeID.String())
			return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
		}).Twice()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		"drain-prepared " + nodeA.ID.String(), "drain " + nodeA.ID.String(), "renew " + nodeA.ID.String(),
		"restore " + nodeA.ID.String(), "restored " + nodeA.ID.String(),
		"drain-prepared " + nodeB.ID.String(), "drain " + nodeB.ID.String(), "renew " + nodeB.ID.String(),
		"restore " + nodeB.ID.String(), "restored " + nodeB.ID.String(),
	}, events.snapshot())
	require.Len(t, events.postApply, 2)
	for _, req := range events.postApply {
		require.Equal(t, rolloutkubevip.GuardNameKubeVipDrainRestore, req.GuardName)
		require.Equal(t, clusterID, req.ClusterID)
		require.Equal(t, rollout.OutcomeApplied, req.Outcome)
		require.Empty(t, req.Params)
	}
	env.AssertExpectations(t)
}

// TestRenewK8sCertsSingleControlPlaneIsNotDrained: with one control-plane node the
// drain would cut the API off for workers that have nowhere else to connect, even
// when nothing needs renewing. Renewal must run without touching the kube-vip guard.
func TestRenewK8sCertsSingleControlPlaneIsNotDrained(t *testing.T) {
	for _, outcome := range []db.CertificateRenewalOutcome{db.CertificateRenewalOutcomeUnchanged, db.CertificateRenewalOutcomeRenewed} {
		t.Run(string(outcome), func(t *testing.T) {
			clusterID := uuid.New()
			node := certificateRenewalWorkflowNode(clusterID, "cp-a")
			env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
			events := &kubeVipGuardEvents{}
			expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
			expectCertificateRenewalBegin(t, env, a, clusterID, false)
			env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
			env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
					events.add("renew " + input.NodeID.String())
					return activities.KubernetesCertificateRenewalResult{Outcome: outcome}, nil
				}).Once()
			env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

			env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, []string{"renew " + node.ID.String()}, events.snapshot())
			require.Empty(t, events.postApply)
			env.AssertExpectations(t)
		})
	}
}

// TestRenewK8sCertsRestoresKubeVipAfterRenewalFailure keeps mutation-lock
// resolution unchanged while still undoing the drain for a node that failed.
func TestRenewK8sCertsRestoresKubeVipAfterRenewalFailure(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, &node.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
			events.add("renew " + input.NodeID.String())
			return activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError("node renewal failed", "renewal", nil)
		}).Once()
	env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.Anything).Return(errors.New("node still running")).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.CycleID == cycleID() && input.Phase == certificateRenewalRenewNode && input.RetainMutationLock &&
			strings.Contains(input.Message, "node renewal failed")
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "node renewal failed")
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "drain " + node.ID.String(), "renew " + node.ID.String(),
		"restore " + node.ID.String(), "restored " + node.ID.String(),
	}, events.snapshot())
	require.Len(t, events.postApply, 1)
	require.Equal(t, rollout.OutcomeFailedNotRestored, events.postApply[0].Outcome)
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsRestoresKubeVipAfterCancellation requires the restore to use
// a context that survives the workflow cancellation that aborted renewal.
func TestRenewK8sCertsRestoresKubeVipAfterCancellation(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, &node.ID).Maybe()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{}, activity.ErrResultPending).Once()
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
	env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.Anything).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()))
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "drain " + node.ID.String(),
		"restore " + node.ID.String(), "restored " + node.ID.String(),
	}, events.snapshot())
	env.AssertExpectations(t)
}

// TestRenewK8sCertsStopsBeforeRenewalWhenKubeVipDrainFails verifies a failed
// drain never reaches the apiserver restart, and the partially drained node is restored.
func TestRenewK8sCertsStopsBeforeRenewalWhenKubeVipDrainFails(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{checkPreApplyErr: "new VIP socket appeared"})
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, &node.ID).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.CycleID == cycleID() && input.Phase == certificateRenewalRenewNode && !input.RetainMutationLock &&
			strings.Contains(input.Message, "new VIP socket appeared")
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "new VIP socket appeared")
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "drain " + node.ID.String(),
		"restore " + node.ID.String(), "restored " + node.ID.String(),
	}, events.snapshot())
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsFailsCycleWhenKubeVipRestoreFails ensures a node that renewed
// but could not have its VIP restored is never reported as a success.
func TestRenewK8sCertsFailsCycleWhenKubeVipRestoreFails(t *testing.T) {
	clusterID := uuid.New()
	nodeA := certificateRenewalWorkflowNode(clusterID, "cp-a")
	nodeB := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{
		postApplyErr: temporal.NewNonRetryableApplicationError("restore refused", "RolloutCleanupFailed", nil),
	})
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{nodeA, nodeB}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{nodeA.ID, nodeB.ID}, &nodeA.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
		return input.CycleID == cycleID() && input.FailedNodeID != nil && *input.FailedNodeID == nodeA.ID &&
			input.Phase == certificateRenewalRenewNode && !input.RetainMutationLock &&
			strings.Contains(input.Message, "restore refused")
	})).Return(activities.FailCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.ErrorContains(t, env.GetWorkflowError(), "restore refused")
	require.Len(t, events.postApply, 1, "the cycle must stop before the next node")
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsWithoutKubeVipGuardStateStillRenews covers clusters whose
// agents have no kube-vip to drain: the guard reports no state to check and
// renewal proceeds unchanged.
func TestRenewK8sCertsWithoutKubeVipGuardStateStillRenews(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	env, a := newCertificateRenewalWorkflowEnvWithoutGuardMocks(t)
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{statelessNoChecks: true})
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, &node.ID).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID, peer.ID}, &peer.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil).Twice()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "restore " + node.ID.String(),
		"drain-prepared " + peer.ID.String(), "restore " + peer.ID.String(),
	}, events.snapshot())
	env.AssertExpectations(t)
}

func TestRenewK8sCertsReserveFailureReleasesMutationLock(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	env, a := newCertificateRenewalWorkflowEnv(t)
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).Return(activities.KubernetesCertificateRenewalResult{},
		temporal.NewNonRetryableApplicationError("node-agent installation is in progress", activities.CertificateRenewalNotDispatched, nil)).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.FailCertificateRenewalInput) (activities.FailCertificateRenewalResult, error) {
		require.False(t, input.RetainMutationLock)
		require.Equal(t, &node.ID, input.FailedNodeID)
		require.Contains(t, input.Message, "node-agent installation is in progress")
		return activities.FailCertificateRenewalResult{}, nil
	}).Once()
	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})
	require.Error(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// All unchanged scheduled nodes skip both guards and dispatch. Forced cycles
// bypass read-only selection and still drain each node before restarting it.
func TestRenewK8sCertsUnchangedThreeNodeCycleNeverDrainsUnlessForced(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("forced=%t", forced), func(t *testing.T) {
			clusterID := uuid.New()
			nodes := []db.Node{
				certificateRenewalWorkflowNode(clusterID, "cp-a"),
				certificateRenewalWorkflowNode(clusterID, "cp-b"),
				certificateRenewalWorkflowNode(clusterID, "cp-c"),
			}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
			a := &activities.Activities{}
			events := &kubeVipGuardEvents{}
			expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
			expectCertificateRenewalBegin(t, env, a, clusterID, forced)
			env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return(nodes, nil).Once()
			expectedIDs := []uuid.UUID{nodes[0].ID, nodes[1].ID, nodes[2].ID}
			expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, nil).Once()
			outcome := db.CertificateRenewalOutcomeUnchanged
			if forced {
				outcome = db.CertificateRenewalOutcomeRenewed
				env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
					return input.Force && input.ExpectedEtcdMembers == 3
				})).Return(activities.KubernetesCertificateRenewalResult{Outcome: outcome}, nil).Times(3)
			} else {
				// Allow an erroneous dispatch to complete, so the regression fails
				// on observed guard calls rather than a missing activity mock.
				env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
					Return(activities.KubernetesCertificateRenewalResult{Outcome: outcome}, nil).Maybe()
				env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
					return !input.Force && input.ThresholdDays == 14
				})).Return(activities.KubernetesCertificateRenewalCheckResult{}, nil).Times(3)
			}
			for _, node := range nodes {
				expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &node.ID).Once()
			}
			env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.CompleteCertificateRenewalInput) bool {
				return input.Outcome == outcome
			})).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()
			input := RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, Force: forced}
			if !forced {
				due := time.Now().Add(-time.Hour)
				input.ScheduledFor = &due
			}
			env.ExecuteWorkflow(RenewK8sCertsV2, input)
			require.NoError(t, env.GetWorkflowError())
			var result RenewK8sCertsResult
			require.NoError(t, env.GetWorkflowResult(&result))
			require.Equal(t, outcome, result.Outcome)
			require.Len(t, result.Results, 3)
			for _, nodeResult := range result.Results {
				require.Equal(t, outcome, nodeResult.Outcome)
			}
			if forced {
				require.Len(t, events.postApply, 3)
				env.AssertNotCalled(t, "CheckKubernetesCertificateRenewalRequired", mock.Anything, mock.Anything)
			} else {
				require.Empty(t, events.snapshot())
				env.AssertNotCalled(t, rollouttemporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything)
				env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.Anything)
				env.AssertNotCalled(t, "RenewLegacyKubernetesCertificates", mock.Anything, mock.Anything)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestRenewK8sCertsExistingV2HistorySkipsExpiryCheck(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	env, a := newCertificateRenewalWorkflowEnv(t)
	env.OnGetVersion(certificateRenewalExpiryCheckChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()
	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})
	require.NoError(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "CheckKubernetesCertificateRenewalRequired", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

func TestRenewK8sCertsExpiryCheckFailureDoesNotDispatch(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.Anything).Return(activities.KubernetesCertificateRenewalCheckResult{},
		temporal.NewNonRetryableApplicationError("unparseable expiry", "TestFailure", nil)).Once()
	env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.Anything).Return(func(_ context.Context, input activities.FailCertificateRenewalInput) (activities.FailCertificateRenewalResult, error) {
		require.False(t, input.RetainMutationLock)
		return activities.FailCertificateRenewalResult{}, nil
	}).Once()
	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14})
	require.ErrorContains(t, env.GetWorkflowError(), "unparseable expiry")
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsPendingLegacyIntentUsesLegacyExecutor models an abandoned
// legacy-runbook attempt left in restart_pending whose agent was then upgraded to
// support V2. Only the runbook resumes its own phase file, so the node must go to
// the legacy executor, still behind the kube-vip drain, without probing V2: the
// typed operation would report fresh certificates unchanged and leave the
// remaining static pods on their old in-memory certificates.
func TestRenewK8sCertsPendingLegacyIntentUsesLegacyExecutor(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	a := &activities.Activities{}
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectedIDs := []uuid.UUID{node.ID, peer.ID}
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &node.ID).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &peer.ID).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID
	})).Return(activities.KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true}, nil).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == peer.ID
	})).Return(activities.KubernetesCertificateRenewalCheckResult{}, nil).Once()
	// A V2-capable agent would accept typed work and report it unchanged.
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil).Maybe()
	env.OnActivity(a.RenewLegacyKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.ExpectedEtcdMembers == 2 && !input.Force
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		events.add("legacy " + input.NodeID.String())
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.CompleteCertificateRenewalInput) bool {
		return input.Outcome == db.CertificateRenewalOutcomeRenewed
	})).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	due := time.Now().Add(-time.Hour)
	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, ScheduledFor: &due})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "drain " + node.ID.String(), "legacy " + node.ID.String(),
		"restore " + node.ID.String(), "restored " + node.ID.String(),
	}, events.snapshot())
	env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRenewK8sCertsPendingIntentInBothExecutorsFinishesBothInOneCycle models a
// legacy phase file left in restart_pending and a later forced typed attempt that
// also failed mid-restart. The legacy executor resumes its own phase file, then
// the typed executor resumes the agent's intent in the same cycle and behind the
// same drain, so neither restart waits for the next due time.
func TestRenewK8sCertsPendingIntentInBothExecutorsFinishesBothInOneCycle(t *testing.T) {
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	a := &activities.Activities{}
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	expectCertificateRenewalBegin(t, env, a, clusterID, false)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectedIDs := []uuid.UUID{node.ID, peer.ID}
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &node.ID).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &peer.ID).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID
	})).Return(activities.KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true, Typed: true}, nil).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == peer.ID
	})).Return(activities.KubernetesCertificateRenewalCheckResult{}, nil).Once()
	env.OnActivity(a.RenewLegacyKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.ExpectedEtcdMembers == 2 && !input.Force
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		events.add("legacy " + input.NodeID.String())
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.ExpectedEtcdMembers == 2 && !input.Force
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		events.add("typed " + input.NodeID.String())
		// Fresh certificates: the typed operation only finishes its restarts.
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil
	}).Once()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.CompleteCertificateRenewalInput) bool {
		return input.Outcome == db.CertificateRenewalOutcomeRenewed
	})).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()

	due := time.Now().Add(-time.Hour)
	env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, ScheduledFor: &due})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		"drain-prepared " + node.ID.String(), "drain " + node.ID.String(), "legacy " + node.ID.String(), "typed " + node.ID.String(),
		"restore " + node.ID.String(), "restored " + node.ID.String(),
	}, events.snapshot())
	env.AssertNotCalled(t, "ResolveKubernetesCertificateRenewal", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}
