// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// OutcomeSucceeded and OutcomeFailed are the outcomes a finished phase reports.
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"

	phaseSpanPrefix  = "phase."
	phaseFinishedMsg = "phase finished"
)

// phaseState is the phase a run is in: its name and the span that times it.
type phaseState struct {
	name string
	span trace.SpanContext
}

// StartPhase begins the phase called name as a child of the run's root span and
// returns a context carrying the phase span, and a function that ends the phase.
// Until the phase ends, log records made through LogHandler carry the phase's
// span ID and a phase field. The end function takes the error that failed the
// phase, or nil, ends the span with that outcome and logs one "phase finished"
// record with the phase, its outcome and its duration in milliseconds. Calling
// it again does nothing.
//
// Phases are sequential: a run is in at most one phase, and starting a phase
// before the previous one ended replaces it. On a nil run, or one that has not
// started, StartPhase returns ctx and a function that still logs the record.
func (run *Run) StartPhase(ctx context.Context, name string) (context.Context, func(error)) {
	started := time.Now()
	var span trace.Span
	if provider := run.startedTracerProvider(); provider != nil {
		ctx, span = provider.Tracer(tracerName).Start(ctx, phaseSpanPrefix+name,
			trace.WithAttributes(attribute.String("nvke.phase", name)))
		run.setPhase(&phaseState{name: name, span: span.SpanContext()})
	}
	ended := false
	return ctx, func(err error) {
		if ended {
			return
		}
		ended = true
		outcome := OutcomeSucceeded
		if err != nil {
			outcome = OutcomeFailed
		}
		slog.Default().LogAttrs(ctx, slog.LevelInfo, phaseFinishedMsg,
			slog.String("phase", name),
			slog.String("outcome", outcome),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		if span == nil {
			return
		}
		span.SetAttributes(attribute.String("nvke.outcome", outcome))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
		run.clearPhase(name)
	}
}

// SetLogFields sets the operation and cluster fields LogHandler adds to every
// record, so loggers built before the values were known carry them too. An empty
// value leaves its field off. It is a no-op on a nil run.
func (run *Run) SetLogFields(operation, cluster string) {
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	run.operation, run.cluster = operation, cluster
}

// startedTracerProvider returns the tracer provider of a started run, or nil
// for a nil or unstarted run.
func (run *Run) startedTracerProvider() *sdktrace.TracerProvider {
	if run == nil {
		return nil
	}
	return run.activeTracerProvider()
}

func (run *Run) setPhase(state *phaseState) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.phase = state
}

// clearPhase leaves the phase called name. A later phase that replaced it stays.
func (run *Run) clearPhase(name string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.phase != nil && run.phase.name == name {
		run.phase = nil
	}
}

// logFields returns the run's current phase, operation and cluster.
func (run *Run) logFields() (phase *phaseState, operation, cluster string) {
	if run == nil {
		return nil, "", ""
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.phase, run.operation, run.cluster
}
