// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/NVIDIA/nke/controller/api"
)

// Mode comes from PERMISSIVE_MODE, outside this package. Only ModePermissive
// suppresses a rejection; every other value, the zero value included, enforces.
type Mode string

const (
	// ModeEnforcing answers a failure with 401 or 403; ModePermissive counts
	// what enforcement would have refused and serves the request anyway.
	ModeEnforcing  Mode = "enforcing"
	ModePermissive Mode = "permissive"
)

// Outcome is the nke_auth_requests_total{outcome} label value, a closed set.
type Outcome string

const (
	// OutcomeAuthenticated is recorded in permissive mode too, and
	// OutcomeWouldReject is the number an operator watches before enforcing.
	OutcomeAuthenticated Outcome = "authenticated"
	OutcomeWouldReject   Outcome = "would_reject"
	OutcomeRejected      Outcome = "rejected"
)

const (
	// issuerUnknown is the {issuer} label for a failure raised before the
	// registry lookup. See Error.IssuerName.
	issuerUnknown = "unknown"

	// bearerScheme is the only scheme read. RFC 7235 makes the scheme token
	// case-insensitive, so the comparison is too.
	bearerScheme = "bearer"

	// authenticateHeader carries the challenge RFC 7235 requires on a 401.
	authenticateHeader = "WWW-Authenticate"
)

// Recorder counts one decision. A function rather than a *prometheus.CounterVec
// so this package does not depend on internal/metrics, whose RecordAuthRequest
// has this shape. reason is empty on success; issuer is a Name or issuerUnknown.
type Recorder func(outcome Outcome, reason FailureReason, issuer string)

// Authenticator turns a Verifier into HTTP middleware. Immutable after
// construction and safe for concurrent use.
type Authenticator struct {
	verifier *Verifier

	// ModePermissive suppresses both the 401 and the 403; suppressing only the
	// 401 would 403 every caller outside the allow-list.
	mode Mode

	logger *slog.Logger
	record Recorder
}

// NewAuthenticator builds the middleware around a Verifier. A nil verifier is a
// wiring bug: disabling authentication means building no Authenticator at all.
func NewAuthenticator(verifier *Verifier, mode Mode, logger *slog.Logger, record Recorder) (*Authenticator, error) {
	if verifier == nil {
		return nil, errors.New("authenticator requires a verifier")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if record == nil {
		record = func(Outcome, FailureReason, string) {}
	}
	return &Authenticator{
		verifier: verifier,
		mode:     mode,
		logger:   logger,
		record:   record,
	}, nil
}

// Wrap returns next with authentication in front of it, carrying the *Principal
// on the request context on success. It sits outside the mux rather than in the
// generated router's per-operation middleware list, which runs after routing
// alongside the OpenAPI validator, where a bad token on a malformed request would
// be answered 400.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anonymous only if both views of the path agree. See routes.go.
		escaped := r.URL.EscapedPath()
		if IsAnonymousRequestPath(escaped, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		path := normalizePath(escaped)

		principal, err := a.verifier.Authenticate(r.Context(), bearerToken(r))
		if err != nil {
			a.handleFailure(w, r, next, path, err)
			return
		}

		a.record(OutcomeAuthenticated, "", principal.IssuerName)
		// Elided for the same reason as the failure path: admitted is not the
		// same as trusted with unbounded log volume.
		a.logger.Debug("request authenticated",
			"issuer", principal.IssuerName,
			"subject", principal.Subject,
			"method", r.Method,
			"path", elideForLog(path),
		)
		next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), principal)))
	})
}

// handleFailure records the decision, then serves the request (permissive) or
// answers it (enforcing).
func (a *Authenticator) handleFailure(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	path string,
	err error,
) {
	reason := reasonOrDefault(err)
	issuer := issuerOrUnknown(err)
	status := statusFor(reason)

	// The path is caller-controlled and bounded only by MaxHeaderBytes.
	loggedPath := elideForLog(path)

	if a.mode == ModePermissive {
		a.record(OutcomeWouldReject, reason, issuer)
		a.logger.Warn("authentication would have rejected this request",
			"reason", string(reason),
			"issuer", issuer,
			"status", status,
			"method", r.Method,
			"path", loggedPath,
			"remote_addr", r.RemoteAddr,
			"error", err,
		)
		next.ServeHTTP(w, r)
		return
	}

	a.record(OutcomeRejected, reason, issuer)
	a.logger.Info("rejected request",
		"reason", string(reason),
		"issuer", issuer,
		"status", status,
		"method", r.Method,
		"path", loggedPath,
		"remote_addr", r.RemoteAddr,
		"error", err,
	)

	if status == http.StatusUnauthorized {
		w.Header().Set(authenticateHeader, "Bearer")
	}
	// The same api.GenericError body as every other error in this API;
	// internal/server.handleError is not reusable, it depends on this package.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(api.GenericError{Message: messageFor(reason)}); encErr != nil {
		a.logger.Error("failed to encode authentication error response", "error", encErr)
	}
}

// bearerToken returns the empty string for anything that is not a bearer token,
// which Verify reports as ReasonNoToken.
func bearerToken(r *http.Request) string {
	// Fields, not Cut: cutting on the first space would return "tok extra" for
	// "Bearer tok extra", which is not a token.
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], bearerScheme) {
		return ""
	}
	return parts[1]
}

// reasonOrDefault falls back to ReasonBadSignature so an unlabeled error can
// never become a 200. Unreachable through Verifier, which only returns *Error.
func reasonOrDefault(err error) FailureReason {
	if reason, ok := ReasonOf(err); ok {
		return reason
	}
	return ReasonBadSignature
}

// issuerOrUnknown falls back to issuerUnknown. Those are the only two sources of
// the label, so an arbitrary "iss" creates no new metric series.
func issuerOrUnknown(err error) string {
	if name, ok := IssuerOf(err); ok {
		return name
	}
	return issuerUnknown
}

// statusFor splits 401 from 403: admission is the one failure a caller cannot fix
// with a better token.
func statusFor(reason FailureReason) int {
	if reason == ReasonNotAdmitted {
		return http.StatusForbidden
	}
	return http.StatusUnauthorized
}

// messageFor names the reason and nothing else; the underlying error can carry
// issuer names and configuration detail, and belongs only in the log.
func messageFor(reason FailureReason) string {
	switch reason { //nolint:exhaustive // the remaining reasons share the default sentence
	case ReasonNoToken:
		return "Authentication required: no bearer token was presented."
	case ReasonNotAdmitted:
		return "Not admitted: the authenticated identity matches no admission rule."
	default:
		return "Authentication failed: " + string(reason) + "."
	}
}
