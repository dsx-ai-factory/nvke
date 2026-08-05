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
	pathpkg "path"
	"strings"
)

const (
	// ManagedByHeader is the HTTP header name used to identify requests from platform controllers
	// that are managing resources via the K8s Platform API
	ManagedByHeader = "X-Managed-By"

	// PlatformAPIValue is the expected value for the X-Managed-By header when requests
	// come from K8s Platform API controllers
	PlatformAPIValue = "k8s-platform-api"

	// nodesAPIPrefix is the OpenAPI base path for node resources. Mutating requests under this
	// path are allowed without X-Managed-By when MANAGED_BY_K8S_PLATFORM is enabled.
	APIPrefixNodes = "/api/nodes"
)

// isExemptPath reports whether this request path is an API path exempt from the X-Managed-By
// requirement when MANAGED_BY_K8S_PLATFORM is enabled. Currently: /api/nodes and sub-paths only
// Mutating requests on these paths are allowed without the header but we generate a warning log
//
// Callers must pass an already-cleaned path (e.g. via path.Clean) so that dot-segments
// or duplicate slashes cannot bypass this check.
func isExemptPath(path string) bool {
	return path == APIPrefixNodes || strings.HasPrefix(path, APIPrefixNodes+"/")
}

// isOperationalPath reports whether this request path targets a health-check or metrics
// endpoint (/healthz, /readyz, /metrics). These are skipped by the managed-by middleware
//
// Callers must pass an already-cleaned path (e.g. via path.Clean) so that dot-segments
// or duplicate slashes cannot bypass this check.
func isOperationalPath(path string) bool {
	return path == "/healthz" || strings.HasPrefix(path, "/healthz/") ||
		path == "/readyz" || strings.HasPrefix(path, "/readyz/") ||
		path == "/metrics" || strings.HasPrefix(path, "/metrics/")
}

// isMutatingMethod reports whether the HTTP method mutates server state and is therefore
// subject to the managed-by header check.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// managedByMiddleware creates a middleware that checks for the X-Managed-By header
// on mutating requests (POST, PUT, PATCH, DELETE) to prevent dual source synchronization issues.
// When enforce is true, requests without the header are blocked with HTTP 409, except mutating
// requests that are exempt (currently /api/nodes).
func managedByMiddleware(enforce bool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Clean the path so dot-segments or duplicate slashes (e.g. "//healthz",
			// "/./metrics", "/api/nodes/../admin") cannot bypass the checks below.
			path := pathpkg.Clean(r.URL.Path)

			// Pass through unchecked when the feature is disabled, the method is not
			// mutating, or the path is an operational probe (health/metrics).
			if !enforce || !isMutatingMethod(r.Method) || isOperationalPath(path) {
				next.ServeHTTP(w, r)
				return
			}

			managedBy := r.Header.Get(ManagedByHeader)

			// Block non-exempt mutating requests that are missing the header.
			if managedBy == "" && !isExemptPath(path) {
				logger.Warn("mutating request missing managed-by header - blocking",
					"method", r.Method,
					"path", path,
					"remote_addr", r.RemoteAddr,
				)
				handleError(w, logger,
					"This resource may be managed by the K8s Platform API. "+
						"Direct modifications via this API could lead to synchronization conflicts. "+
						"If you are a platform controller, include the '"+ManagedByHeader+": "+PlatformAPIValue+"' header. "+
						"To disable this check, set MANAGED_BY_K8S_PLATFORM=false.",
					http.StatusConflict,
				)
				return
			}

			// Request is allowed. Either it is on an exempt path with no header
			// (audit-log it) or the header is present (validate its value).
			switch {
			// Reaching this case implies the path is exempt; non-exempt + no-header
			// requests are already blocked with 409 above.
			case managedBy == "":
				logger.Warn("allowing exempt mutating API request not originating from the K8s Platform API while MANAGED_BY_K8S_PLATFORM is enabled",
					"method", r.Method,
					"path", path,
					"remote_addr", r.RemoteAddr,
				)
			case managedBy != PlatformAPIValue:
				logger.Info("mutating request with unexpected managed-by value",
					"method", r.Method,
					"path", path,
					"managed_by", managedBy,
					"expected", PlatformAPIValue,
				)
			default:
				logger.Debug("mutating request with valid managed-by header",
					"method", r.Method,
					"path", path,
					"managed_by", managedBy,
				)
			}

			next.ServeHTTP(w, r)
		})
	}
}
