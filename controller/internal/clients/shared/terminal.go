// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared

import (
	"errors"
)

// TerminalError marks an error whose outcome cannot change by re-running the
// operation. Producers wrap with Terminal where permanence is known; the
// Temporal activity boundary converts the marker into a non-retryable
// failure. This package stays workflow-engine-free: clients declare
// permanence, adapters map it to their engine.
type TerminalError struct {
	code string
	err  error
}

// Terminal wraps err as permanent. code becomes the application-error type
// at the Temporal boundary (usable in RetryPolicy.NonRetryableErrorTypes);
// give it a stable name such as "nico.ErrImageVerificationFailed".
func Terminal(code string, err error) error {
	if err == nil {
		return nil
	}
	return &TerminalError{code: code, err: err}
}

// Error reports the wrapped error unchanged.
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
