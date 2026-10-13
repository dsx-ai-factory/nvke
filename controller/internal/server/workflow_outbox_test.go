// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestEnqueueWorkflowAddsResourceAttributesAndPropagatesSpan(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	clusterID := uuid.New()
	nodePoolID := uuid.New()
	attrs := otelx.WorkflowAttributes{
		WorkflowType: "ProvisionNodePool",
		WorkflowID:   "provision-node-pool-1",
		ClusterID:    clusterID,
		ClusterName:  "cluster-1",
		NodePoolID:   nodePoolID,
		NodePoolName: "workers",
	}
	input := map[string]any{"nodePoolID": nodePoolID}
	tx := &testutil.MockTx{}
	tx.On(
		"CreateWorkflowOutboxEntry",
		mock.MatchedBy(func(ctx context.Context) bool {
			return trace.SpanContextFromContext(ctx).IsValid()
		}),
		attrs.WorkflowType,
		attrs.WorkflowID,
		input,
	).Return(db.WorkflowOutbox{}, nil).Once()

	require.NoError(t, enqueueWorkflow(context.Background(), tx, attrs, input))
	tx.AssertExpectations(t)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, string(otelx.WorkflowEnqueue), spans[0].Name())

	got := make(map[string]string)
	for _, attr := range spans[0].Attributes() {
		got[string(attr.Key)] = attr.Value.AsString()
	}
	require.Equal(t, map[string]string{
		"nvke.workflow.type":  attrs.WorkflowType,
		"nvke.workflow.id":    attrs.WorkflowID,
		"nvke.cluster.id":     clusterID.String(),
		"nvke.cluster.name":   attrs.ClusterName,
		"nvke.node_pool.id":   nodePoolID.String(),
		"nvke.node_pool.name": attrs.NodePoolName,
	}, got)
}
