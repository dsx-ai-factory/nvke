// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package csr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// observationHeartbeat is how long an unchanged snapshot stays unwritten. The
	// observation time is what NVKEKubeletServingCSRObservationUnavailable reads
	// (stale above 300s), so it must be refreshed well inside that. Probes run every
	// minute, so an unchanged snapshot is stored every two to three minutes.
	observationHeartbeat = 2 * time.Minute
)

// NewObserverRegistrar adds periodic CSR inventory observations to each tenant
// manager. Snapshot metrics recover after restart and do not retain deleted CSRs.
// Only the credential metrics read the snapshot, so it is written to the
// wrapper's underlying database: a snapshot write must not queue a reconcile in
// every cluster-watching controller.
func NewObserverRegistrar(database *dbwrapper.DB, logger *slog.Logger) tenant.Registrar {
	return func(clusterID uuid.UUID, manager ctrl.Manager) error {
		// A synced informer cache can keep serving successful lists after API loss.
		// Freshness must come from a successful API read, rather than cached contents.
		return manager.Add(&credentialObserver{clusterID: clusterID, database: database.DB, reader: manager.GetAPIReader(), logger: logger})
	}
}

type credentialObserver struct {
	clusterID uuid.UUID
	database  db.DB
	reader    client.Reader
	logger    *slog.Logger
}

// NeedLeaderElection lets the tenant manager's lifecycle control the observer.
func (o *credentialObserver) NeedLeaderElection() bool { return false }

// Start polls a complete snapshot so unresolved and terminal CSRs are observable.
func (o *credentialObserver) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := o.observe(probeCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			o.logger.Warn("Unable to observe tenant serving CSRs", "clusterID", o.clusterID, "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (o *credentialObserver) observe(ctx context.Context) error {
	list := &certificatesv1.CertificateSigningRequestList{}
	if err := o.reader.List(ctx, list); err != nil {
		return fmt.Errorf("list serving CSRs: %w", err)
	}
	observation := summarizeServingCSRs(list.Items, time.Now().UTC())
	err := o.database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, o.clusterID)
		if err != nil {
			return err
		}
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}
		// Skip an unchanged snapshot; only the heartbeat keeps it fresh.
		if stored := cluster.Conditions.CredentialCSRs; stored != nil && sameInventory(*stored, observation) &&
			observation.ObservedAt.Sub(stored.ObservedAt) < observationHeartbeat {
			return nil
		}
		cluster.Conditions.CredentialCSRs = &observation
		// An observation is not a cluster change: leave updated_at, which
		// nvke_controller_cluster_info exports as a label, untouched.
		return tx.UpdateClusterConditions(ctx, &cluster)
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	return err
}

// sameInventory compares everything but the observation time. The oldest pending
// time is compared as an instant, as the database may return it in another zone.
func sameInventory(a, b db.CredentialCSRObservation) bool {
	if a.Pending != b.Pending || a.Failed != b.Failed || a.Denied != b.Denied {
		return false
	}
	if a.OldestPendingAt == nil || b.OldestPendingAt == nil {
		return a.OldestPendingAt == b.OldestPendingAt
	}
	return a.OldestPendingAt.Equal(*b.OldestPendingAt)
}

func summarizeServingCSRs(items []certificatesv1.CertificateSigningRequest, now time.Time) db.CredentialCSRObservation {
	observation := db.CredentialCSRObservation{ObservedAt: now}
	for _, item := range items {
		if item.Spec.SignerName != certificatesv1.KubeletServingSignerName {
			continue
		}
		approved, failed, denied := false, false, false
		for _, condition := range item.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			switch condition.Type {
			case certificatesv1.CertificateApproved:
				approved = true
			case certificatesv1.CertificateFailed:
				failed = true
			case certificatesv1.CertificateDenied:
				denied = true
			}
		}
		switch {
		case failed:
			observation.Failed++
		case denied:
			observation.Denied++
		case !approved || len(item.Status.Certificate) == 0:
			// Approval alone is not issuance. A signer failure after approval remains
			// pending until a certificate or an explicit terminal condition exists.
			observation.Pending++
			created := item.CreationTimestamp.Time
			if !created.IsZero() && (observation.OldestPendingAt == nil || created.Before(*observation.OldestPendingAt)) {
				observation.OldestPendingAt = &created
			}
		}
	}
	return observation
}
