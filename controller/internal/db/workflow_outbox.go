// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
)

type WorkflowOutbox struct {
	bun.BaseModel `bun:"table:workflow_outbox"`

	ID            uuid.UUID         `bun:"type:uuid,pk,default:gen_random_uuid()"`
	WorkflowType  string            `bun:"type:text,notnull"`
	WorkflowID    string            `bun:"type:text,notnull"`
	TraceContext  map[string]string `bun:"type:jsonb"`
	WorkflowInput json.RawMessage   `bun:"type:jsonb,notnull"`
	CreatedAt     time.Time         `bun:"type:timestamptz,notnull,default:current_timestamp"`
	ProcessedAt   *time.Time        `bun:"type:timestamptz"`
}

type WorkflowOutboxQueries interface {
	CreateWorkflowOutboxEntry(ctx context.Context, workflowType, workflowID string, workflowInput interface{}) (WorkflowOutbox, error)
	ListUnprocessedWorkflowOutboxEntries(ctx context.Context, limit int) ([]WorkflowOutbox, error)
	MarkWorkflowOutboxEntryProcessed(ctx context.Context, id uuid.UUID) error
}

func (q *queriesImpl) CreateWorkflowOutboxEntry(
	ctx context.Context,
	workflowType string,
	workflowID string,
	workflowInput interface{},
) (WorkflowOutbox, error) {
	inputJSON, err := json.Marshal(workflowInput)
	if err != nil {
		return WorkflowOutbox{}, fmt.Errorf("failed to marshal workflow input: %w", err)
	}

	entry := WorkflowOutbox{
		WorkflowType:  workflowType,
		WorkflowID:    workflowID,
		WorkflowInput: inputJSON,
		TraceContext:  otelx.Capture(ctx),
	}

	err = q.db.NewInsert().Model(&entry).Scan(ctx)
	return entry, err
}

// ListUnprocessedWorkflowOutboxEntries returns unprocessed entries and locks them for update.
// The "FOR UPDATE SKIP LOCKED" ensures that in a concurrent environment, multiple workers
// won't process the same entries. Any rows already locked by another transaction are skipped.
func (q *queriesImpl) ListUnprocessedWorkflowOutboxEntries(ctx context.Context, limit int) ([]WorkflowOutbox, error) {
	var entries []WorkflowOutbox
	err := q.db.NewSelect().
		Model(&entries).
		Where("processed_at IS NULL").
		Order("created_at ASC").
		Limit(limit).
		For("UPDATE SKIP LOCKED").
		Scan(ctx)
	return entries, err
}

func (q *queriesImpl) MarkWorkflowOutboxEntryProcessed(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	_, err := q.db.NewUpdate().
		Model((*WorkflowOutbox)(nil)).
		Set("processed_at = ?", now).
		Where("id = ? AND processed_at IS NULL", id).
		Exec(ctx)
	return err
}
