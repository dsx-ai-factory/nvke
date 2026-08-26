// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authz holds the NKE API authorization: the route-to-action mapping,
// the policy engines, and the middleware that turns a request into an
// authorization decision. Three states, chosen outside this package:
//
//	Disabled   neither AUTHZ_BUNDLE_PATH nor AUTHZ_ENDPOINT set; the
//	           middleware is not in the chain.
//	Permissive evaluate, log and count, never reject.
//	Enforcing  403 on anything but Allow.
//
// Two engines implement Authorizer: Local evaluates in this process with
// cedar-go, and CedarAgent calls a cedar-agent sidecar. Exactly one runs, and
// both are held to the conformance matrix in conformance_test.go.
//
// It runs immediately after internal/auth and reads the Principal that package
// put on the request context. It reads no claim for entitlement: who a caller is
// comes from the token, what they may do comes from the policy bundle.
package authz

import (
	"context"
	"errors"
)

// The action vocabulary is generated from the same OpenAPI document the router
// is generated from, so the two cannot drift. actions_test.go asserts parity in
// both directions, which is what turns "somebody forgot to regenerate" from a
// route nobody can call into a failing build.
//
// The bundle's generated half — the Cedar schema and the action entities — comes
// out of the same pass, because both describe the same set of operations.
//
//go:generate go run ../../hack/gen-authz-actions -spec ../../api/openapi.yaml -out actions.gen.go -bundle ../../deploy/authz

// Operation is one row of the generated table in actions.gen.go: what a route
// pattern authorizes.
type Operation struct {
	// Action is the OpenAPI operationId, which is the Cedar action name.
	Action string

	// ResourceType is the Cedar entity type of the object acted on, empty for a
	// collection operation.
	ResourceType string

	// IDParam is the path parameter carrying the object's identifier, empty for
	// a collection operation.
	IDParam string

	// Collection is the first path segment, naming the NKE::Collection entity a
	// collection operation is authorized against.
	Collection string
}

// Entity type and namespace constants. The namespace is fixed rather than
// configurable: it appears in every policy, and a deployment that could change
// it would make policy non-portable for no gain.
const (
	// PrincipalType carries the verified subject, whatever its shape: a SPIFFE
	// ID, or system:serviceaccount:<ns>:<name>.
	PrincipalType = "NKE::Principal"

	// ActionType carries the operationId.
	ActionType = "NKE::Action"

	// CollectionType is the resource for an operation with no object identifier.
	CollectionType = "NKE::Collection"

	// ContextKeyIssuer and ContextKeyMethod are the keys of the Cedar context
	// record. Shared because three things have to agree on them: both engines
	// build the record, and hack/gen-authz-actions declares them in the schema.
	// A policy reading a key nobody sets is caught by validation, but only if
	// the spellings match here first.
	ContextKeyIssuer = "issuer"
	ContextKeyMethod = "method"
)

// Query is one authorization question. Every field is derived from something
// already verified: the Principal the authentication middleware produced, or the
// route the mux matched. Nothing here comes from an unverified claim or from an
// unrouted path.
type Query struct {
	// Principal is Principal.Subject, verbatim.
	Principal string

	// Issuer is Principal.IssuerName — the operator's label for the issuer entry,
	// not the "iss" claim. Stable across an issuer URL change, and a closed set
	// fixed at startup, which is what makes it safe to put in a policy.
	Issuer string

	// Action is the operationId.
	Action string

	// ResourceType and ResourceID name the object acted on.
	ResourceType string
	ResourceID   string

	// Method is the HTTP verb, for policy that wants to speak in terms of reads
	// and writes without naming every operation.
	Method string
}

// Decision is one answer. Allow is the only value that lets a request through;
// every other outcome, an error included, is a denial.
type Decision struct {
	Allow bool

	// Reasons are the policy IDs that produced the decision. Diagnostic only:
	// they are logged, never returned to the caller.
	Reasons []string
}

// Authorizer evaluates one Query against a policy bundle. It is the whole of the
// contract between the middleware and an engine: nothing outside this file knows
// which engine is running, and nothing in an engine knows how a Query was built.
//
// Implementations must be safe for concurrent use, and must answer the same way
// for the same bundle — the conformance matrix in conformance_test.go is what
// "the same way" means, and every implementation is run against it.
//
// An error is a denial, not a pass. Every implementation therefore fails towards
// refusing: an engine that cannot answer must return an error rather than a
// zero-value Decision that happened to be false, so the middleware can tell the
// two apart and count them separately.
type Authorizer interface {
	// Authorize answers one Query. An error that the caller should classify as a
	// malformed question rather than an engine failure must wrap ErrInvalidQuery.
	Authorize(ctx context.Context, q Query) (Decision, error)

	// Ready reports whether the engine holds a usable policy bundle. Called once
	// at startup, before serving, so a bundle that failed to load stops the
	// process rather than silently denying every request.
	Ready(ctx context.Context) error

	// Name identifies the implementation for logs: "cedar-go", "cedar-agent".
	// Which engine made a decision is the first thing anyone asks when a
	// decision surprises them, and asking the engine beats inferring it from
	// whichever option was set.
	Name() string
}

// ErrInvalidQuery marks a Query an engine could not ask — one carrying a field
// it cannot express, or missing one it requires. It is a denial like any other,
// but a distinct one: it says the fault is in how the request was turned into a
// question, not in the engine or the policy.
//
// Engines wrap it rather than defining their own sentinel, so the middleware
// classifies the reason without knowing which engine produced it.
var ErrInvalidQuery = errors.New("the authorization query is not valid")
