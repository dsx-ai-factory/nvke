// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authz holds the NKE API authorization: the route-to-action mapping,
// the policy engines, and the middleware that turns a request into an
// authorization decision. Three states, chosen outside this package:
//
//	Disabled   AUTHZ_BUNDLE_PATH unset; the middleware is not in the chain.
//	Permissive evaluate, log and count, never reject.
//	Enforcing  403 on anything but Allow.
//
// Cedar is evaluated in this process by Local, using cedar-go. An out-of-process
// engine was built and dropped — see the design's Appendix C — so Authorizer has
// one implementation today. It stays an interface because the middleware's tests
// need a fake, and because it is what keeps the middleware from knowing anything
// about how a decision is reached.
//
// It runs immediately after auth/authn and reads the Principal that package
// put on the request context. It reads no claim for entitlement: who a caller is
// comes from the token, what they may do comes from the policy bundle.
package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dsx-ai-factory/nvke/auth/authn"
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

	// IDFormat is that parameter's declared OpenAPI format, empty for a
	// collection operation. The middleware canonicalizes the identifier
	// according to it, so that the entity a policy names is the object the
	// handler acts on rather than one of the several ways of spelling it. A
	// format the middleware does not know is a denial, not a guess.
	IDFormat string

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

	// RoleType carries a role. A principal is a member of the roles
	// roleMappings.json grants it, and roles nest, which is how one role covers
	// a set of operations.
	RoleType = "NKE::Role"

	// VariableContext is the Cedar request variable carrying the context record,
	// and the only part of a request whose attributes this engine populates.
	// checkPolicyShape refuses a policy that reads an attribute of anything
	// else, so this is the one variable name it has to know.
	VariableContext = "context"

	// CollectionType is the resource for an operation with no object identifier.
	CollectionType = "NKE::Collection"

	// ContextKeyIssuer and ContextKeyMethod are the keys of the Cedar context
	// record. Shared because two things have to agree on them: Local builds the
	// record, and hack/gen-authz-actions declares them in the schema. A policy
	// reading a key nobody sets is caught by validation, but only if the
	// spellings match here first.
	ContextKeyIssuer = "issuer"
	ContextKeyMethod = "method"
)

// Query is one authorization question. Every field is derived from something
// already verified: the Principal the authentication middleware produced, or the
// route the mux matched. Nothing here comes from an unverified claim or from an
// unrouted path.
type Query struct {
	// Principal is Principal.Subject, verbatim, and becomes the Cedar principal
	// UID. It carries no issuer, so it grants nothing by itself: every role comes
	// from a roleMappings.json entry, which names an issuer. A policy that names
	// a principal matches this string from every issuer; authn.Config.Validate
	// refuses a service-account or SPIFFE subject two issuers can both admit.
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

	// Method is the HTTP method of the route that matched, for policy that wants
	// to speak in terms of reads and writes without naming every operation.
	//
	// The route's, not the request's. They differ for a HEAD, which
	// http.ServeMux serves off a GET pattern: the action, its group and the
	// schema all describe the GET, so a context saying "HEAD" would be the one
	// field of the query disagreeing with the rest of it.
	Method string

	// Claims is authn.Principal.Claims, verbatim. Read only by the bundle's role
	// mappings, in Go, before Cedar runs (roleMappings.parentsFor,
	// role_mappings.go). Never placed in the Cedar context or on any entity.
	Claims map[string]any

	// ServiceAccount is the verified bound service account, or nil when the
	// token is not one. Set from authn.Principal.ServiceAccount, which has
	// already checked the nested kubernetes.io claim against the subject.
	//
	// An engine that ignores Claims and ServiceAccount grants no role except
	// through a spiffe mapping, which reads Principal.
	ServiceAccount *authn.ServiceAccount
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
// how a decision is reached, and nothing in an engine knows how a Query was
// built.
//
// Implementations must be safe for concurrent use. contract_test.go asserts the
// rules below against every implementation rather than describing them here and
// hoping.
//
// An error is a denial, not a pass. An engine that cannot answer must return an
// error rather than a zero-value Decision that happened to be false, so the
// middleware can tell the two apart and count them separately.
type Authorizer interface {
	// Authorize answers one Query that has already passed Query.Validate. An
	// engine may assume its fields are non-empty and renderable, and must not
	// re-derive what a bad question is — see Validate for why that matters.
	Authorize(ctx context.Context, q Query) (Decision, error)

	// Ready reports whether the engine holds a usable policy bundle. Called once
	// at startup, before serving, so a bundle that failed to load stops the
	// process rather than silently denying every request.
	Ready(ctx context.Context) error

	// Name identifies the implementation in the startup log. One value today;
	// it exists so that if a second engine is ever added, which one answered is
	// a fact the log states rather than something inferred from configuration.
	Name() string
}

// ErrInvalidQuery marks a Query no engine should be asked — one missing a field
// it requires, or carrying one that cannot be expressed. It is a denial like any
// other, but a distinct one: it says the fault is in how the request was turned
// into a question, not in the engine or the policy.
var ErrInvalidQuery = errors.New("the authorization query is not valid")

// Validate reports whether this Query is one an engine may be asked. The
// middleware calls it before dispatching, so an engine never sees a bad one.
//
// It refuses rather than sanitizes. cedar-go takes typed values, so a quote in
// an identifier is data and not syntax — the refusal is not protecting the
// evaluator. It is protecting everything downstream of the decision: the
// identifier reaches the deny log and the audit record, and an identifier
// carrying a control character or an invalid UTF-8 byte is one that cannot be
// written down faithfully. A request whose subject cannot be recorded is one
// that should not be authorized.
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

// checkEntityIDValue is the one definition of what may appear in an entity
// identifier. The returned error is a bare phrase, so a caller can name the
// field.
func checkEntityIDValue(value string) error {
	if value == "" {
		return errors.New("is empty")
	}
	// Before the rune scan, not after: strings.IndexFunc decodes an invalid byte
	// to U+FFFD, which is not a quote, a backslash or a control character, so
	// the scan alone lets it through. Two identifiers differing only in invalid
	// bytes are distinct entities to Cedar and the same U+FFFD sequence in every
	// log line and JSON record about them, which is a decision nobody can audit.
	if !utf8.ValidString(value) {
		return errors.New("is not valid UTF-8")
	}
	// Subjects arrive from verified tokens and object identifiers from URL path
	// segments the mux has already percent-decoded, so a caller does control
	// these bytes. No legitimate identifier in this API contains any of these,
	// and one that did could not be logged faithfully.
	if i := strings.IndexFunc(value, func(r rune) bool {
		return r == '"' || r == '\\' || r < 0x20 || r == 0x7f
	}); i >= 0 {
		return fmt.Errorf("holds a character that cannot appear in an entity literal, at byte %d", i)
	}
	return nil
}
