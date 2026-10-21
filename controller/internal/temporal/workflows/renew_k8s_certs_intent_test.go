// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// Persisted typed intent belongs to the typed executor even when V2 Start reports
// Unsupported. In particular, a completed legacy receipt cannot stand in for the
// second (typed) leg. Both legs stay behind one drain, including forced cycles.
func TestRenewK8sCertsTypedIntentPreservesExecutor(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, forced := range []bool{false, true} {
			for _, unsupported := range []bool{false, true} {
				t.Run(fmt.Sprintf("legacy=%t/forced=%t/unsupported=%t", legacy, forced, unsupported), func(t *testing.T) {
					testRenewK8sCertsTypedIntent(t, legacy, forced, unsupported)
				})
			}
		}
	}
}

func testRenewK8sCertsTypedIntent(t *testing.T, legacy, forced, unsupported bool) {
	t.Helper()
	clusterID := uuid.New()
	node := certificateRenewalWorkflowNode(clusterID, "cp-a")
	peer := certificateRenewalWorkflowNode(clusterID, "cp-b")
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.OnGetVersion(certificateRenewalExecutorIntentChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	a := &activities.Activities{}
	events := &kubeVipGuardEvents{}
	expectKubeVipGuard(env, events, kubeVipGuardBehavior{})
	cycleID := expectCertificateRenewalBegin(t, env, a, clusterID, forced)
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node, peer}, nil).Once()
	expectedIDs := []uuid.UUID{node.ID, peer.ID}
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, nil).Once()
	expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &node.ID).Once()
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.Force == forced
	})).Return(activities.KubernetesCertificateRenewalCheckResult{Required: true, Legacy: legacy, Typed: true}, nil).Maybe()
	// Allow an incorrect fallback to return the already-completed receipt, then
	// assert that it was not used to report typed completion.
	env.OnActivity(a.RenewLegacyKubernetesCertificates, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
			events.add("legacy " + input.NodeID.String())
			return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
		}).Maybe()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == node.ID && input.Force == forced && input.ExpectedEtcdMembers == 2 &&
			input.OperationID == certificateRenewalOperationID(cycleID(), node.ID)
	})).Return(func(_ context.Context, input activities.KubernetesCertificateRenewalInput) (activities.KubernetesCertificateRenewalResult, error) {
		events.add("typed " + input.NodeID.String())
		if unsupported {
			return activities.KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError("unsupported", activities.CertificateRenewalLegacyRequired, nil)
		}
		return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil
	}).Once()
	if unsupported {
		env.OnActivity(a.ResolveKubernetesCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.ResolveKubernetesCertificateRenewalInput) bool {
			return !input.Legacy && input.NodeID == node.ID && input.OperationID == certificateRenewalOperationID(cycleID(), node.ID)
		})).Return(func(ctx context.Context, _ activities.ResolveKubernetesCertificateRenewalInput) error {
			require.NoError(t, ctx.Err(), "typed cleanup is disconnected")
			return errors.New("typed termination unconfirmed")
		}).Once()
		env.OnActivity(a.FailCertificateRenewal, mock.Anything, mock.MatchedBy(func(input activities.FailCertificateRenewalInput) bool {
			return input.RetainMutationLock && input.FailedNodeID != nil && *input.FailedNodeID == node.ID
		})).Return(activities.FailCertificateRenewalResult{}, nil).Once()
	} else {
		expectCertificateRenewalHealth(t, env, a, clusterID, expectedIDs, &peer.ID).Once()
		if forced {
			env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
				return input.NodeID == peer.ID && input.Force
			})).Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil).Once()
		}
	}
	// A fresh peer skips renewal unless forced. It is never reached on failure.
	env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
		return input.NodeID == peer.ID
	})).Return(activities.KubernetesCertificateRenewalCheckResult{Required: forced}, nil).Maybe()
	env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).
		Return(activities.CompleteCertificateRenewalResult{}, nil).Maybe()

	input := RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, Force: forced}
	if !forced {
		due := time.Now().Add(-time.Hour)
		input.ScheduledFor = &due
	}
	env.ExecuteWorkflow(RenewK8sCertsV2, input)
	if unsupported {
		require.ErrorContains(t, env.GetWorkflowError(), "unsupported")
		env.AssertNotCalled(t, "CompleteCertificateRenewal", mock.Anything, mock.Anything)
		env.AssertNotCalled(t, "CheckKubernetesCertificateRenewalRequired", mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
			return input.NodeID == peer.ID
		}))
	} else {
		require.NoError(t, env.GetWorkflowError())
		env.AssertNumberOfCalls(t, "CompleteCertificateRenewal", 1)
		var result RenewK8sCertsResult
		require.NoError(t, env.GetWorkflowResult(&result))
		require.Equal(t, db.CertificateRenewalOutcomeRenewed, result.Outcome)
	}
	if unsupported || !forced {
		env.AssertNotCalled(t, "RenewKubernetesCertificates", mock.Anything, mock.MatchedBy(func(input activities.KubernetesCertificateRenewalInput) bool {
			return input.NodeID == peer.ID
		}))
	}
	want := []string{"drain-prepared " + node.ID.String(), "drain " + node.ID.String()}
	if legacy {
		want = append(want, "legacy "+node.ID.String())
	}
	want = append(want, "typed "+node.ID.String(), "restore "+node.ID.String(), "restored "+node.ID.String())
	if forced && !unsupported {
		want = append(want, "drain-prepared "+peer.ID.String(), "drain "+peer.ID.String(), "restore "+peer.ID.String(), "restored "+peer.ID.String())
	}
	require.Equal(t, want, events.snapshot(), "restore must run even when typed termination is unconfirmed")
	checkCalls := 2
	if unsupported {
		checkCalls = 1
	}
	env.AssertNumberOfCalls(t, "CheckKubernetesCertificateRenewalRequired", checkCalls)
	env.AssertExpectations(t)
}

// The new intent marker cannot add forced checks or remove a recorded fallback
// in histories that predate it. The original expiry and typed-after-legacy
// markers still decide their respective command sequences.
func TestRenewK8sCertsExecutorIntentDefaultVersionPreservesCommands(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("forced=%t", forced), func(t *testing.T) {
			clusterID := uuid.New()
			node := certificateRenewalWorkflowNode(clusterID, "cp-a")
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			a := &activities.Activities{}
			env.OnGetVersion(certificateRenewalExecutorIntentChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
			env.OnGetVersion(certificateRenewalExpiryCheckChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
			expectCertificateRenewalBegin(t, env, a, clusterID, forced)
			env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return([]db.Node{node}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).Once()
			legacyCalls := 1
			if !forced {
				legacyCalls = 2
				env.OnGetVersion(certificateRenewalTypedAfterLegacyChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
				env.OnActivity(a.CheckKubernetesCertificateRenewalRequired, mock.Anything, mock.Anything).
					Return(activities.KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true, Typed: true}, nil).Once()
			}
			env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).Return(activities.KubernetesCertificateRenewalResult{},
				temporal.NewNonRetryableApplicationError("unsupported", activities.CertificateRenewalLegacyRequired, nil)).Once()
			env.OnActivity(a.RenewLegacyKubernetesCertificates, mock.Anything, mock.Anything).
				Return(activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeRenewed}, nil).Times(legacyCalls)
			env.OnActivity(a.CompleteCertificateRenewal, mock.Anything, mock.Anything).Return(activities.CompleteCertificateRenewalResult{}, nil).Once()
			env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, Force: forced})
			require.NoError(t, env.GetWorkflowError())
			if forced {
				env.AssertNotCalled(t, "CheckKubernetesCertificateRenewalRequired", mock.Anything, mock.Anything)
			}
			env.AssertExpectations(t)
		})
	}
}
