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
	"fmt"
	"strings"
	"unicode/utf8"
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
	// Authorize answers one Query that has already passed Query.Validate. An
	// engine may assume its fields are non-empty and renderable, and must not
	// re-derive what a bad question is — see Validate for why that matters.
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

// ErrInvalidQuery marks a Query no engine should be asked — one missing a field
// it requires, or carrying one that cannot be expressed. It is a denial like any
// other, but a distinct one: it says the fault is in how the request was turned
// into a question, not in the engine or the policy.
var ErrInvalidQuery = errors.New("the authorization query is not valid")

// Validate reports whether this Query is one any engine may be asked. The
// middleware calls it before dispatching, so an engine never sees a bad one.
//
// It lives above both engines because otherwise they disagree about what a bad
// question is, and the disagreement is observable. The sidecar renders the
// identifier into Cedar source and must refuse a quote; the in-process engine
// passes typed values and would treat the same input as data, deny it on the
// merits, and report a different metric reason for the same request. AZ-4 says
// nothing outside an engine knows which engine is running, and a reason label
// that depends on which one is deployed breaks that.
//
// So the strict rule applies to every engine: an identifier a caller controls is
// refused, never escaped and never reinterpreted, whoever will evaluate it.
func (q Query) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"principal", q.Principal},
		{"action", q.Action},
		{"resource type", q.ResourceType},
		{"resource id", q.ResourceID},
	} {
		if err := checkEntityIDValue(field.value); err != nil {
			return fmt.Errorf("%w: %s %w", ErrInvalidQuery, field.name, err)
		}
	}
	return nil
}

// checkEntityIDValue is the one definition of what may appear in a Cedar entity
// literal. Query.Validate applies it across a whole query and entityUID applies
// it to one value on the way to the wire; both must agree, so neither carries
// its own copy of the rule.
//
// The returned error is a bare phrase, so a caller can name the field.
func checkEntityIDValue(value string) error {
	if value == "" {
		return errors.New("is empty")
	}
	// Before the rune scan, not after: strings.IndexFunc decodes an invalid byte
	// to U+FFFD, which is not a quote, a backslash or a control character, so
	// the scan alone lets it through. Two identifiers differing only in invalid
	// bytes are then distinct Go strings in-process and the same U+FFFD literal
	// once JSON-encoded to the sidecar — one object on one engine, two on the
	// other.
	if !utf8.ValidString(value) {
		return errors.New("is not valid UTF-8")
	}
	// Subjects arrive from verified tokens and object identifiers from URL path
	// segments the mux has already percent-decoded, so a caller does control
	// these bytes. Escaping would also work; refusing is checkable by reading
	// it, and no legitimate identifier in this API contains any of these.
	if i := strings.IndexFunc(value, func(r rune) bool {
		return r == '"' || r == '\\' || r < 0x20 || r == 0x7f
	}); i >= 0 {
		return fmt.Errorf("holds a character that cannot appear in an entity literal, at byte %d", i)
	}
	return nil
}
