// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbmetrics

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type credentialDBStub struct {
	clusters []db.Cluster
	nodes    []db.Node
	err      error
}

func (s *credentialDBStub) ListClusters(context.Context, db.ClusterFilters) ([]db.Cluster, error) {
	return s.clusters, s.err
}

func (s *credentialDBStub) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return s.nodes, s.err
}

func gatherCredentials(t *testing.T, database *credentialDBStub) map[string]*dto.MetricFamily {
	t.Helper()
	collector := NewCredentialCollector(slog.New(slog.DiscardHandler), database, config.Config{CredentialMonitoringEnabled: true, CertRenewalControllerEnabled: true, CertificateObservationMaxAge: 3 * time.Hour, CertificateWarningDays: 45, CertRenewalInterval: 12 * time.Hour})
	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, registry.Register(collector))
	families, err := registry.Gather()
	require.NoError(t, err)
	result := map[string]*dto.MetricFamily{}
	for _, family := range families {
		result[family.GetName()] = family
	}
	return result
}

func TestCredentialMetricsBoundedInventoryFreshnessAndDeletion(t *testing.T) {
	now := time.Now().UTC()
	id := uuid.New()
	before := now.Add(-time.Hour)
	cluster := db.Cluster{ID: id, Name: "managed", CreatedAt: before, Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "")}}
	node := db.Node{ID: uuid.New(), ClusterID: id, Name: "node-one", Conditions: db.NodeConditions{Joined: db.NewCondition(true, "")}, CertificateInspection: &db.CertificateInspection{Available: true, ObservedAt: &before, Certificates: []db.CertificateObservation{
		{Credential: "kubelet-client", State: "available", NotAfter: now.Add(24 * time.Hour)},
		{Credential: "arbitrary-secret-name", State: "available", NotAfter: now.Add(time.Hour)},
	}}}
	database := &credentialDBStub{clusters: []db.Cluster{cluster}, nodes: []db.Node{node}}
	got := gatherCredentials(t, database)
	observations := got["nvke_controller_certificate_observation_available"].GetMetric()
	require.Len(t, observations, 2)
	for _, metric := range observations {
		for _, label := range metric.GetLabel() {
			require.NotEqual(t, "arbitrary-secret-name", label.GetValue())
			require.Contains(t, []string{"tenant_cluster", "node", "credential"}, label.GetName())
		}
	}
	require.InDelta(t, 1, got["nvke_controller_certificate_inspection_available"].GetMetric()[0].GetGauge().GetValue(), 0.000001)
	require.Len(t, got["nvke_controller_certificate_not_after_timestamp_seconds"].GetMetric(), 1)
	// A fresh scrape after a process restart derives the same metrics from DB state.
	again := gatherCredentials(t, database)
	require.InDelta(t, 1, again["nvke_controller_certificate_inspection_available"].GetMetric()[0].GetGauge().GetValue(), 0.000001)
	old := now.Add(-4 * time.Hour)
	database.nodes[0].CertificateInspection.ObservedAt = &old
	got = gatherCredentials(t, database)
	require.Zero(t, got["nvke_controller_certificate_inspection_available"].GetMetric()[0].GetGauge().GetValue())
	require.Len(t, got["nvke_controller_certificate_not_after_timestamp_seconds"].GetMetric(), 1)
	database.nodes = nil
	got = gatherCredentials(t, database)
	require.Nil(t, got["nvke_controller_certificate_observation_available"])
	database.clusters = nil
	got = gatherCredentials(t, database)
	require.Nil(t, got["nvke_controller_renewal_state"])
}

func TestCredentialMetricsNeverRenewedOverdue(t *testing.T) {
	for _, tt := range []struct {
		name        string
		clusterAge  time.Duration
		wantOverdue float64
	}{
		{name: "younger than renewal interval", clusterAge: 2 * time.Hour, wantOverdue: 0},
		{name: "older than renewal interval", clusterAge: 13 * time.Hour, wantOverdue: 3600},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := db.Cluster{
				ID:         uuid.New(),
				Name:       "never-renewed",
				CreatedAt:  time.Now().Add(-tt.clusterAge),
				Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "")},
			}
			got := gatherCredentials(t, &credentialDBStub{clusters: []db.Cluster{cluster}})
			metrics := got["nvke_controller_renewal_overdue_seconds"].GetMetric()
			require.Len(t, metrics, 1)
			overdue := metrics[0].GetGauge().GetValue()
			if tt.wantOverdue == 0 {
				require.Zero(t, overdue)
			} else {
				require.InDelta(t, tt.wantOverdue, overdue, 1)
			}
		})
	}
}

func TestCredentialMetricsOptionalCredentialAndFailures(t *testing.T) {
	now := time.Now().UTC()
	id := uuid.New()
	started := now.Add(-3 * time.Hour)
	cluster := db.Cluster{ID: id, Name: "managed", CreatedAt: started, Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, ""), CertificateRenewal: &db.CertificateRenewalStatus{State: db.CertificateRenewalStateFailed, StartedAt: started, Message: "secret diagnostic"}, EtcdKeyRotation: &db.EtcdKeyRotationStatus{State: db.EtcdKeyRotationStateRunning, LastProgressAt: started, WorkflowID: "private-request-id"}, UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}}}
	node := db.Node{ID: uuid.New(), ClusterID: id, Name: "cp", ControlPlane: true, Conditions: db.NodeConditions{Joined: db.NewCondition(true, "")}, CertificateInspection: &db.CertificateInspection{Available: true, ObservedAt: &now}}
	database := &credentialDBStub{clusters: []db.Cluster{cluster}, nodes: []db.Node{node}}
	collector := NewCredentialCollector(slog.New(slog.DiscardHandler), database, config.Config{CertificateObservationMaxAge: 3 * time.Hour})
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`# HELP nvke_controller_renewal_mutation_lock_retained One when a failed renewal still owns cluster mutation.
# TYPE nvke_controller_renewal_mutation_lock_retained gauge
nvke_controller_renewal_mutation_lock_retained{tenant_cluster="managed"} 1
`), "nvke_controller_renewal_mutation_lock_retained"))
	got := gatherCredentials(t, database)
	require.Greater(t, got["nvke_controller_etcd_key_rotation_progress_age_seconds"].GetMetric()[0].GetGauge().GetValue(), float64(10000))
	for _, metric := range got["nvke_controller_certificate_required"].GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "credential" && label.GetValue() == "super-admin" {
				require.Zero(t, metric.GetGauge().GetValue())
			}
		}
	}
	database.err = errors.New("database disconnected")
	got = gatherCredentials(t, database)
	require.Zero(t, got["nvke_controller_credential_metrics_collection_success"].GetMetric()[0].GetGauge().GetValue())
	require.Nil(t, got["nvke_controller_renewal_state"])
}
