// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestTransportDialPropagatesTraceContext(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	traceparents := make(chan string, 2)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(
		ctx context.Context,
		req any,
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		incoming, _ := metadata.FromIncomingContext(ctx)
		values := incoming.Get("traceparent")
		if len(values) == 0 {
			traceparents <- ""
		} else {
			traceparents <- values[0]
		}
		return handler(ctx, req)
	}))
	healthpb.RegisterHealthServer(server, health.NewServer())

	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	transport := &Transport{cfg: config.Config{NodeAgent: config.NodeAgentConfig{Port: port}}}
	conn, cleanup, err := transport.Dial(context.Background(), db.Node{IP: host})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	client := healthpb.NewHealthClient(conn)

	parentCtx, parent := provider.Tracer("transport-test").Start(context.Background(), "parent")
	parentSpanContext := parent.SpanContext()
	_, err = client.Check(parentCtx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	parent.End()
	propagatedSpanContext := spanContextFromTraceparent(t, receiveTraceparent(t, traceparents))
	assert.Equal(t, parentSpanContext.TraceID(), propagatedSpanContext.TraceID())
	assert.NotEqual(t, parentSpanContext.SpanID(), propagatedSpanContext.SpanID())

	_, err = client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	rootSpanContext := spanContextFromTraceparent(t, receiveTraceparent(t, traceparents))
	assert.NotEqual(t, parentSpanContext.TraceID(), rootSpanContext.TraceID())
}

func receiveTraceparent(t *testing.T, traceparents <-chan string) string {
	t.Helper()
	select {
	case traceparent := <-traceparents:
		return traceparent
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for traceparent metadata")
		return ""
	}
}

func spanContextFromTraceparent(t *testing.T, traceparent string) trace.SpanContext {
	t.Helper()
	require.NotEmpty(t, traceparent)
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent})
	spanContext := trace.SpanContextFromContext(ctx)
	require.True(t, spanContext.IsValid())
	return spanContext
}
