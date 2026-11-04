// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// The errors Check returns, one per refusing Outcome. Each refusal wraps
// exactly one of them. An API maps ErrNotFound to 404 and the others to 403.
var (
	// ErrDenied reports OutcomeDeny. It also covers every failure Check
	// fails closed on.
	ErrDenied   = errors.New("request is not authorized")
	ErrNotFound = errors.New("resource not found")
	ErrNoTenant = errors.New("caller has no tenant")
)

// Outcomes only Check counts; a backend never returns them.
const (
	// OutcomeError counts a request Check refused without a decision, for
	// example because the backend failed.
	OutcomeError Outcome = "error"

	// OutcomeUnauthenticated counts a request with no authenticated caller,
	// which permissive authN passes on.
	OutcomeUnauthenticated Outcome = "unauthenticated"
)

// Recorder counts each Check, by outcome and action.
// It's a function so that this package doesn't depend on a metrics library.
// The API's Recorder adds the mode as a label.
type Recorder func(outcome Outcome, action string)

// Target is the action a request takes and the resource it acts on. The API
// works both out from the request. Action is also a metric label, so take it
// from the API's own list of actions, never from request input.
//
// Every existing object needs an owner tenant and a valid ID. Check refuses
// an object with no tenant for every caller, so an API with objects that
// predate tenancy must give them owners first.
type Target struct {
	Action   string
	Resource Resource

	// Missing says the object doesn't exist. Check answers ErrNotFound
	// without asking the backend.
	Missing bool
}

// Client is what an API calls to authorize a request. Check asks the backend
// for a decision, counts it, logs refusals, and on an allow returns a context
// carrying the authorized Caller. In permissive mode it lets every request
// through. It doesn't depend on HTTP or gRPC, and it's safe for concurrent use.
type Client struct {
	backend Authorizer
	mode    authn.Mode
	logger  *slog.Logger
	record  Recorder
}

// NewClient returns a Client that asks backend. logger and record may be nil.
func NewClient(backend Authorizer, mode authn.Mode, logger *slog.Logger, record Recorder) (*Client, error) {
	if backend == nil {
		return nil, errors.New("backend is nil")
	}
	if v := reflect.ValueOf(backend); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, fmt.Errorf("backend is a nil %T", backend)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if record == nil {
		record = func(Outcome, string) {}
	}
	if mode != authn.ModePermissive {
		mode = authn.ModeEnforcing
	}
	return &Client{backend: backend, mode: mode, logger: logger, record: record}, nil
}

// Check authorizes one request, for the caller that auth/authn put on ctx. On
// an allow, it returns a context carrying the authorized Caller. Otherwise it
// returns ctx unchanged and an error for the API to answer with, using
// WriteError over HTTP:
//
//   - ErrDenied, ErrNotFound or ErrNoTenant: the request is refused.
//   - An error wrapping ErrDenied: the request couldn't be decided, for
//     example because the backend failed. It's counted as OutcomeError.
//
// A request with no authenticated caller is refused, and counted as
// OutcomeUnauthenticated. Behind permissive authN, enforcing mode answers an
// unauthenticated request with 403, not 401, so enforce authN first.
//
// In permissive mode, Check counts and logs the same outcome but never returns
// an error. The context it returns carries the decision's Caller when that
// Caller has a valid tenant, and makes ListTenant list every tenant. A refused
// request may carry no Caller, such as one with no principal. Its lists still
// hold every tenant, and an API that records an owner from the Caller leaves
// the object unowned.
//
// A missing object gets ErrNotFound, the same answer a caller gets for another
// tenant's object, so the caller can't tell the two apart. That holds for every
// caller with a tenant except an operator, who can reach every tenant's
// objects. A caller with no tenant, or one the backend refuses before the
// isolation check, gets a 403 for another tenant's object.
func (c *Client) Check(ctx context.Context, t Target) (context.Context, error) {
	if c == nil || c.backend == nil {
		return ctx, fmt.Errorf("%w: client has no backend", ErrDenied)
	}
	principal, ok := authn.PrincipalFromContext(ctx)
	if !ok {
		err := fmt.Errorf("%w: no authenticated principal", ErrDenied)
		c.record(OutcomeUnauthenticated, t.Action)
		level, message := c.refusalLog()
		if c.mode != authn.ModePermissive {
			// In enforcing mode, a missing principal means authN isn't
			// enforcing on the route.
			level, message = slog.LevelError, "failed to authorize request"
		}
		c.logger.Log(ctx, level, message,
			"action", t.Action,
			"outcome", string(OutcomeUnauthenticated),
			"resource_type", t.Resource.Type,
			"resource_id", loggableID(t.Resource.ID),
			"error", err,
		)
		return c.refuse(ctx, nil, err)
	}

	if t.Missing {
		c.record(OutcomeNotFound, t.Action)
		level, message := c.refusalLog()
		c.logger.Log(ctx, level, message,
			"action", t.Action,
			"outcome", string(OutcomeNotFound),
			"missing", true,
			"issuer", principal.IssuerName,
			"subject", principal.Subject,
			"resource_type", t.Resource.Type,
			"resource_id", loggableID(t.Resource.ID),
		)
		return c.refuse(ctx, nil, ErrNotFound)
	}

	d, err := c.authorize(ctx, Query{Principal: *principal, Action: t.Action, Resource: t.Resource})
	outcome := d.Outcome
	if failedClosed(err) {
		outcome = OutcomeError
	}
	c.record(outcome, t.Action)
	if err == nil {
		return c.pass(ctx, &d.Caller), nil
	}

	level, message := c.refusalLog()
	if outcome == OutcomeError {
		level, message = slog.LevelError, "failed to authorize request"
	}
	c.logger.Log(ctx, level, message,
		"action", t.Action,
		"outcome", string(outcome),
		"issuer", principal.IssuerName,
		"subject", principal.Subject,
		"tenant", d.Caller.Tenant,
		"resource_type", t.Resource.Type,
		"resource_id", loggableID(t.Resource.ID),
		"reasons", d.Reasons,
		"error", err,
	)
	return c.refuse(ctx, &d.Caller, err)
}

// refuse returns what Check returns for a refusal: ctx and err, or in
// permissive mode a context that lets the request through. That context
// carries caller only if caller isn't nil and has a valid tenant.
func (c *Client) refuse(ctx context.Context, caller *Caller, err error) (context.Context, error) {
	if c.mode != authn.ModePermissive {
		return ctx, err
	}
	if caller != nil && CheckTenantID(caller.Tenant) != nil {
		caller = nil
	}
	return c.pass(ctx, caller), nil
}

// pass returns ctx carrying caller, if caller isn't nil. In permissive mode it
// also adds the mark that makes ListTenant list every tenant. In enforcing mode
// it hides any mark an earlier permissive Check added.
func (c *Client) pass(ctx context.Context, caller *Caller) context.Context {
	if caller != nil {
		ctx = ContextWithCaller(ctx, *caller)
	}
	return context.WithValue(ctx, permissiveContextKey{}, c.mode == authn.ModePermissive)
}

// refusalLog returns the log level and message for a refusal in c's mode.
func (c *Client) refusalLog() (slog.Level, string) {
	if c.mode == authn.ModePermissive {
		return slog.LevelWarn, "authorization would have refused this request"
	}
	return slog.LevelInfo, "refused request"
}

// loggableID returns id if Query.Validate accepts it, and "<invalid>" otherwise,
// so a refusal log never holds an oversized or unprintable ID.
func loggableID(id string) string {
	if checkString(id, maxNameBytes) != nil {
		return "<invalid>"
	}
	return id
}

// Ready reports whether the backend can answer. See Authorizer.Ready.
func (c *Client) Ready(ctx context.Context) error {
	if c == nil || c.backend == nil {
		return errors.New("client has no backend")
	}
	if err := c.backend.Ready(ctx); err != nil {
		return fmt.Errorf("%s: %w", c.backend.Name(), err)
	}
	return nil
}

// authorize asks the backend about q. It returns a nil error only for
// OutcomeAllow. Act on Decision.Caller only then.
//
// It fails closed. Each of these returns ErrDenied with OutcomeDeny, so code
// that reads only the Decision still refuses:
//
//   - an invalid query
//   - a backend error
//   - an unknown outcome
//   - an allow with a malformed caller tenant, or with none on a resource
//     that isn't global
func (c *Client) authorize(ctx context.Context, q Query) (Decision, error) {
	if err := q.Validate(); err != nil {
		return Decision{Outcome: OutcomeDeny}, fmt.Errorf("%w: %w", ErrDenied, err)
	}
	d, err := c.backend.Authorize(ctx, q)
	if err != nil {
		// Refuse in the Decision too, so code that reads only the Decision
		// still refuses. Keep the backend's Caller and Reasons for logs.
		d.Outcome = OutcomeDeny
		return d, fmt.Errorf("%w: %s: %w", ErrDenied, c.backend.Name(), hideSentinels(err))
	}
	switch d.Outcome { //nolint:exhaustive // OutcomeError fails closed in default
	case OutcomeAllow:
		// The API records Caller.Tenant as a create's owner, filters a list
		// by it and logs it. An allow must carry a valid one, or none for a
		// global resource.
		if t := d.Caller.Tenant; (t != "" || q.Resource.Scope != ScopeGlobal) && CheckTenantID(t) != nil {
			d.Outcome = OutcomeDeny
			return d, fmt.Errorf("%w: %s allowed with no valid caller tenant", ErrDenied, c.backend.Name())
		}
		return d, nil
	case OutcomeDeny:
		return d, ErrDenied
	case OutcomeNotFound:
		return d, ErrNotFound
	case OutcomeNoTenant:
		return d, ErrNoTenant
	default:
		// An unknown outcome, or one only Check counts.
		unknown := d.Outcome
		d.Outcome = OutcomeDeny
		return d, fmt.Errorf("%w: %s returned an unknown outcome %q", ErrDenied, c.backend.Name(), unknown)
	}
}

// failedClosed reports whether err means the request couldn't be decided,
// rather than being refused. authorize returns a refusal as one of its
// sentinel errors, and a failure as an error wrapping ErrDenied.
func failedClosed(err error) bool {
	switch err {
	case nil, ErrDenied, ErrNotFound, ErrNoTenant: //nolint:errorlint // only the bare sentinel is a refusal
		return false
	default:
		return true
	}
}

// hideSentinels keeps a backend error from wrapping one of this package's
// sentinels. A backend failure must not read as ErrNotFound and get a 404, or
// as ErrInvalidQuery and be blamed on the API that built the query.
func hideSentinels(err error) error {
	for _, sentinel := range []error{ErrDenied, ErrNotFound, ErrNoTenant, ErrInvalidQuery} {
		if errors.Is(err, sentinel) {
			return errors.New(err.Error())
		}
	}
	return err
}
