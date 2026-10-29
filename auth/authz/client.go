// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// The errors Client.Authorize returns, one per refusing Outcome. Each refusal
// wraps exactly one of them. An API maps ErrNotFound to 404 and the others to
// 403.
var (
	// ErrDenied reports OutcomeDeny. It also covers every failure
	// Client.Authorize fails closed on.
	ErrDenied   = errors.New("request is not authorized")
	ErrNotFound = errors.New("resource not found")
	ErrNoTenant = errors.New("caller has no tenant")
)

// Client is what an API calls to authorize a request.
type Client struct {
	backend Authorizer
}

// NewClient returns a Client that asks backend.
func NewClient(backend Authorizer) (*Client, error) {
	if backend == nil {
		return nil, errors.New("backend is nil")
	}
	if v := reflect.ValueOf(backend); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, fmt.Errorf("backend is a nil %T", backend)
	}
	return &Client{backend: backend}, nil
}

// Authorize asks the backend about q. It returns a nil error only for
// OutcomeAllow. Act on Decision.Caller only then.
//
// It fails closed. Each of these returns ErrDenied with OutcomeDeny, so code
// that reads only the Decision still refuses:
//
//   - a nil client
//   - an invalid query
//   - a backend error
//   - an unknown outcome
//   - an allow with a malformed caller tenant, or with none on a resource
//     that isn't global
func (c *Client) Authorize(ctx context.Context, q Query) (Decision, error) {
	if c == nil || c.backend == nil {
		return Decision{Outcome: OutcomeDeny}, fmt.Errorf("%w: client has no backend", ErrDenied)
	}
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
	switch d.Outcome {
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
		unknown := d.Outcome
		d.Outcome = OutcomeDeny
		return d, fmt.Errorf("%w: %s returned an unknown outcome %q", ErrDenied, c.backend.Name(), unknown)
	}
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
