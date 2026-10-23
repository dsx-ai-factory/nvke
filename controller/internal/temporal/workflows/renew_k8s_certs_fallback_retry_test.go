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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// TestRenewK8sCertsV2SelectsLegacyAfterTypedRetry uses the real typed activity:
// synthesizing LegacyRequired in a mock would miss its activity-attempt guard.
// Real legacy receipt verification and cycle completion must release both owners
// and advance the due time after successful postflight.
func TestRenewK8sCertsV2SelectsLegacyAfterTypedRetry(t *testing.T) {
	for _, failure := range []string{"Start", "GetNode", "reservation", "release"} {
		t.Run(failure, func(t *testing.T) {
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
				node: node, failOnce: failure,
			}
			env, _ := newCertificateRenewalWorkflowEnv(t)
			var attempts []int32
			var inputs []activities.KubernetesCertificateRenewalInput
			env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, values converter.EncodedValues) {
				if info.ActivityType.Name == "RenewKubernetesCertificates" {
					var input activities.KubernetesCertificateRenewalInput
					require.NoError(t, values.Get(&input))
					attempts = append(attempts, info.Attempt)
					inputs = append(inputs, input)
				}
			})

			starts, mutations, receipts := 0, 0, 0
			var operationID string
			fakeAgent := &agenttest.FakeClient{
				StartKubernetesCertificateRenewalV2Hook: func(_ db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
					starts++
					if operationID == "" {
						operationID = req.GetOperationId()
					}
					require.Equal(t, operationID, req.GetOperationId())
					require.Equal(t, int32(1), req.GetExpectedEtcdMembers())
					require.Equal(t, int32(14), req.GetThresholdDays())
					require.Equal(t, &db.NodeAgentCertificateRenewal{
						NodeID: node.ID, OperationID: operationID, Fenced: true,
					}, database.cluster.CertificateRenewal)
					if failure == "Start" && starts == 1 {
						database.failOnce = ""
						return nil, status.Error(codes.Unavailable, "connection reset")
					}
					return nil, fmt.Errorf("wrapped Start: %w", status.Error(codes.Unimplemented, "unknown method"))
				},
				GetKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
					return nil, status.Error(codes.Unimplemented, "status unsupported")
				},
				ExecuteScriptHook: func(_ db.Node, name, script string) (*nvkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, "k8s-cert-renewal", name)
					require.Contains(t, script, "export OPERATION_ID='"+operationID+"'")
					require.NotNil(t, database.cluster.Conditions.UpdateInProgress)
					require.Equal(t, operationID, database.cluster.CertificateRenewal.OperationID)
					if strings.Contains(script, "export STATUS_ONLY='true'") {
						require.Equal(t, 1, mutations, "receipt must follow legacy mutation")
						receipts++
						return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_TERMINAL_OPERATION_ID=" + operationID + "\n"}, nil
					}
					require.Contains(t, script, "export EXPECTED_ETCD_MEMBERS='1'")
					require.Contains(t, script, "export THRESHOLD_DAYS='14'")
					mutations++
					return &nvkeproto.ExecuteScriptResponse{Output: "RENEWAL_OUTCOME=renewed\n"}, nil
				},
			}
			a := activities.NewActivities(config.Config{RunbooksDir: t.TempDir(), CertRenewalInterval: 12 * time.Hour}, database, nil, fakeAgent, "", nil, "")
			env.RegisterActivity(a.BeginCertificateRenewal)
			env.RegisterActivity(a.ListControlPlaneNodes)
			env.RegisterActivity(a.RenewKubernetesCertificates)
			env.RegisterActivity(a.RenewLegacyKubernetesCertificates)
			env.RegisterActivity(a.CompleteCertificateRenewal)
			env.RegisterActivity(a.ResolveKubernetesCertificateRenewal)
			env.RegisterActivity(a.FailCertificateRenewal)
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, nil).Once()
			expectCertificateRenewalHealth(t, env, a, clusterID, []uuid.UUID{node.ID}, &node.ID).
				Return(func(context.Context, activities.ValidateCertificateRenewalHealthInput) error {
					assert.Equal(t, 1, mutations)
					assert.Equal(t, 1, receipts, "postflight must follow matching terminal receipt verification")
					assert.Nil(t, database.cluster.CertificateRenewal, "node ownership must be released before postflight")
					assert.NotNil(t, database.cluster.Conditions.UpdateInProgress, "cycle still owns the mutation lock")
					assert.Equal(t, oldDue, *database.cluster.NextCertRenewalDue)
					return nil
				}).Once()

			env.ExecuteWorkflow(RenewK8sCertsV2, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 14, ScheduledFor: &oldDue})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			var result RenewK8sCertsResult
			require.NoError(t, env.GetWorkflowResult(&result))
			require.Equal(t, db.CertificateRenewalOutcomeRenewed, result.Outcome)
			require.Len(t, result.Results, 1)
			require.Equal(t, []int32{1, 2}, attempts)
			require.Len(t, inputs, 2)
			require.Equal(t, inputs[0], inputs[1], "all renewal parameters remain stable across attempts")
			require.Equal(t, operationID, inputs[0].OperationID)
			wantStarts := 1
			if failure == "Start" || failure == "release" {
				wantStarts = 2
			}
			require.Equal(t, wantStarts, starts)
			require.Equal(t, 1, mutations)
			require.Equal(t, 1, receipts)
			require.Empty(t, fakeAgent.StartKubernetesCertificateRenewalCalls)
			require.Empty(t, fakeAgent.GetKubernetesCertificateRenewalCalls, "fallback must not enter ambiguous typed status resolution")
			require.Nil(t, database.cluster.CertificateRenewal)
			require.Nil(t, database.cluster.Conditions.UpdateInProgress)
			renewal := database.cluster.Conditions.CertificateRenewal
			require.Equal(t, db.CertificateRenewalStateSucceeded, renewal.State)
			require.Equal(t, db.CertificateRenewalOutcomeRenewed, renewal.Outcome)
			require.NotNil(t, renewal.CompletedAt)
			require.Equal(t, renewal.CompletedAt.Add(12*time.Hour), *database.cluster.NextCertRenewalDue)
			require.True(t, database.cluster.NextCertRenewalDue.After(oldDue))
			require.Empty(t, database.failOnce, "the selected failure was exercised")
			env.AssertExpectations(t)
		})
	}
}

// certificateRenewalFallbackDatabase keeps the real admission, reservation and
// completion activities on the same cluster record, with one injected I/O error.
type certificateRenewalFallbackDatabase struct {
	db.DB
	mu       sync.Mutex
	cluster  db.Cluster
	node     db.Node
	failOnce string
}

func (d *certificateRenewalFallbackDatabase) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(ctx, d)
}

func (d *certificateRenewalFallbackDatabase) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	return d.cluster, nil
}

func (d *certificateRenewalFallbackDatabase) GetCluster(context.Context, uuid.UUID) (db.Cluster, error) {
	return d.cluster, nil
}

func (d *certificateRenewalFallbackDatabase) GetNode(context.Context, uuid.UUID) (db.Node, error) {
	if d.failOnce == "GetNode" {
		d.failOnce = ""
		return db.Node{}, errors.New("node lookup temporarily unavailable")
	}
	return d.node, nil
}

func (d *certificateRenewalFallbackDatabase) GetNodeForUpdate(context.Context, uuid.UUID) (db.Node, error) {
	if d.failOnce == "reservation" {
		d.failOnce = ""
		return db.Node{}, errors.New("reservation temporarily unavailable")
	}
	return d.node, nil
}

func (d *certificateRenewalFallbackDatabase) ListNodePools(context.Context, db.NodePoolFilters) ([]db.NodePool, error) {
	return nil, nil
}

func (d *certificateRenewalFallbackDatabase) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return []db.Node{d.node}, nil
}

func (d *certificateRenewalFallbackDatabase) UpdateCluster(_ context.Context, cluster *db.Cluster) (db.Cluster, error) {
	if d.failOnce == "release" && d.cluster.CertificateRenewal != nil && cluster.CertificateRenewal == nil {
		d.failOnce = ""
		return db.Cluster{}, errors.New("reservation release temporarily unavailable")
	}
	d.cluster = *cluster
	return *cluster, nil
}
