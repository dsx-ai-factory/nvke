// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WorkflowOperation identifies how a controller hands work to Temporal.
type WorkflowOperation string

const (
	workflowInstrumentationName = "github.com/dsx-ai-factory/nvke/controller/internal/otelx"

	// WorkflowStart is a direct Temporal workflow start.
	WorkflowStart WorkflowOperation = "workflow.start"
	// WorkflowEnqueue persists a workflow start in the transactional outbox.
	WorkflowEnqueue WorkflowOperation = "workflow.enqueue"
)

// WorkflowAttributes describes the workflow and the NVKE resources that caused
// it. Empty resource fields are omitted rather than recorded as zero values.
type WorkflowAttributes struct {
	WorkflowType string
	WorkflowID   string
	ClusterID    uuid.UUID
	ClusterName  string
	NodePoolID   uuid.UUID
	NodePoolName string
	NodeID       uuid.UUID
	NodeName     string
}

// StartWorkflowSpan starts an NVKE-owned span around a direct workflow start or
// an outbox enqueue. The returned context must be passed to Temporal or the
// outbox so downstream work continues the span's trace.
func StartWorkflowSpan(ctx context.Context, operation WorkflowOperation, values WorkflowAttributes) (context.Context, trace.Span) {
	return startWorkflowSpan(ctx, otel.Tracer(workflowInstrumentationName), operation, values)
}

func startWorkflowSpan(ctx context.Context, tracer trace.Tracer, operation WorkflowOperation, values WorkflowAttributes) (context.Context, trace.Span) {
	attrs := make([]attribute.KeyValue, 0, 8)
	attrs = appendStringAttribute(attrs, "nvke.workflow.type", values.WorkflowType)
	attrs = appendStringAttribute(attrs, "nvke.workflow.id", values.WorkflowID)
	attrs = appendUUIDAttribute(attrs, "nvke.cluster.id", values.ClusterID)
	attrs = appendStringAttribute(attrs, "nvke.cluster.name", values.ClusterName)
	attrs = appendUUIDAttribute(attrs, "nvke.node_pool.id", values.NodePoolID)
	attrs = appendStringAttribute(attrs, "nvke.node_pool.name", values.NodePoolName)
	attrs = appendUUIDAttribute(attrs, "nvke.node.id", values.NodeID)
	attrs = appendStringAttribute(attrs, "nvke.node.name", values.NodeName)

	//nolint:spancheck // Ownership is transferred to the caller through the returned span.
	spanCtx, span := tracer.Start(
		ctx,
		string(operation),
		trace.WithAttributes(attrs...),
	)
	return spanCtx, span //nolint:spancheck // Ownership is transferred to the caller.
}

// EndWorkflowSpan records a start or enqueue error, if any, and ends the span.
func EndWorkflowSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func appendStringAttribute(attrs []attribute.KeyValue, key, value string) []attribute.KeyValue {
	if value == "" {
		return attrs
	}
	return append(attrs, attribute.String(key, value))
}

func appendUUIDAttribute(attrs []attribute.KeyValue, key string, value uuid.UUID) []attribute.KeyValue {
	if value == uuid.Nil {
		return attrs
	}
	return append(attrs, attribute.String(key, value.String()))
}
