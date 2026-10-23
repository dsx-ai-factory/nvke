// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// Mode comes from authzPermissive in nke-controller-config, outside this
// package. Only ModePermissive suppresses a denial; every other value, the zero
// value included, enforces.
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

	// ReasonAmbiguousPath is a path whose escaped and decoded views name
	// different objects, so no single entity describes what the handler would
	// act on. Its own label rather than folded into invalid_query, because it
	// is the signature of someone probing for the gap between the two and an
	// operator should be able to see it without reading logs.
	ReasonAmbiguousPath DenyReason = "ambiguous_path"

	// ReasonInvalidQuery is a question the engine could not be asked: a field
	// it cannot express, or one it requires and did not get. The fault is in
	// the query, not the engine or the policy. See ErrInvalidQuery.
	ReasonInvalidQuery DenyReason = "invalid_query"

	// ReasonEngineError is the engine unreachable, slow, or answering something
	// unreadable. CWE's failure, not the caller's — and still a denial.
	ReasonEngineError DenyReason = "engine_error"
)

// Recorder counts one decision. A function rather than a *prometheus.CounterVec
// so this package does not depend on internal/metrics, matching the shape
// auth/authn.Recorder established. reason is empty on success.
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
		if authn.IsAnonymousRequestPath(r.URL.EscapedPath(), r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		principal, ok := authn.PrincipalFromContext(r.Context())
		if !ok {
			// Authentication runs first and puts a Principal on every
			// non-anonymous request it admits. No Principal here means the
			// chain is misordered, or authentication is off while this is on.
			// Neither is a state to authorize a request in.
			m.deny(w, r, next, ReasonNoPrincipal, Query{}, nil, nil)
			return
		}

		// The caller's identity first, so that a refusal on the way to naming
		// the operation is still attributable to someone. A request probing for
		// endpoints that do not exist is precisely one an operator wants a
		// subject for.
		caller := Query{Principal: principal.Subject, Issuer: principal.IssuerName, Claims: principal.Claims}
		if sa, ok := principal.ServiceAccount(); ok {
			caller.ServiceAccount = &sa
		}

		query, err := m.resolver.Resolve(r)
		if err != nil {
			m.deny(w, r, next, reasonForResolve(err), caller, nil, err)
			return
		}
		query.Principal = caller.Principal
		query.Issuer = caller.Issuer
		query.Claims = caller.Claims
		query.ServiceAccount = caller.ServiceAccount

		// Validated here rather than inside an engine, so every engine is asked
		// only questions it can answer and a bad one is reported the same way
		// whichever engine is deployed.
		if err := query.Validate(); err != nil {
			m.deny(w, r, next, ReasonInvalidQuery, query, nil, err)
			return
		}

		decision, err := m.authorizer.Authorize(r.Context(), query)
		if err != nil {
			// ErrInvalidQuery is the one error class the middleware
			// distinguishes, and every engine wraps it for the same meaning.
			// Anything else is the engine's failure rather than the question's.
			reason := ReasonEngineError
			if errors.Is(err, ErrInvalidQuery) {
				reason = ReasonInvalidQuery
			}
			m.deny(w, r, next, reason, query, nil, err)
			return
		}
		if !decision.Allow {
			m.deny(w, r, next, ReasonNoPolicyMatch, query, decision.Reasons, nil)
			return
		}

		m.record(OutcomeAllowed, "", query.Action)
		m.logger.Debug("request authorized",
			"action", query.Action,
			"subject", query.Principal,
			"issuer", query.Issuer,
			"resource", resourceOf(query),
			"policies", strings.Join(decision.Reasons, ","),
		)
		next.ServeHTTP(w, r)
	})
}

// deny records the decision, then serves the request (permissive) or answers it
// (enforcing).
//
// It takes the whole Query and the policies that decided, because a denial that
// says only "no_policy_match" is one nobody can act on. Behind a Service the
// remote address is a proxy, so before this the only identifying field in either
// the response or the log was a path — while the allow path logged the subject
// and the policies that matched, and auth/authn logged the issuer on
// rejection. And the shipped bundle carries a forbid, so a rule firing and
// nothing matching at all are the ordinary pair of outcomes; they produced a
// byte-identical line.
//
// The policy ids stay out of the metric deliberately: a label whose values come
// from the bundle is a label whose cardinality an operator changes by editing
// policy. The log is where a specific answer belongs.
func (m *Middleware) deny(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	reason DenyReason,
	query Query,
	policies []string,
	cause error,
) {
	// All three are caller-influenced: the path is bounded only by
	// MaxHeaderBytes, and the subject and the identifier come from a token and a
	// URL. A log field is not the place to discover how long they were.
	//
	// One of these is deliberately imprecise. When the reason is invalid_query
	// or ambiguous_path, the identifier logged is exactly the one that could not
	// be written down faithfully — so cause names the field and the offending
	// byte rather than leaving triage to depend on how it renders.
	attrs := []any{
		"reason", string(reason),
		"action", query.Action,
		"subject", elide(query.Principal, 256),
		"issuer", query.Issuer,
		"resource", elide(resourceOf(query), 256),
		"policies", strings.Join(policies, ","),
		"method", r.Method,
		"path", elide(r.URL.Path, 256),
		"remote_addr", r.RemoteAddr,
		"error", cause,
	}

	if m.mode == ModePermissive {
		m.record(OutcomeWouldReject, reason, query.Action)
		m.logger.Warn("authorization would have refused this request", attrs...)
		next.ServeHTTP(w, r)
		return
	}

	m.record(OutcomeRejected, reason, query.Action)
	m.logger.Info("refused request", attrs...)

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
	case errors.Is(err, ErrAmbiguousPath):
		return ReasonAmbiguousPath
	case errors.Is(err, ErrInvalidQuery):
		return ReasonInvalidQuery
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
	case ReasonEngineError, ReasonInvalidQuery, ReasonAmbiguousPath, ReasonNoPrincipal:
		return "Not authorized: the request could not be evaluated."
	case ReasonNoPolicyMatch:
		return "Not authorized: no policy permits this action."
	default:
		return "Not authorized."
	}
}

// resourceOf renders the object a query names, or nothing when the query never
// got as far as naming one.
func resourceOf(q Query) string {
	if q.ResourceType == "" {
		return ""
	}
	return q.ResourceType + "/" + q.ResourceID
}

// elide bounds a caller-controlled string before it reaches a log line. The
// request path is bounded only by MaxHeaderBytes, and a log field is not the
// place to discover that.
func elide(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
