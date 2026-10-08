// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestInspectionObservationUnavailableDoesNotLookHealthy(t *testing.T) {
	now := time.Now().UTC()
	before := now.Add(-time.Hour)
	previous := &db.CertificateInspection{Available: true, ObservedAt: &before, Certificates: []db.CertificateObservation{{Credential: "kubelet-client", State: "available", NotAfter: now.Add(time.Hour)}}}
	for _, code := range []codes.Code{codes.Unavailable, codes.Unimplemented} {
		t.Run(code.String(), func(t *testing.T) {
			got := inspectionObservation(previous, nil, status.Error(code, "private error must not persist"), false, now)
			require.False(t, got.Available)
			require.Equal(t, previous.Certificates, got.Certificates)
			require.Equal(t, &before, got.ObservedAt)
			require.NotContains(t, got.Reason, "private")
			if code == codes.Unimplemented {
				require.Equal(t, "unsupported", got.Reason)
			}
		})
	}
}

func TestReconcileRetriesFailedInspectionSoonerAndSettlesOnSuccess(t *testing.T) {
	ctx := context.Background()
	database := inspectionSQLiteDB{DB: db.NewDB(testutil.SetupDB(ctx, t, (*db.Node)(nil)))}
	node, err := database.CreateNode(ctx, &db.Node{ID: uuid.New(), ClusterID: uuid.New(), Name: "managed-node", SpireAttestationMethod: db.SpireAttestationMethodTPM, Conditions: db.NodeConditions{Joined: db.NewCondition(true, "joined")}})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	complete := &pb.InspectKubernetesCertificatesResponse{Certificates: []*pb.KubernetesCertificateObservation{
		{Credential: "kubelet-client", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: now.Add(time.Hour).Unix()},
		{Credential: "kubelet-serving", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: now.Add(time.Hour).Unix()},
	}}
	stub := &inspectorStub{err: status.Error(codes.Unavailable, "agent restarting")}
	controller := NewController(config.Config{CertificateInspectionInterval: time.Hour}, dbwrapper.New(database, slog.New(slog.DiscardHandler)), stub)
	// age moves the stored attempt back, so the next reconcile is due without sleeping.
	age := func(by time.Duration) {
		t.Helper()
		stored, err := database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		stored.CertificateInspection.AttemptedAt = stored.CertificateInspection.AttemptedAt.Add(-by)
		_, err = database.UpdateNode(ctx, &stored)
		require.NoError(t, err)
	}
	reconcile := func() time.Duration {
		t.Helper()
		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		return result.RequeueAfter
	}

	// Each consecutive failure doubles the delay, up to the bound and never past the interval.
	delays := make([]time.Duration, 0, maxInspectionRetries+2)
	for range maxInspectionRetries + 2 {
		delays = append(delays, reconcile())
		age(time.Hour)
	}
	require.Equal(t, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, 32 * time.Minute, 32 * time.Minute}, delays)
	stored, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.False(t, stored.CertificateInspection.Available)
	require.Equal(t, maxInspectionRetries, stored.CertificateInspection.Retries)

	// An attempt whose backoff has not elapsed is not probed again.
	stored.CertificateInspection.AttemptedAt = time.Now()
	_, err = database.UpdateNode(ctx, &stored)
	require.NoError(t, err)
	calls := stub.calls
	require.InDelta(t, float64(32*time.Minute), float64(reconcile()), float64(time.Minute))
	require.Equal(t, calls, stub.calls, "a failed inspection was probed before its backoff elapsed")

	// Once the agent answers completely, the full interval returns and the count clears.
	stub.err = nil
	stub.response = complete
	age(time.Hour)
	require.Equal(t, time.Hour, reconcile())
	require.Equal(t, calls+1, stub.calls)
	stored, err = database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.True(t, stored.CertificateInspection.Available)
	require.Zero(t, stored.CertificateInspection.Retries)

	// An agent without the RPC is not retried early.
	stub.err = status.Error(codes.Unimplemented, "old agent")
	age(2 * time.Hour)
	require.Equal(t, time.Hour, reconcile())
	stored, err = database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.Zero(t, stored.CertificateInspection.Retries)
}

func TestNextDelayRespectsInspectionInterval(t *testing.T) {
	for _, interval := range []time.Duration{5 * time.Minute, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			controller := &Controller{cfg: config.Config{CertificateInspectionInterval: interval}}
			require.Equal(t, interval, controller.nextDelay(&db.CertificateInspection{}))
			require.Equal(t, time.Minute, controller.nextDelay(&db.CertificateInspection{Retries: 1}))
			require.Equal(t, min(32*time.Minute, interval), controller.nextDelay(&db.CertificateInspection{Retries: maxInspectionRetries}))
		})
	}
}

func TestNeedsEarlyRetry(t *testing.T) {
	complete := func(servingState, superAdminState string) *db.CertificateInspection {
		return &db.CertificateInspection{Available: true, Certificates: []db.CertificateObservation{
			{Credential: "kubelet-client", State: "available"},
			{Credential: "kubelet-serving", State: servingState},
			{Credential: "super-admin", State: superAdminState},
		}}
	}
	for _, test := range []struct {
		name        string
		observation *db.CertificateInspection
		want        bool
	}{
		{"transport failure", &db.CertificateInspection{Reason: "unavailable"}, true},
		{"invalid response", &db.CertificateInspection{Reason: inspectionInvalidResponse}, true},
		{"unsupported agent", &db.CertificateInspection{Reason: inspectionUnsupported}, false},
		{"everything available", complete("available", "available"), false},
		{"required credential missing", complete("missing", "available"), true},
		{"required credential invalid", complete("invalid", "available"), true},
		{"only the optional super-admin is missing", complete("available", "missing"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, needsEarlyRetry(test.observation))
		})
	}
}

func TestInspectionObservationRequiresCompleteBoundedCatalog(t *testing.T) {
	now := time.Now().UTC()
	good := &pb.InspectKubernetesCertificatesResponse{Certificates: []*pb.KubernetesCertificateObservation{
		{Credential: "kubelet-client", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: now.Add(-time.Hour).Unix()},
		{Credential: "kubelet-serving", State: pb.KubernetesCertificateObservation_STATE_MISSING},
	}}
	got := inspectionObservation(nil, good, nil, false, now)
	require.True(t, got.Available)
	require.Len(t, got.Certificates, 2)
	require.True(t, got.Certificates[0].NotAfter.Before(now))
	for _, items := range [][]*pb.KubernetesCertificateObservation{good.GetCertificates()[:1], {good.GetCertificates()[0], good.GetCertificates()[0]}, {{Credential: "secret/arbitrary", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: now.Unix()}}, {{Credential: "kubelet-client", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE}, {Credential: "kubelet-serving", State: pb.KubernetesCertificateObservation_STATE_MISSING}}} {
		got := inspectionObservation(nil, &pb.InspectKubernetesCertificatesResponse{Certificates: items}, nil, false, now)
		require.False(t, got.Available)
		require.Equal(t, "invalid_response", got.Reason)
	}
}

type inspectorStub struct {
	response *pb.InspectKubernetesCertificatesResponse
	err      error
	calls    int
}

func (s *inspectorStub) InspectKubernetesCertificates(context.Context, db.Node) (*pb.InspectKubernetesCertificatesResponse, error) {
	s.calls++
	return s.response, s.err
}

func TestReconcilePersistsPublicObservationsAndPreservesLifecycleConditions(t *testing.T) {
	ctx := context.Background()
	database := inspectionSQLiteDB{DB: db.NewDB(testutil.SetupDB(ctx, t, (*db.Node)(nil)))}
	now := time.Now().UTC().Truncate(time.Second)
	node, err := database.CreateNode(ctx, &db.Node{ID: uuid.New(), ClusterID: uuid.New(), Name: "managed-node", SpireAttestationMethod: db.SpireAttestationMethodTPM, Conditions: db.NodeConditions{Joined: db.NewCondition(true, "joined"), KubernetesReady: db.NewCondition(true, "ready")}})
	require.NoError(t, err)
	stub := &inspectorStub{response: &pb.InspectKubernetesCertificatesResponse{Certificates: []*pb.KubernetesCertificateObservation{
		{Credential: "kubelet-client", State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: now.Add(time.Hour).Unix()},
		{Credential: "kubelet-serving", State: pb.KubernetesCertificateObservation_STATE_MISSING},
	}}}
	controller := NewController(config.Config{CertificateInspectionInterval: time.Hour}, dbwrapper.New(database, slog.New(slog.DiscardHandler)), stub)
	result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
	require.NoError(t, err)
	// The missing kubelet-serving certificate may still be bootstrapping, so it is checked again soon.
	require.Equal(t, inspectionRetryBase, result.RequeueAfter)
	stored, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.True(t, stored.CertificateInspection.Available)
	require.Equal(t, 1, stored.CertificateInspection.Retries)
	require.Len(t, stored.CertificateInspection.Certificates, 2)
	require.True(t, node.Conditions.Equal(stored.Conditions))
	// Restart uses the persisted attempt time rather than probing at scrape speed.
	restarted := NewController(config.Config{CertificateInspectionInterval: time.Hour}, dbwrapper.New(database, slog.New(slog.DiscardHandler)), stub)
	_, err = restarted.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
	require.NoError(t, err)
	require.Equal(t, 1, stub.calls)
	stored.CertificateInspection.AttemptedAt = time.Now().Add(-2 * time.Hour)
	stored.CertificateInspection.Retries = 0
	_, err = database.UpdateNode(ctx, &stored)
	require.NoError(t, err)
	stub.err = status.Error(codes.Unavailable, "transport failed")
	_, err = restarted.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
	require.NoError(t, err)
	stored, err = database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.False(t, stored.CertificateInspection.Available)
	require.Equal(t, now.Add(time.Hour), stored.CertificateInspection.Certificates[0].NotAfter)
	require.NoError(t, database.DeleteNode(ctx, node.ID))
	_, err = restarted.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
	require.NoError(t, err)
	require.Equal(t, 2, stub.calls)
}

// SQLite serializes this test on one connection and does not implement FOR UPDATE.
// The adapter preserves real transactions/JSON persistence while translating that lock read.
type inspectionSQLiteDB struct{ db.DB }

func (d inspectionSQLiteDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return d.DB.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error { return fn(ctx, inspectionSQLiteTx{Tx: tx}) })
}

type inspectionSQLiteTx struct{ db.Tx }

func (t inspectionSQLiteTx) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return t.Tx.GetNode(ctx, id)
}
