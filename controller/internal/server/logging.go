// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"log/slog"
	"net/http"
	"time"
)

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response wrapper to capture the status code
		wrapped := responseWriter{ResponseWriter: w, status: http.StatusOK}

		level := slog.LevelInfo
		if r.Method == http.MethodGet {
			level = slog.LevelDebug
		}

		// Log the incoming request
		logger.Log(r.Context(), level, "Incoming request", "method", r.Method, "path", r.URL.Path)

		// Process the request
		next.ServeHTTP(&wrapped, r)

		if wrapped.status >= 400 {
			level = slog.LevelError
		}

		// Log the completed request
		logger.Log(r.Context(), level, "Request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration", time.Since(start).String())
	})
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
