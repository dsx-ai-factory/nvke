// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared

import (
	"errors"
)

// TerminalError marks an error whose failure cannot change by re-running the
// operation: a policy refusal, a validation verdict, an immutable-input
// mismatch. The producer — the code that knows the failure is permanent —
// wraps with Terminal; the Temporal activity boundary converts any error
// carrying this marker into a non-retryable application failure, so a
// permanent refusal fails the workflow with its reason instead of retrying
// forever.
//
// This package stays workflow-engine-free on purpose: clients declare
// permanence, adapters decide what permanence means to their engine.
type TerminalError struct {
	code string
	err  error
}

// Terminal wraps err as permanent. code becomes the application-error type at
// the Temporal boundary (and is usable in RetryPolicy.NonRetryableErrorTypes),
// so give it a stable, grep-able name such as "nico.ErrImageVerificationFailed".
func Terminal(code string, err error) error {
	if err == nil {
		return nil
	}
	return &TerminalError{code: code, err: err}
}

// Error reports the wrapped error unchanged: the marker adds semantics, not
// message noise.
func (e *TerminalError) Error() string { return e.err.Error() }

// Unwrap keeps errors.Is and errors.As working through the marker.
func (e *TerminalError) Unwrap() error { return e.err }

// Code is the stable error-type name the Temporal boundary reports.
func (e *TerminalError) Code() string { return e.code }

// AsTerminal reports the terminal marker anywhere in err's chain, if one is
// carried.
func AsTerminal(err error) (*TerminalError, bool) {
	var t *TerminalError
	if errors.As(err, &t) {
		return t, true
	}
	return nil, false
}
