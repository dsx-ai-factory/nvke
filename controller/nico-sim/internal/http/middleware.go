// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/state"
)

// maxRequestBodyBytes limits the size of incoming request bodies (1 MiB).
const maxRequestBodyBytes = 1 << 20

// authMiddleware rejects requests that lack a Bearer token, except for the
// unauthenticated health, file-serving, and UI paths.
func (s *Server) authMiddleware(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/files/") || strings.HasPrefix(r.URL.Path, "/ui/") {
			next.ServeHTTP(w, r)
			return
		}
		if !hasBearerAuth(r) {
			writeForgeError(w, nethttp.StatusUnauthorized, "missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs requests whose method is present in s.logMethods.
// A nil or empty map disables request logging entirely.
func (s *Server) loggingMiddleware(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: nethttp.StatusOK}
		next.ServeHTTP(rw, r)

		if !s.logMethods[r.Method] {
			return
		}
		s.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start),
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	nethttp.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(nethttp.Flusher); ok {
		f.Flush()
	}
}

// ── Helper functions ─────────────────────────────────────────────────────────

// hasBearerAuth checks whether the request carries a non-empty Bearer token.
func hasBearerAuth(r *nethttp.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) != ""
}

// writeForgeError writes a standard Forge-shaped error response.
func writeForgeError(w nethttp.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{
		"source":  "forge",
		"message": message,
		"data":    nil,
	})
}

// writeJSON serialises payload as JSON and writes it with the given status.
func writeJSON(w nethttp.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Default().Error("failed to encode JSON response", "error", err)
	}
}

// parsePage reads pageNumber and pageSize from the query string.
func parsePage(r *nethttp.Request) state.Page {
	page := state.Page{Number: 1, Size: 100}
	if v := r.URL.Query().Get("pageNumber"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page.Number = n
		}
	}
	if v := r.URL.Query().Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page.Size = n
		}
	}
	return page
}

// orgIDFromRequest extracts the organisation identifier from path values.
// It checks the v1 key "orgId" first, then the v2 key "org".
func orgIDFromRequest(r *nethttp.Request) string {
	if v := r.PathValue("orgId"); v != "" {
		return v
	}
	return r.PathValue("org")
}

// siteIDForList returns the optional siteId after verifying that it belongs to
// the organization in the request path. On lookup failure it writes the Forge
// error response and returns false, so callers must stop without writing a
// second response.
func (s *Server) siteIDForList(w nethttp.ResponseWriter, r *nethttp.Request) (string, bool) {
	siteID := r.URL.Query().Get("siteId")
	if siteID == "" {
		return "", true
	}
	if _, err := s.store.GetSiteForOrg(r.Context(), orgIDFromRequest(r), siteID); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Site with specified ID")
		} else {
			writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		}
		return "", false
	}
	return siteID, true
}
