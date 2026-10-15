// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
)

func enqueueWorkflow(ctx context.Context, tx db.Tx, attrs otelx.WorkflowAttributes, input any) error {
	workflowCtx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowEnqueue, attrs)
	_, err := tx.CreateWorkflowOutboxEntry(workflowCtx, attrs.WorkflowType, attrs.WorkflowID, input)
	otelx.EndWorkflowSpan(span, err)
	return err
}
