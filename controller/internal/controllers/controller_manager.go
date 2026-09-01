// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
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

// ControllerManager runs the cluster reconciliation loop and manages controllers
// that have not yet migrated to controller-runtime.
type ControllerManager struct {
	cfg    config.Config
	logger *slog.Logger
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
		stopCh:             make(chan struct{}),
		forgeCAPIMigration: forgeCAPIMigration,
	}, nil
}

// Start begins the controller-manager loop.
func (a *ControllerManager) Start(ctx context.Context) {
	a.logger.Info("Starting controller-manager",
		"pollingInterval", a.cfg.ControllerManagerPollingInterval.String(),
		"jitterFactor", a.cfg.ControllerManagerJitterFactor)

	go a.processLoop(ctx)
}

// Stop signals the processor to stop.
func (a *ControllerManager) Stop() {
	a.logger.Info("Stopping controller-manager")
	close(a.stopCh)
}

// processLoop continuously polls for clusters that are due for reconciliation.
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

			if err := a.reconcile(ctx); err != nil {
				a.logger.Error("Error processing reconciliation batch", "error", err)
			}

			// Wait for the next polling interval
			time.Sleep(interval)
		}
	}
}

func (a *ControllerManager) reconcile(ctx context.Context) error {
	// These Reconciles are executed sequentially just for development simplicity.
	// They are designed to be independent and may be run in parallel or separate go routines or processes in the future.
	// FIXME: We are starting to get a lot of these we should evaluate concurrency options.

	if a.forgeCAPIMigration != nil {
		return a.forgeCAPIMigration.Reconcile(ctx)
	}
	return nil
}
