// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/dsx-ai-factory/nvke/auth/internal/httperror"
)

const (
	// forbiddenMessage is the body of every 403. It gives no reason, so a
	// refused caller learns nothing about the policy.
	forbiddenMessage = "Not authorized"
)

// statusFor returns the HTTP status for err: 404 for ErrNotFound, 403 for
// ErrDenied or ErrNoTenant, and 500 for anything else, including nil. A
// request that couldn't be decided wraps ErrDenied, so it gets 403.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrDenied), errors.Is(err, ErrNoTenant):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// WriteError answers an error from Client.Check over HTTP. The response never
// contains err's text: a 404 has no body, as elsewhere in the NVKE API, and a
// 403 or 500 has the API's error body, {"message": ...}. A 500 also logs err;
// logger may be nil. Check has already logged any refusal.
func WriteError(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	status := statusFor(err)
	var message string
	switch status {
	case http.StatusForbidden:
		message = forbiddenMessage
	case http.StatusInternalServerError:
		logger.ErrorContext(r.Context(), "failed to authorize request", "error", err)
		message = http.StatusText(status)
	}
	httperror.Write(w, r, status, message, logger)
}
