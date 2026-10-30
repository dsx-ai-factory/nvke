// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoCaller means code that needs the authorized caller found none on the
// context, or found one with no valid tenant. It's a bug in the API's wiring,
// not a refusal, so the API answers it with 500.
var ErrNoCaller = errors.New("request has no authorized caller with a valid tenant")

// callerContextKey is unexported so no other package can collide with it.
type callerContextKey struct{}

// ContextWithCaller returns a copy of ctx carrying c as the authorized caller.
// Client.Check calls it on an allow. Elsewhere, use it only in tests.
func ContextWithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// CallerFromContext returns the authorized caller on ctx, and false if there
// is none.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerContextKey{}).(Caller)
	return c, ok
}

// ListTenant says which tenant's objects a list may hold. It returns the
// caller's tenant and false, or "" and true for an operator, who sees every
// tenant. It returns ErrNoCaller if ctx has no authorized caller, or the
// caller has no valid tenant. On an error it returns "" and false, so a caller
// that drops the error lists nothing.
func ListTenant(ctx context.Context) (string, bool, error) {
	c, ok := CallerFromContext(ctx)
	if !ok {
		return "", false, ErrNoCaller
	}
	if err := CheckTenantID(c.Tenant); err != nil {
		return "", false, fmt.Errorf("%w: caller tenant %w", ErrNoCaller, err)
	}
	if c.Operator {
		return "", true, nil
	}
	return c.Tenant, false, nil
}
