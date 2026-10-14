// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxUserAgentLogLength caps the logged User-Agent, which the client controls.
const (
	maxUserAgentLogLength = 256
)

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		userAgent := truncate(r.UserAgent(), maxUserAgentLogLength)

		// Create a response wrapper to capture the status code
		wrapped := responseWriter{ResponseWriter: w, status: http.StatusOK}

		level := slog.LevelInfo
		if r.Method == http.MethodGet {
			level = slog.LevelDebug
		}

		// Log the incoming request
		logger.Log(r.Context(), level, "Incoming request", "method", r.Method, "path", r.URL.Path, "user_agent", userAgent)

		// Process the request
		next.ServeHTTP(&wrapped, r)

		if wrapped.status >= 400 {
			level = slog.LevelError
		}

		// Log the completed request
		logger.Log(r.Context(), level, "Request completed",
			"event", "http.completed",
			"outcome", httpOutcome(wrapped.status),
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"user_agent", userAgent,
			"duration", time.Since(start).String())
	})
}

// truncate returns s cut to at most n bytes, without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// httpOutcome describes the HTTP response, which is separate from the later
// asynchronous workflow outcome. A 4xx is the server correctly rejecting a bad
// request, so it is "rejected"; only a 5xx is "failed", which keeps client errors
// out of dashboards that filter on outcome=failed.
func httpOutcome(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "failed"
	case status >= http.StatusBadRequest:
		return "rejected"
	default:
		return "succeeded"
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader captures the status code and passes it to the wrapped ResponseWriter.
func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
