// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authz holds the NVKE API authorization: the client an API calls, the
// Authorizer interface every backend implements, and the isolation check and
// tenant ID rules every backend shares.
//
// A backend is the Authorizer a Client asks for each decision, such as
// LocalBundle (authz/localbundle), which evaluates Cedar policy from files the
// deployer mounts.
//
// The backend owns tenancy. It reads the caller's tenant, decides who is an
// operator, and refuses a cross-tenant request before any policy runs. The
// client never reads the caller's token: it checks the query, calls the
// backend, and turns the outcome into an error.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// Query is one authorization question. The API builds it from the verified
// Principal, the action its route maps to, and the resource the request
// targets.
type Query struct {
	// Principal is the caller, as auth/authn verified it.
	Principal authn.Principal

	// Action names the API operation, such as getCluster.
	Action string

	Resource Resource
}

// Scope says how a Resource relates to tenants. The zero value is invalid, so
// a query that doesn't set it is refused.
type Scope string

const (
	// ScopeObject is an existing object. Resource.Tenant is required and names
	// the tenant that owns the object. The backend lets a caller reach the object
	// only if the caller belongs to that tenant or is an operator.
	ScopeObject Scope = "object"

	// ScopeCreate is a new object. Resource.Tenant is the tenant it will
	// belong to, or empty for the caller's own tenant. In that case the API
	// records Decision.Caller.Tenant as the owner.
	ScopeCreate Scope = "create"

	// ScopeList is a collection. Resource.Tenant is empty. The API filters the
	// results to Decision.Caller.Tenant, unless Decision.Caller.Operator is
	// set.
	ScopeList Scope = "list"

	// ScopeGlobal is a resource no tenant owns, such as the list of releases.
	// Resource.Tenant is empty, and any caller may reach it, subject to policy.
	ScopeGlobal Scope = "global"
)

// Resource names the object an action acts on.
type Resource struct {
	// Type and ID name the object. A list or create names its collection, such
	// as Type "Collection" and ID "clusters".
	Type string
	ID   string

	Scope Scope

	// Tenant is the owning tenant, as Scope describes.
	Tenant string
}

// Outcome is the verdict in a Decision.
type Outcome string

const (
	// OutcomeAllow lets the request through. Every other outcome refuses it.
	OutcomeAllow Outcome = "allow"

	// OutcomeDeny refuses an action the policy doesn't allow.
	OutcomeDeny Outcome = "deny"

	// OutcomeNotFound refuses a request for another tenant's object from a
	// caller who isn't an operator. It also refuses a Resource whose Tenant
	// doesn't fit its Scope. The API answers as if the object didn't exist, so
	// a caller can't probe for other tenants' objects.
	OutcomeNotFound Outcome = "not_found"

	// OutcomeNoTenant refuses a caller with no tenant, for a resource that
	// isn't ScopeGlobal, and a caller with a malformed tenant ID, for any
	// resource.
	OutcomeNoTenant Outcome = "no_tenant"
)

// Decision is a backend's answer to one Query.
type Decision struct {
	Outcome Outcome

	// Caller is the caller's tenancy. On an allow, the API uses it to filter a
	// list, record a create's owner and log by tenant. On a refusal, it is for
	// logs only.
	Caller Caller

	// Reasons name the policies behind the outcome. The API logs them and
	// never returns them to the caller.
	Reasons []string
}

// Authorizer is the interface every backend implements. Implementations must
// be safe for concurrent use, and must pass authztest.Run.
type Authorizer interface {
	// Authorize answers one Query that has passed Query.Validate. It must call
	// CheckIsolation before any policy, and return a refusal as the Outcome
	// with a nil error. On an allow it must set Decision.Caller. An error means
	// the backend couldn't answer; the client refuses the request.
	Authorize(ctx context.Context, q Query) (Decision, error)

	// Ready reports whether the backend can answer. Call it, through
	// Client.Ready, at startup and exit on an error, so a backend that can't
	// load its policy never serves.
	Ready(ctx context.Context) error

	// Name identifies the backend in logs and in the client's errors.
	Name() string
}

// ErrInvalidQuery marks a Query no backend should be asked. The fault is in how
// the API built the query, not in the backend or its policy.
var ErrInvalidQuery = errors.New("authorization query is not valid")

// Validate reports whether q may go to a backend. It refuses a name it can't
// log exactly, rather than cleaning it. It also refuses a Resource whose
// Tenant doesn't fit its Scope, such as an object with no owner.
func (q Query) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"principal subject", q.Principal.Subject},
		{"principal issuer name", q.Principal.IssuerName},
		{"action", q.Action},
		{"resource type", q.Resource.Type},
		{"resource ID", q.Resource.ID},
	} {
		if err := checkString(field.value, maxNameBytes); err != nil {
			return fmt.Errorf("%w: %s %w", ErrInvalidQuery, field.name, err)
		}
	}
	if err := q.Resource.validateScope(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQuery, err)
	}
	return nil
}

// validateScope checks that Tenant fits Scope: an object names its owner,
// a list or a global resource names none, and a create may name one.
func (r Resource) validateScope() error {
	switch r.Scope {
	case ScopeObject:
		if r.Tenant == "" {
			return errors.New("resource tenant is empty on an object")
		}
	case ScopeCreate:
		// Tenant is optional: empty means the caller's own tenant.
	case ScopeList, ScopeGlobal:
		if r.Tenant != "" {
			return fmt.Errorf("resource tenant is set on scope %s", r.Scope)
		}
	default:
		return fmt.Errorf("resource scope %q is not object, create, list or global", r.Scope)
	}
	if r.Tenant == "" {
		return nil
	}
	if err := CheckTenantID(r.Tenant); err != nil {
		return fmt.Errorf("resource tenant %w", err)
	}
	return nil
}
