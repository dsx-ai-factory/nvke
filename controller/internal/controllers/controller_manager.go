// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kube"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	temporalactivities "github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

// ControllerManager runs the site reconciliation loop and manages controllers
// that have not yet migrated to controller-runtime.
type ControllerManager struct {
	cfg    config.Config
	logger *slog.Logger
	db     db.DB
	stopCh chan struct{}

	forgeCAPIMigration *ForgeCAPIMigrationController
}

// NewControllerManager creates a new ControllerManager.
func NewControllerManager(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	forgeProvider *forge.ForgeProvider,
	nodeAgentClient agent.Client,
	tenantClientProvider *kube.DBClusterTenantClientProvider,
	trustDomain string,
) (*ControllerManager, error) {
	controllerManagerLogger := logger.With("logger", "controller-manager")

	var forgeCAPIMigration *ForgeCAPIMigrationController
	if cfg.EnableForgeCapi {
		activities := temporalactivities.NewActivities(
			cfg,
			dbInstance,
			k8sClient,
			nodeAgentClient,
			forgeProvider,
			trustDomain,
			tenantClientProvider,
		)
		forgeCAPIMigration = NewForgeCAPIMigrationController(controllerManagerLogger, dbInstance, k8sClient, activities)
	}

	return &ControllerManager{
		cfg:                cfg,
		logger:             controllerManagerLogger,
		db:                 dbInstance,
		stopCh:             make(chan struct{}),
		forgeCAPIMigration: forgeCAPIMigration,
	}, nil
}

// Start begins the controller-manager loop.
func (a *ControllerManager) Start(ctx context.Context) {
	a.logger.Info("Starting controller-manager",
		"pollingInterval", a.cfg.ControllerManagerPollingInterval.String(),
		"jitterFactor", a.cfg.ControllerManagerJitterFactor,
		"batchSize", a.cfg.ControllerManagerBatchSize)

	go a.processLoop(ctx)
}

// Stop signals the processor to stop.
func (a *ControllerManager) Stop() {
	a.logger.Info("Stopping controller-manager")
	close(a.stopCh)
}

// processLoop continuously polls for sites that are due for reconciliation.
func (a *ControllerManager) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			a.logger.Info("Context canceled, stopping controller-manager")
			return
		case <-a.stopCh:
			a.logger.Info("Stop signal received, stopping controller-manager")
			return
		default:
			// Add jitter to the polling interval to avoid thundering herd
			//nolint:gosec // Use of insecure random number generator is not a security issue here
			jitter := time.Duration(rand.Float64() * a.cfg.ControllerManagerJitterFactor * float64(a.cfg.ControllerManagerPollingInterval))
			interval := a.cfg.ControllerManagerPollingInterval + jitter

			// Process any sites due for reconciliation
			if err := a.processBatch(ctx); err != nil {
				a.logger.Error("Error processing reconciliation batch", "error", err)
			}

			// Wait for the next polling interval
			time.Sleep(interval)
		}
	}
}

// processBatch processes sites due for reconciliation, up to ControllerManagerBatchSize.
//
// Each site is claimed in its own short transaction (FOR UPDATE SKIP LOCKED +
// UPDATE next_reconcile_due to a future lease time) that commits immediately.
// Reconciliation then runs outside any transaction, so the row lock is held
// for only a few milliseconds per site. This prevents concurrent API calls
// that also lock the site row from being blocked by slow Temporal/Forge work.
func (a *ControllerManager) processBatch(ctx context.Context) error {
	for range a.cfg.ControllerManagerBatchSize {
		site, err := a.claimSiteForReconcile(ctx)
		if err != nil {
			return fmt.Errorf("failed to claim site for reconciliation: %w", err)
		}
		if site == nil {
			return nil // no more sites due
		}

		a.processSite(ctx, *site)

		// Advance the schedule now that reconciliation is done.
		nextDue := time.Now().Add(a.cfg.ControllerManagerInterval)
		if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
			return tx.UpdateSiteReconcileDue(ctx, site.ID, nextDue)
		}); err != nil {
			a.logger.Error("Failed to update site reconciliation due time",
				"error", err,
				"siteID", site.ID.String())
		}
	}
	return nil
}

// claimSiteForReconcile selects one site due for reconciliation using
// FOR UPDATE SKIP LOCKED and immediately bumps its next_reconcile_due to
// act as a lease, preventing other workers from picking it up. The
// transaction commits before any reconcile work begins.
// Returns nil when no site is due.
func (a *ControllerManager) claimSiteForReconcile(ctx context.Context) (*db.Site, error) {
	var claimed *db.Site
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		sites, err := tx.ListSitesDueForReconcile(ctx, 1)
		if err != nil {
			return fmt.Errorf("failed to list sites due for reconciliation: %w", err)
		}
		if len(sites) == 0 {
			return nil
		}
		site := sites[0]
		if err := tx.UpdateSiteReconcileDue(ctx, site.ID, time.Now().Add(a.cfg.ControllerManagerInterval)); err != nil {
			return fmt.Errorf("failed to set reconciliation lease: %w", err)
		}
		claimed = &site
		return nil
	})
	return claimed, err
}

// processSite processes a single site by executing the underlying controllers.
func (a *ControllerManager) processSite(ctx context.Context, site db.Site) {
	a.logger.Debug("Processing site for reconciliation", "siteID", site.ID.String())

	// These Reconciles are executed sequentially just for development simplicity.
	// They are designed to be independent and may be run in parallel or separate go routines or processes in the future.
	// FIXME: We are starting to get a lot of these we should evaluate concurrency options.

	if a.forgeCAPIMigration != nil {
		if err := a.forgeCAPIMigration.Reconcile(ctx, site); err != nil {
			// Log errors but continue processing the other controllers.
			a.logger.Error("Failed to reconcile Forge CAPI migration", "error", err, "siteID", site.ID.String())
		}
	}
}
