// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// OutboxProcessor is responsible for polling the workflow outbox table and triggering workflows.
type OutboxProcessor struct {
	cfg            config.Config
	logger         *slog.Logger
	db             db.DB
	temporalClient client.Client
	stopCh         chan struct{}
}

// NewOutboxProcessor creates a new OutboxProcessor.
func NewOutboxProcessor(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	temporalClient client.Client,
) *OutboxProcessor {
	return &OutboxProcessor{
		cfg:            cfg,
		logger:         logger,
		db:             dbInstance,
		temporalClient: temporalClient,
		stopCh:         make(chan struct{}),
	}
}

// Start begins the outbox processing loop.
func (p *OutboxProcessor) Start(ctx context.Context) {
	p.logger.Info("Starting workflow outbox processor",
		"pollingInterval", p.cfg.OutboxPollingInterval.String(),
		"jitterFactor", p.cfg.OutboxJitterFactor,
		"batchSize", p.cfg.OutboxBatchSize)

	go p.processLoop(ctx)
}

// Stop signals the processor to stop.
func (p *OutboxProcessor) Stop() {
	p.logger.Info("Stopping workflow outbox processor")
	close(p.stopCh)
}

// processLoop continuously polls the outbox table and processes entries.
func (p *OutboxProcessor) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			p.logger.Info("Context canceled, stopping workflow outbox processor")
			return
		case <-p.stopCh:
			p.logger.Info("Stop signal received, stopping workflow outbox processor")
			return
		default:
			// Add jitter to the polling interval to avoid thundering herd
			//nolint:gosec // Use of insecure random number generator is not a security issue here
			jitter := time.Duration(rand.Float64() * p.cfg.OutboxJitterFactor * float64(p.cfg.OutboxPollingInterval))
			interval := p.cfg.OutboxPollingInterval + jitter

			// Process any pending outbox entries
			if err := p.processBatch(ctx); err != nil {
				p.logger.Error("Error processing workflow outbox batch", "error", err)
			}

			// Wait for the next polling interval
			time.Sleep(interval)
		}
	}
}

// processBatch processes a batch of outbox entries.
func (p *OutboxProcessor) processBatch(ctx context.Context) error {
	// Use a transaction for the entire batch processing
	return p.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// List unprocessed entries with FOR UPDATE SKIP LOCKED to avoid duplicate processing
		entries, err := tx.ListUnprocessedWorkflowOutboxEntries(ctx, p.cfg.OutboxBatchSize)
		if err != nil {
			return fmt.Errorf("failed to list unprocessed workflow outbox entries: %w", err)
		}

		if len(entries) == 0 {
			return nil // No entries to process
		}

		p.logger.Debug("Processing workflow outbox entries", "count", len(entries))

		// Process each entry
		for _, entry := range entries {
			if err := p.processEntry(ctx, entry); err != nil {
				p.logger.Error("Failed to process workflow outbox entry",
					"error", err,
					"id", entry.ID.String(),
					"workflowType", entry.WorkflowType)
				// Continue processing other entries even if one fails
				continue
			}

			// Mark the entry as processed within the transaction
			if err := tx.MarkWorkflowOutboxEntryProcessed(ctx, entry.ID); err != nil {
				p.logger.Error("Failed to mark workflow outbox entry as processed",
					"error", err,
					"id", entry.ID.String())
				// Continue processing other entries even if marking one as processed fails
				continue
			}
		}

		return nil
	})
}

// processEntry processes a single outbox entry by starting the appropriate workflow.
func (p *OutboxProcessor) processEntry(ctx context.Context, entry db.WorkflowOutbox) error {
	p.logger.Debug("Processing workflow outbox entry",
		"id", entry.ID.String(),
		"workflowType", entry.WorkflowType,
		"workflowID", entry.WorkflowID)

	// Unmarshal the workflow input
	var inputMap map[string]interface{}
	if err := json.Unmarshal(entry.WorkflowInput, &inputMap); err != nil {
		return fmt.Errorf("failed to unmarshal workflow input: %w", err)
	}

	// Start the workflow using the workflow type name directly
	workflowOpts := client.StartWorkflowOptions{
		TaskQueue: p.cfg.TemporalTaskQueue,
		ID:        entry.WorkflowID,
	}
	we, err := p.temporalClient.ExecuteWorkflow(ctx, workflowOpts, entry.WorkflowType, inputMap)
	if err != nil {
		// A WorkflowExecutionAlreadyStarted error means a prior delivery of this same
		// outbox entry already reached Temporal successfully. Treat it as success so the
		// entry is marked processed and doesn't loop forever.
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			p.logger.Warn("Workflow already started for outbox entry, marking processed",
				"workflowID", entry.WorkflowID,
				"workflowType", entry.WorkflowType)
			return nil
		}
		return fmt.Errorf("failed to start workflow: %w", err)
	}

	p.logger.Info("Started workflow from outbox entry",
		"workflowID", we.GetID(),
		"runID", we.GetRunID(),
		"workflowType", entry.WorkflowType)

	return nil
}
