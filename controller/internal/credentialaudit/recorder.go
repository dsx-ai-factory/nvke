// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentialaudit

import (
	"context"
	"log/slog"
	"time"
)

const (
	WriteStored  = "stored"
	WriteInvalid = "invalid_event"
	WriteFailed  = "sink_failed"
	// WriteRateLimited counts CSR-derived events dropped by the per-node bound.
	// It is expected under CSR churn and is not a sink failure.
	WriteRateLimited = "rate_limited"

	// CSRAuditLimit bounds CSR-derived events per managed node within
	// CSRAuditWindow, however many CSRs a tenant creates. A valid CSR records
	// check, approve and verify, so this admits four of them per hour.
	CSRAuditLimit  = 12
	CSRAuditWindow = time.Hour
)

// Store persists a validated event, preserving the first event for a repeated ID.
type Store interface {
	RecordCredentialAuditEvent(ctx context.Context, event Event) error
}

// CSRStore persists a CSR-derived event only while its node is within the bound.
type CSRStore interface {
	RecordCSRCredentialAuditEvent(ctx context.Context, event Event, limit int, since time.Time) (bool, error)
}

// Recorder writes bounded best-effort observations. A failed sink is observable
// but cannot replace the operation result or modify its ownership state.
type Recorder struct {
	store   Store
	logger  *slog.Logger
	observe func(string)
}

// NewRecorder constructs a ledger recorder; observe receives only fixed outcomes.
func NewRecorder(store Store, logger *slog.Logger, observe func(string)) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{store: store, logger: logger, observe: observe}
}

// Record preserves operation cancellation while giving its audit write a bounded
// independent context. It emits neither raw errors nor invalid input to logs.
func (r *Recorder) Record(ctx context.Context, event Event) {
	if r == nil {
		return
	}
	validated, err := New(event)
	if err != nil {
		r.report(WriteInvalid, "")
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if r.store == nil || r.store.RecordCredentialAuditEvent(writeCtx, validated) != nil {
		r.report(WriteFailed, validated.ID)
		return
	}
	if r.observe != nil {
		r.observe(WriteStored)
	}
}

// RecordCSR records a CSR-derived event through the per-node bound. Tenants can
// create CSRs for managed nodes at will, so events beyond CSRAuditLimit within
// CSRAuditWindow are dropped and counted as WriteRateLimited.
func (r *Recorder) RecordCSR(ctx context.Context, event Event) {
	if r == nil {
		return
	}
	validated, err := New(event)
	if err != nil {
		r.report(WriteInvalid, "")
		return
	}
	store, ok := r.store.(CSRStore)
	if !ok {
		r.report(WriteFailed, validated.ID)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	stored, err := store.RecordCSRCredentialAuditEvent(writeCtx, validated, CSRAuditLimit, validated.Timestamp.Add(-CSRAuditWindow))
	switch {
	case err != nil:
		r.report(WriteFailed, validated.ID)
	case !stored:
		if r.observe != nil {
			r.observe(WriteRateLimited)
		}
		r.logger.Debug("Credential audit CSR observation dropped by the per-node bound", "event_id", validated.ID)
	case r.observe != nil:
		r.observe(WriteStored)
	}
}

func (r *Recorder) report(outcome, eventID string) {
	if r.observe != nil {
		r.observe(outcome)
	}
	r.logger.Error("Credential audit observation could not be persisted", "reason", outcome, "event_id", eventID)
}
