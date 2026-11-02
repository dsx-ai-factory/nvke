// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httperror writes the NVKE API's error response for auth/authn and
// auth/authz.
package httperror

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Write answers r with status and the API's error body, {"message": message}.
// An empty message writes the status with no body, like every 404 in the API.
// Set any other header, such as WWW-Authenticate, before calling it. A failure
// to write the body is logged to logger, which may be nil.
func Write(w http.ResponseWriter, r *http.Request, status int, message string, logger *slog.Logger) {
	if message == "" {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body{Message: message}); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.ErrorContext(r.Context(), "failed to encode error response", "status", status, "error", err)
	}
}

// body is the API's error shape, api.GenericError in controller/api, declared
// here so this module doesn't depend on the controller's API module. The
// controller's auth wiring test checks that the two stay the same.
type body struct {
	// Message says what went wrong.
	Message string `json:"message"`
}
