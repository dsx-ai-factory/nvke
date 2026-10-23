// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// TestRenewK8sCertsV2TypedIntentOnPreV2AgentReleasesLock uses the real activities
// against an agent that holds typed intent but lacks V2 Start and so never
// accepted the operation. Its status RPC answers NotFound, which must not be
// mistaken for an unconfirmed operation: the cycle fails and releases the lock so
// the agent upgrade that can finish the typed renewal is not blocked.
func TestRenewK8sCertsV2TypedIntentOnPreV2AgentReleasesLock(t *testing.T) {
	for _, required := range []string{"typed", "legacy_then_typed"} {
		t.Run(required, func(t *testing.T) {
			clusterID := uuid.New()
			node := certificateRenewalWorkflowNode(clusterID, "cp-a")
			node.IP = "192.0.2.10"
			node.Conditions.Joined = db.NewCondition(true, "")
			node.Conditions.Provisioned = db.NewCondition(true, "")
			oldDue := time.Now().Add(-time.Hour).UTC()
			database := &certificateRenewalFallbackDatabase{
				cluster: db.Cluster{
					ID: clusterID, NextCertRenewalDue: &oldDue,
					Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "")},
				},
				node: node,
			}
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
			rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)

			checks, mutations, receipts := 0, 0, 0
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
					return nil, status.Error(codes.Unimplemented, "unknown method")
				},
				GetKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return nil, status.Error(codes.NotFound, "operation not found")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, "k8s-cert-renewal", name)
					if strings.Contains(script, "export CHECK_ONLY='true'") {
						checks++
						return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_REQUIRED=" + required + "\n"}, nil
					}
					require.Equal(t, "legacy_then_typed", required, "typed intent must not dispatch the legacy runbook")
					operationID := database.cluster.CertificateRenewal.OperationID
					if strings.Contains(script, "export STATUS_ONLY='true'") {
						receipts++
						return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_TERMINAL_OPERATION_ID=" + operationID + "\n"}, nil
					}
					mutations++
					return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=renewed\n"}, nil
				},
			}
			a := activities.NewActivities(config.Config{RunbooksDir: t.TempDir(), CertRenewalInterval: 12 * time.Hour}, database, nil, fakeAgent, "", nil, "")
			env.RegisterActivity(a.BeginCertificateRenewal)
			env.RegisterActivity(a.ListControlPlaneNodes)
			env.RegisterActivity(a.CheckKubernetesCertificateRenewalRequired)
			env.RegisterActivity(a.RenewKubernetesCertificates)
			env.RegisterActivity(a.RenewLegacyKubernetesCertificates)
			env.RegisterActivity(a.CompleteCertificateRenewal)
			env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)
			env.RegisterActivity(a.FailCertificateRenewal)
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).
				Return(func(context.Context, activities.ValidateCertificateRenewalHealthInput) error { return nil }).Once()

			env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, ScheduledFor: &oldDue})

			require.True(t, env.IsWorkflowCompleted())
			workflowErr := env.GetWorkflowError()
			require.ErrorContains(t, workflowErr, "node agent lacks certificate renewal V2 but holds pending typed renewal intent; upgrade the node agent")
			require.Equal(t, 1, checks)
			wantLegacy := 0
			if required == "legacy_then_typed" {
				wantLegacy = 1
			}
			require.Equal(t, wantLegacy, mutations)
			require.Equal(t, wantLegacy, receipts)
			require.Len(t, fakeAgent.StartKubernetesCertificateRenewalV2Calls, 1)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Empty(t, fakeAgent.GetKubernetesCertificateRenewalCalls, "a never-accepted operation needs no status resolution")
			require.Nil(t, database.cluster.CertificateRenewal)
			require.Nil(t, database.cluster.Conditions.UpdateInProgress, "the mutation lock must not block the agent upgrade")
			renewal := database.cluster.Conditions.CertificateRenewal
			require.Equal(t, db.CertificateRenewalStateFailed, renewal.State)
			require.Equal(t, certificateRenewalRenewNode, renewal.Phase)
			require.Equal(t, &node.ID, renewal.FailedNodeID)
			require.Contains(t, renewal.Message, "upgrade the node agent")
			require.NotContains(t, renewal.Message, "cluster mutation lock retained")
			require.Equal(t, oldDue, *database.cluster.NextCertRenewalDue)
			env.AssertExpectations(t)
		})
	}
}
