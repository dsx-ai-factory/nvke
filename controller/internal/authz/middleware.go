// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/auth"
)

// Mode comes from AUTHZ_PERMISSIVE, outside this package. Only ModePermissive
// suppresses a denial; every other value, the zero value included, enforces.
type Mode string

const (
	ModeEnforcing  Mode = "enforcing"
	ModePermissive Mode = "permissive"
)

// Outcome is the nke_authz_requests_total{outcome} label value, a closed set.
type Outcome string

const (
	OutcomeAllowed     Outcome = "allowed"
	OutcomeWouldReject Outcome = "would_reject"
	OutcomeRejected    Outcome = "rejected"
)

// DenyReason is why a request was not allowed, a closed set so a caller cannot
// widen the metric label set.
type DenyReason string

const (
	// ReasonNoPolicyMatch is the ordinary denial: the engine answered, and no
	// permit applied.
	ReasonNoPolicyMatch DenyReason = "no_policy_match"

	// ReasonNoPrincipal is an authenticated-request invariant failing: the
	// middleware ran on a path that authentication did not cover.
	ReasonNoPrincipal DenyReason = "no_principal"

	// ReasonNoRoute is a request no registered pattern matches.
	ReasonNoRoute DenyReason = "no_route"

	// ReasonUnknownOperation is a route the router serves and the generated
	// table does not know.
	ReasonUnknownOperation DenyReason = "unknown_operation"

	// ReasonInvalidQuery is an identifier that cannot be put in an entity
	// literal.
	ReasonInvalidQuery DenyReason = "invalid_query"

	// ReasonEngineError is the engine unreachable, slow, or answering something
	// unreadable. CWE's failure, not the caller's — and still a denial.
	ReasonEngineError DenyReason = "engine_error"
)

// Recorder counts one decision. A function rather than a *prometheus.CounterVec
// so this package does not depend on internal/metrics, matching the shape
// internal/auth.Recorder established. reason is empty on success.
type Recorder func(outcome Outcome, reason DenyReason, action string)

// Middleware turns an Authorizer into HTTP middleware. Immutable after
// construction and safe for concurrent use.
type Middleware struct {
	authorizer Authorizer
	resolver   *Resolver
	mode       Mode
	logger     *slog.Logger
	record     Recorder
}

// NewMiddleware builds the authorization middleware. A nil authorizer or
// resolver is a wiring bug: disabling authorization means building no Middleware
// at all, never one that allows everyone.
func NewMiddleware(authorizer Authorizer, resolver *Resolver, mode Mode, logger *slog.Logger, record Recorder) (*Middleware, error) {
	if authorizer == nil {
		return nil, errors.New("authorization middleware requires an authorizer")
	}
	if resolver == nil {
		return nil, errors.New("authorization middleware requires a resolver")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if record == nil {
		record = func(Outcome, DenyReason, string) {}
	}
	return &Middleware{
		authorizer: authorizer,
		resolver:   resolver,
		mode:       mode,
		logger:     logger,
		record:     record,
	}, nil
}

// Wrap returns next with authorization in front of it.
//
// It sits outside the mux, immediately after authentication, so that it sees
// every request — including routes registered directly on the mux rather than
// through the generated router, which the generated router's per-operation
// middleware list would never run on. That position is also what makes a 403
// precede the 400 the generated wrapper would return for a malformed path
// parameter, so an unauthorized caller cannot use the difference to probe an API
// it may not call.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same anonymous surface authentication uses, from the same function
		// rather than a second list: two lists that must agree eventually will
		// not.
		if auth.IsAnonymousRequestPath(r.URL.EscapedPath(), r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			// Authentication runs first and puts a Principal on every
			// non-anonymous request it admits. No Principal here means the
			// chain is misordered, or authentication is off while this is on.
			// Neither is a state to authorize a request in.
			m.deny(w, r, next, ReasonNoPrincipal, "", nil)
			return
		}

		query, err := m.resolver.Resolve(r)
		if err != nil {
			m.deny(w, r, next, reasonForResolve(err), "", err)
			return
		}
		query.Principal = principal.Subject
		query.Issuer = principal.IssuerName

		decision, err := m.authorizer.Authorize(r.Context(), query)
		if err != nil {
			reason := ReasonEngineError
			if errors.Is(err, ErrUnsafeEntityID) {
				reason = ReasonInvalidQuery
			}
			m.deny(w, r, next, reason, query.Action, err)
			return
		}
		if !decision.Allow {
			m.deny(w, r, next, ReasonNoPolicyMatch, query.Action, nil)
			return
		}

		m.record(OutcomeAllowed, "", query.Action)
		m.logger.Debug("request authorized",
			"action", query.Action,
			"subject", query.Principal,
			"resource", query.ResourceType+"/"+query.ResourceID,
			"policies", strings.Join(decision.Reasons, ","),
		)
		next.ServeHTTP(w, r)
	})
}

// deny records the decision, then serves the request (permissive) or answers it
// (enforcing).
func (m *Middleware) deny(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	reason DenyReason,
	action string,
	cause error,
) {
	// The path is caller-controlled and bounded only by MaxHeaderBytes.
	loggedPath := elide(r.URL.Path, 256)

	if m.mode == ModePermissive {
		m.record(OutcomeWouldReject, reason, action)
		m.logger.Warn("authorization would have refused this request",
			"reason", string(reason),
			"action", action,
			"method", r.Method,
			"path", loggedPath,
			"remote_addr", r.RemoteAddr,
			"error", cause,
		)
		next.ServeHTTP(w, r)
		return
	}

	m.record(OutcomeRejected, reason, action)
	m.logger.Info("refused request",
		"reason", string(reason),
		"action", action,
		"method", r.Method,
		"path", loggedPath,
		"remote_addr", r.RemoteAddr,
		"error", cause,
	)

	// The same api.GenericError body as every other error in this API. The
	// message names the reason and nothing else: which policy matched, which
	// issuer was involved and what the engine said belong in the log.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if err := json.NewEncoder(w).Encode(api.GenericError{Message: messageFor(reason)}); err != nil {
		m.logger.Error("failed to encode authorization error response", "error", err)
	}
}

func reasonForResolve(err error) DenyReason {
	switch {
	case errors.Is(err, ErrNoRoute):
		return ReasonNoRoute
	case errors.Is(err, ErrUnknownOperation):
		return ReasonUnknownOperation
	default:
		return ReasonNoRoute
	}
}

// messageFor keeps every denial the same length of sentence, and says nothing
// about policy. A caller learns that it was refused, not what would have worked.
func messageFor(reason DenyReason) string {
	switch reason {
	case ReasonNoRoute, ReasonUnknownOperation:
		return "Not authorized: no policy covers this request."
	case ReasonEngineError, ReasonInvalidQuery, ReasonNoPrincipal:
		return "Not authorized: the request could not be evaluated."
	case ReasonNoPolicyMatch:
		return "Not authorized: no policy permits this action."
	default:
		return "Not authorized."
	}
}
