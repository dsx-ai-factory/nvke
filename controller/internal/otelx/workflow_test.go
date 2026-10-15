// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestWorkflowSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeID := uuid.New()
	ctx, span := startWorkflowSpan(context.Background(), provider.Tracer(workflowInstrumentationName), WorkflowStart, WorkflowAttributes{
		WorkflowType: "ProvisionNode",
		WorkflowID:   "provision-node-1",
		ClusterID:    clusterID,
		ClusterName:  "cluster-1",
		NodePoolID:   nodePoolID,
		NodeID:       nodeID,
		NodeName:     "node-1",
	})
	require.True(t, span.SpanContext().IsValid())
	require.Equal(t, span.SpanContext(), trace.SpanContextFromContext(ctx))

	startErr := errors.New("temporal unavailable")
	EndWorkflowSpan(span, startErr)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, string(WorkflowStart), spans[0].Name())
	require.Equal(t, "Error", spans[0].Status().Code.String())

	got := make(map[string]string)
	for _, attr := range spans[0].Attributes() {
		got[string(attr.Key)] = attr.Value.AsString()
	}
	require.Equal(t, map[string]string{
		"nvke.workflow.type": "ProvisionNode",
		"nvke.workflow.id":   "provision-node-1",
		"nvke.cluster.id":    clusterID.String(),
		"nvke.cluster.name":  "cluster-1",
		"nvke.node_pool.id":  nodePoolID.String(),
		"nvke.node.id":       nodeID.String(),
		"nvke.node.name":     "node-1",
	}, got)
}
