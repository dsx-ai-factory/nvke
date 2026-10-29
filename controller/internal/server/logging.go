// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxUserAgentLogLength caps the logged User-Agent, which the client controls.
const (
	maxUserAgentLogLength = 256

	rejectionReasonRequestParsingFailed     = "request_parsing_failed"
	rejectionReasonRequestValidationFailed  = "request_validation_failed"
	rejectionReasonManagedByHeaderRequired  = "managed_by_header_required"
	rejectionReasonClusterNameAlreadyExists = "cluster_name_already_exists"
	rejectionReasonInvalidClusterNetwork    = "invalid_cluster_network"
)

type requestLogStateKey struct{}

type requestLogState struct {
	rejectionReason string
}

func (s *requestLogState) recordRejectionReason(reason string) {
	if s.rejectionReason == "" {
		s.rejectionReason = reason
	}
}

func requestLogStateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestLogState{}
		ctx := context.WithValue(r.Context(), requestLogStateKey{}, state)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		userAgent := truncate(r.UserAgent(), maxUserAgentLogLength)

		state := requestLogStateFromContext(r.Context())
		if state == nil {
			state = &requestLogState{}
		}
		wrapped := responseWriter{ResponseWriter: w, status: http.StatusOK, requestLogState: state}

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

		attrs := []any{
			"event", "http.completed",
			"outcome", httpOutcome(wrapped.status),
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"user_agent", userAgent,
			"duration", time.Since(start).String(),
		}
		if state.rejectionReason != "" {
			attrs = append(attrs, "reason", state.rejectionReason)
		}

		// Log the completed request
		logger.Log(r.Context(), level, "Request completed", attrs...)
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
	status          int
	requestLogState *requestLogState
	wroteHeader     bool
}

// WriteHeader captures the status code and passes it to the wrapped ResponseWriter.
func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		rw.ResponseWriter.WriteHeader(code)
		return
	}
	rw.wroteHeader = true
	rw.status = code
	rw.recordRejectionReason(httpRejectionReason(code))
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	rw.wroteHeader = true
	return rw.ResponseWriter.Write(p)
}

func httpRejectionReason(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case http.StatusTooManyRequests:
		return "too_many_requests"
	default:
		if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
			return "client_error"
		}
		return ""
	}
}

type rejectionReasonRecorder interface {
	recordRejectionReason(reason string)
}

func (rw *responseWriter) recordRejectionReason(reason string) {
	rw.requestLogState.recordRejectionReason(reason)
}

// recordRejectionReason adds a stable reason to the request completion log
// without copying client-controlled response text into persistent logs.
func recordRejectionReason(w http.ResponseWriter, reason string) {
	if recorder, ok := w.(rejectionReasonRecorder); ok {
		recorder.recordRejectionReason(reason)
	}
}

// recordRequestRejectionReason lets business handlers classify a rejection
// without copying their client-facing response text into persistent logs.
func recordRequestRejectionReason(ctx context.Context, reason string) {
	if state := requestLogStateFromContext(ctx); state != nil {
		state.recordRejectionReason(reason)
	}
}

func requestLogStateFromContext(ctx context.Context) *requestLogState {
	state, _ := ctx.Value(requestLogStateKey{}).(*requestLogState)
	return state
}
