// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authz holds the NKE API authorization: the route-to-action mapping,
// the policy-engine client, and the middleware that turns a request into an
// authorization decision. Three states, chosen outside this package:
//
//	Disabled   AUTHZ_ENDPOINT unset; the middleware is not in the chain.
//	Permissive evaluate, log and count, never reject.
//	Enforcing  403 on anything but Allow.
//
// It runs immediately after internal/auth and reads the Principal that package
// put on the request context. It reads no claim for entitlement: who a caller is
// comes from the token, what they may do comes from the policy bundle.
package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// Authorizer evaluates one Query. Implementations must be safe for concurrent
// use. An error is a denial, not a pass: see middleware.go.
type Authorizer interface {
	Authorize(ctx context.Context, q Query) (Decision, error)

	// Ready reports whether the engine holds a usable policy bundle. Called once
	// at startup, before serving, so a bundle that failed to load stops the
	// process rather than silently denying every request. See NewAuthorizer.
	Ready(ctx context.Context) error
}

// ErrUnsafeEntityID is returned for an identifier that cannot be rendered into a
// Cedar entity literal without ambiguity.
var ErrUnsafeEntityID = errors.New("identifier is not safe in an entity literal")

// entityUID renders a Cedar entity literal, `Type::"id"`.
//
// The identifier is refused rather than escaped when it contains a quote, a
// backslash or a control character. Subjects arrive from verified tokens and
// object identifiers from URL path segments, which http.ServeMux has already
// percent-decoded — so a caller does control these bytes, and a quote landing
// unescaped inside a literal is how a request rewrites the query it is the
// subject of. Escaping would work, but refusing is checkable by reading it, and
// no legitimate identifier in this API contains any of these.
func entityUID(entityType, id string) (string, error) {
	if entityType == "" {
		return "", errors.New("entity type is empty")
	}
	if id == "" {
		return "", errors.New("entity id is empty")
	}
	if i := strings.IndexFunc(id, unsafeInEntityID); i >= 0 {
		return "", fmt.Errorf("%w: byte %d", ErrUnsafeEntityID, i)
	}
	return entityType + `::"` + id + `"`, nil
}

func unsafeInEntityID(r rune) bool {
	return r == '"' || r == '\\' || r < 0x20 || r == 0x7f
}
