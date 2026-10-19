// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbmetrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dsx-ai-factory/nvke/agent/pkg/certificates"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	labelCredential       = "credential"
	labelState            = "state"
	credentialStateFailed = "failed"
)

type credentialDatabase interface {
	ListClusters(ctx context.Context, filters db.ClusterFilters) ([]db.Cluster, error)
	ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error)
}

// CredentialCollector exports bounded, persisted credential observations. It never dials tenant nodes while scraping.
type CredentialCollector struct {
	database    credentialDatabase
	logger      *slog.Logger
	cfg         config.Config
	descriptors map[string]*prometheus.Desc
}

// NewCredentialCollector constructs a collector with cluster/node inventory as the only identity labels.
func NewCredentialCollector(logger *slog.Logger, database credentialDatabase, cfg config.Config) *CredentialCollector {
	definitions := []struct {
		name, help string
		labels     []string
	}{
		{"certificate_not_after_timestamp_seconds", "Observed certificate NotAfter; absent when no expiration is known.", []string{labelTenantCluster, labelNode, labelCredential}},
		{"certificate_required", "One when the credential is required on this node role; optional super-admin is observed if present.", []string{labelTenantCluster, labelNode, labelCredential}},
		{"certificate_observation_available", "One when the last successful inspection is fresh and found this credential; a later failed attempt does not clear it.", []string{labelTenantCluster, labelNode, labelCredential}},
		{"certificate_inspection_available", "One when the latest node inspection succeeded and is fresh.", []string{labelTenantCluster, labelNode}},
		{"certificate_observation_age_seconds", "Age of last successful node inspection; -1 means never observed.", []string{labelTenantCluster, labelNode}},
		{"certificate_warning_window_seconds", "Configured certificate warning window.", nil},
		{"credential_monitoring_enabled", "One when credential inspection and CSR observation are enabled.", nil},
		{"renewal_automation_enabled", "One when automatic certificate renewal is enabled.", nil},
		{"renewal_overdue_seconds", "Seconds past the next renewal check; zero before due. Until the controller schedules the first check, it is taken as cluster creation plus one renewal interval.", []string{labelTenantCluster}},
		{"renewal_state", "Most recent certificate renewal lifecycle state.", []string{labelTenantCluster, labelState}},
		{"renewal_active_seconds", "Age of an accepted renewal cycle, zero otherwise.", []string{labelTenantCluster}},
		{"renewal_mutation_lock_retained", "One when a failed renewal still owns cluster mutation.", []string{labelTenantCluster}},
		{"serving_csr_count", "Snapshot count of pending, failed or denied kubelet-serving CSRs.", []string{labelTenantCluster, labelState}},
		{"serving_csr_oldest_pending_age_seconds", "Age of the oldest unissued serving CSR.", []string{labelTenantCluster}},
		{"serving_csr_observation_age_seconds", "Age of last successful CSR snapshot; -1 means never observed.", []string{labelTenantCluster}},
		{"etcd_key_rotation_state", "Most recent etcd data-key rotation lifecycle state.", []string{labelTenantCluster, labelState}},
		{"etcd_key_rotation_progress_age_seconds", "Seconds since phase progress while rotation is running.", []string{labelTenantCluster}},
		{"credential_metrics_collection_success", "One when credential inventory queries succeeded.", nil},
	}
	descriptors := map[string]*prometheus.Desc{}
	for _, d := range definitions {
		descriptors[d.name] = prometheus.NewDesc("nvke_controller_"+d.name, d.help, d.labels, nil)
	}
	return &CredentialCollector{database: database, logger: logger, cfg: cfg, descriptors: descriptors}
}

// Describe publishes all descriptors, including metrics whose inventory is currently empty.
func (c *CredentialCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descriptors {
		ch <- desc
	}
}

// Collect queries persisted observations and drops labels when managed objects disappear.
func (c *CredentialCollector) Collect(ch chan<- prometheus.Metric) {
	c.emit(ch, "certificate_warning_window_seconds", float64(c.cfg.CertificateWarningDays)*86400)
	c.emit(ch, "credential_monitoring_enabled", boolean(c.cfg.CredentialMonitoringEnabled))
	c.emit(ch, "renewal_automation_enabled", boolean(c.cfg.CertRenewalControllerEnabled))
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	clusters, err := c.database.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		c.logger.Error("Unable to collect credential clusters", "error", err)
		c.emit(ch, "credential_metrics_collection_success", 0)
		return
	}
	nodes, err := c.database.ListNodes(ctx, db.NodeFilters{})
	if err != nil {
		c.logger.Error("Unable to collect credential nodes", "error", err)
		c.emit(ch, "credential_metrics_collection_success", 0)
		return
	}
	c.emit(ch, "credential_metrics_collection_success", 1)
	now := time.Now()
	active := map[uuid.UUID]db.Cluster{}
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue || cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
			continue
		}
		active[cluster.ID] = cluster
		c.collectCluster(ch, cluster, now)
	}
	for _, node := range nodes {
		cluster, ok := active[node.ClusterID]
		if !ok || node.Conditions.Deleting.Status == db.ConditionStatusTrue || node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		c.collectCertificates(ch, cluster.Name, node, now)
	}
}

func (c *CredentialCollector) emit(ch chan<- prometheus.Metric, name string, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(c.descriptors[name], prometheus.GaugeValue, value, labels...)
}

func boolean(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func age(now, observed time.Time) float64 {
	if observed.IsZero() {
		return -1
	}
	return max(0, now.Sub(observed).Seconds())
}

func (c *CredentialCollector) collectCertificates(ch chan<- prometheus.Metric, cluster string, node db.Node, now time.Time) {
	observation := node.CertificateInspection
	// A failed attempt keeps the last successful ObservedAt and certificates, so
	// per-credential observations stay usable until that success ages out; only
	// inspection availability follows the latest attempt.
	observedFresh, inspectionFresh := false, false
	observedAge := -1.0
	byName := map[string]db.CertificateObservation{}
	if observation != nil {
		if observation.ObservedAt != nil {
			observedAge = age(now, *observation.ObservedAt)
			observedFresh = observedAge <= c.cfg.CertificateObservationMaxAge.Seconds()
			inspectionFresh = observation.Available && observedFresh
		}
		for _, item := range observation.Certificates {
			if certificates.Supports(item.Credential, node.ControlPlane) {
				byName[item.Credential] = item
			}
		}
	}
	c.emit(ch, "certificate_inspection_available", boolean(inspectionFresh), cluster, node.Name)
	c.emit(ch, "certificate_observation_age_seconds", observedAge, cluster, node.Name)
	for _, name := range certificates.Names(node.ControlPlane) {
		c.emit(ch, "certificate_required", boolean(certificates.Required(name)), cluster, node.Name, name)
		item, ok := byName[name]
		available := ok && item.State == "available" && !item.NotAfter.IsZero()
		c.emit(ch, "certificate_observation_available", boolean(observedFresh && available), cluster, node.Name, name)
		if available {
			c.emit(ch, "certificate_not_after_timestamp_seconds", float64(item.NotAfter.Unix()), cluster, node.Name, name)
		}
	}
}

func (c *CredentialCollector) collectCluster(ch chan<- prometheus.Metric, cluster db.Cluster, now time.Time) {
	due := cluster.CreatedAt.Add(c.cfg.CertRenewalInterval)
	if cluster.NextCertRenewalDue != nil {
		due = *cluster.NextCertRenewalDue
	}
	overdue := max(0, age(now, due))
	c.emit(ch, "renewal_overdue_seconds", overdue, cluster.Name)
	renewal := cluster.Conditions.CertificateRenewal
	state := LabelValueUnknown
	active := 0.0
	retained := false
	if renewal != nil {
		switch renewal.State {
		case db.CertificateRenewalStateAccepted, db.CertificateRenewalStateSucceeded, db.CertificateRenewalStateFailed:
			state = string(renewal.State)
		}
		if renewal.State == db.CertificateRenewalStateAccepted {
			active = max(0, age(now, renewal.StartedAt))
		}
		retained = renewal.State == db.CertificateRenewalStateFailed && cluster.Conditions.UpdateInProgress != nil && cluster.Conditions.UpdateInProgress.Type == db.OperationTypeCertRenewal
	}
	for _, candidate := range []string{LabelValueUnknown, "accepted", "succeeded", credentialStateFailed} {
		c.emit(ch, "renewal_state", boolean(state == candidate), cluster.Name, candidate)
	}
	c.emit(ch, "renewal_active_seconds", active, cluster.Name)
	c.emit(ch, "renewal_mutation_lock_retained", boolean(retained), cluster.Name)
	c.collectCSRs(ch, cluster, now)
	c.collectRotation(ch, cluster, now)
}

func (c *CredentialCollector) collectCSRs(ch chan<- prometheus.Metric, cluster db.Cluster, now time.Time) {
	observedAge := -1.0
	pendingAge := 0.0
	counts := map[string]int{"pending": 0, credentialStateFailed: 0, "denied": 0}
	if snapshot := cluster.Conditions.CredentialCSRs; snapshot != nil {
		observedAge = age(now, snapshot.ObservedAt)
		counts["pending"] = snapshot.Pending
		counts[credentialStateFailed] = snapshot.Failed
		counts["denied"] = snapshot.Denied
		if snapshot.OldestPendingAt != nil {
			pendingAge = max(0, age(now, *snapshot.OldestPendingAt))
		}
	}
	for state, count := range counts {
		c.emit(ch, "serving_csr_count", float64(count), cluster.Name, state)
	}
	c.emit(ch, "serving_csr_oldest_pending_age_seconds", pendingAge, cluster.Name)
	c.emit(ch, "serving_csr_observation_age_seconds", observedAge, cluster.Name)
}

func (c *CredentialCollector) collectRotation(ch chan<- prometheus.Metric, cluster db.Cluster, now time.Time) {
	state := LabelValueUnknown
	progressAge := 0.0
	if rotation := cluster.Conditions.EtcdKeyRotation; rotation != nil {
		switch rotation.State {
		case db.EtcdKeyRotationStateRunning, db.EtcdKeyRotationStateSucceeded, db.EtcdKeyRotationStateFailed:
			state = string(rotation.State)
		}
		if rotation.State == db.EtcdKeyRotationStateRunning {
			progressAge = age(now, rotation.LastProgressAt)
			if progressAge < 0 {
				progressAge = max(0, age(now, rotation.StartedAt))
			}
		}
	}
	for _, candidate := range []string{LabelValueUnknown, "running", "succeeded", credentialStateFailed} {
		c.emit(ch, "etcd_key_rotation_state", boolean(state == candidate), cluster.Name, candidate)
	}
	c.emit(ch, "etcd_key_rotation_progress_age_seconds", progressAge, cluster.Name)
}
