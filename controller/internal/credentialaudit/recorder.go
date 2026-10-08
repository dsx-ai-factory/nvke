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
)

// Store persists a validated event, preserving the first event for a repeated ID.
type Store interface {
	RecordCredentialAuditEvent(ctx context.Context, event Event) error
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

func (r *Recorder) report(outcome, eventID string) {
	if r.observe != nil {
		r.observe(outcome)
	}
	r.logger.Error("Credential audit observation could not be persisted", "reason", outcome, "event_id", eventID)
}
