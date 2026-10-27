// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e_bootstrap

package bootstraptest_test

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// Temporary check, not for merging: does `bootstrap site` with an OTLP endpoint
// give one trace that holds nvkectl's spans and the temporary controller's, with
// the controller's "POST /api/clusters" under one of nvkectl's spans?

// collectedSpan is a span with the service.name of the resource it came from.
type collectedSpan struct {
	service string
	span    *tracepb.Span
}

// traceCollector is an in-process OTLP/gRPC endpoint that keeps every span it
// receives.
type traceCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []collectedSpan
}

// Export implements coltracepb.TraceServiceServer.
func (c *traceCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resourceSpans := range req.GetResourceSpans() {
		service := ""
		for _, attr := range resourceSpans.GetResource().GetAttributes() {
			if attr.GetKey() == "service.name" {
				service = attr.GetValue().GetStringValue()
			}
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				c.spans = append(c.spans, collectedSpan{service: service, span: span})
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *traceCollector) snapshot() []collectedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collectedSpan(nil), c.spans...)
}

// logsSink accepts the log records nvkectl exports and drops them.
type logsSink struct {
	collogspb.UnimplementedLogsServiceServer
}

// Export implements collogspb.LogsServiceServer.
func (*logsSink) Export(context.Context, *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// hostIPv4 returns the address this host uses to reach other machines. The
// temporary controller in the Kind cluster cannot reach a loopback address.
func hostIPv4(t *testing.T) string {
	t.Helper()
	// Dialling UDP sends nothing; it only picks the route's source address.
	if conn, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP.To4() != nil && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("listing the host's addresses: %v", err)
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil && ipNet.IP.IsGlobalUnicast() {
			return ipNet.IP.String()
		}
	}
	t.Fatal("the host has no non-loopback IPv4 address")
	return ""
}

// startTraceCollector starts a collector on every address of the host and
// returns it with the http:// endpoint to hand to nvkectl.
func startTraceCollector(t *testing.T) (*traceCollector, string) {
	t.Helper()
	lis, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listening for OTLP: %v", err)
	}
	collector := &traceCollector{}
	server := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(server, collector)
	collogspb.RegisterLogsServiceServer(server, &logsSink{})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	port := lis.Addr().(*net.TCPAddr).Port
	return collector, "http://" + net.JoinHostPort(hostIPv4(t), strconv.Itoa(port))
}

// report logs every span received, so that a run shows what arrived even when
// the check fails.
func (c *traceCollector) report(t *testing.T) {
	spans := c.snapshot()
	t.Logf("OTLP trace check: %d spans received", len(spans))
	for _, s := range spans {
		t.Logf("OTLP span: service=%q name=%q kind=%s trace=%x span=%x parent=%x",
			s.service, s.span.GetName(), s.span.GetKind(), s.span.GetTraceId(), s.span.GetSpanId(), s.span.GetParentSpanId())
	}
}

// check asserts what the first acceptance criterion of the span export asks for.
func (c *traceCollector) check(t *testing.T) {
	t.Helper()
	spans := c.snapshot()

	var rootTrace string
	nvkectlSpanIDs := map[string]bool{}
	for _, s := range spans {
		if s.service == "nvkectl" && s.span.GetName() == "nvkectl bootstrap site" {
			rootTrace = string(s.span.GetTraceId())
		}
	}
	if rootTrace == "" {
		t.Fatal(`no "nvkectl bootstrap site" span from service nvkectl reached the collector`)
	}
	for _, s := range spans {
		if s.service == "nvkectl" && string(s.span.GetTraceId()) == rootTrace {
			nvkectlSpanIDs[string(s.span.GetSpanId())] = true
		}
	}

	controllerSpans, posts := 0, 0
	for _, s := range spans {
		if s.service == "nvkectl" || string(s.span.GetTraceId()) != rootTrace {
			continue
		}
		controllerSpans++
		name := s.span.GetName()
		if !strings.Contains(name, "POST") || !strings.Contains(name, "/api/clusters") {
			continue
		}
		posts++
		if !nvkectlSpanIDs[string(s.span.GetParentSpanId())] {
			t.Errorf("the controller's %q span (service %q) has parent %x, which is not an nvkectl span of the trace",
				name, s.service, s.span.GetParentSpanId())
		}
	}
	if controllerSpans == 0 {
		t.Error("no span from the temporary controller is in nvkectl's trace")
	}
	if posts == 0 {
		t.Error(`no "POST /api/clusters" span from the temporary controller is in nvkectl's trace`)
	}
}
