// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
)

// Local is an Authorizer that evaluates Cedar in this process, using the
// first-party cedar-go implementation.
//
// It reads the bundle sources as they are written — cedar-go parses a
// multi-policy .cedar document directly, so nothing assembles anything — and it
// hands the evaluator typed values rather than rendering an entity literal as
// text, so there is no quoting to get right.
//
// Immutable after construction and safe for concurrent use.
type Local struct {
	policies *cedar.PolicySet
	entities types.EntityMap

	// names maps cedar-go's positional policy id to the @id annotation the
	// source carries. Without it a diagnostic reads "policy0", which is not a
	// name anybody wrote and which changes meaning the moment a policy is
	// inserted above it — so the refusal log would point at a different rule
	// after an unrelated edit.
	names map[cedar.PolicyID]string

	// mappings is roleMappings.json, compiled, from the same load as policies
	// and entities. Keeping it on the Local is what makes a reload swap
	// policies, entities and mappings together, as one generation.
	mappings *roleMappings

	// groupOnlyWrites is Bundle.groupOnlyWrites from the same load, carried
	// so the load that publishes this Local can report it.
	groupOnlyWrites []string

	// logger receives the debug line for a role mapping expression that fails
	// to evaluate. Nil logs nothing.
	logger *slog.Logger

	// hash is sha256(policies.cedar || actions.json || roles.json ||
	// roleMappings.json) over the raw bytes this Local was built from. Used
	// only by the hot-reload path (reload.go) to decide whether a successful
	// reload's content actually changed from what was live before it -- see
	// Bundle.hash's doc comment for why it exists and what it does not claim
	// to be.
	hash [sha256.Size]byte
}

var _ Authorizer = (*Local)(nil)

func (l *Local) Name() string { return "cedar-go" }

// NewLocal loads the bundle from dir. issuerNames is the configured
// authentication issuers (auth.Authenticator.IssuerNames); a roleMappings.json
// entry naming any other issuer fails the load. Nil means no issuer, so any
// mapping entry fails.
//
// A file that is missing, unparseable, or empty is an error. That is the whole
// of the readiness story: the bundle is loaded on CWE's own startup path, so
// "it did not load" is an ordinary Go error rather than a state that has to be
// interrogated out of another process afterwards.
//
// A thin wrapper over newLocalFromSource, reading through a DirBundleSource --
// the only production BundleSource today (bundle.go). Kept as the public
// dir-string entry point because it is what every caller in this codebase
// (Setup, the hot-reload path's default loader, every test in this package)
// already calls; the seam BundleSource opens is for what NewLocal reads
// through, not for changing what callers pass it.
func NewLocal(dir string, issuerNames []string) (*Local, error) {
	return newLocalFromSource(context.Background(), DirBundleSource{Dir: dir}, newIssuerSet(issuerNames), nil)
}

// newLocalFromSource is NewLocal's real implementation, taking a BundleSource
// rather than a raw directory -- the seam reload.go's poll loop reads through
// (via reloadable.loader), and the one a future non-directory BundleSource
// (S3, a CRD) would also read through, without a second signature change
// here or in reload.go.
//
// It delegates the loading and checking to source.Load, which for
// DirBundleSource shares loadBundle (validate.go) with ValidateBundle, so the
// two cannot disagree about whether a bundle is fine. loadBundle runs the
// entity-instance checks too, so a bundle applied outside CI (a hand-edited
// ConfigMap, a kustomize apply) goes through them.
//
// The one check loadBundle cannot make is here: every roleMappings.json
// entry names an issuer in issuers. It needs the authentication config, which
// ValidateBundle does not have. Every Local is built through this function, at
// startup and on every reload, so no path skips it.
//
// logger is where the Local logs; nil logs nothing.
func newLocalFromSource(ctx context.Context, source BundleSource, issuers issuerSet, logger *slog.Logger) (*Local, error) {
	b, err := source.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.mappings.checkIssuers(issuers); err != nil {
		return nil, err
	}
	return &Local{
		policies: b.policies, entities: b.entities, names: b.names, mappings: b.mappings,
		groupOnlyWrites: b.groupOnlyWrites, logger: logger, hash: b.hash,
	}, nil
}

// Authorize evaluates one query.
//
// Every value handed to cedar-go is typed, so a subject or an identifier
// containing a quote is data rather than syntax. Query.Validate refuses one
// anyway, for a reason downstream of the evaluator: an identifier that cannot be
// written down faithfully is one whose decision cannot be audited.
func (l *Local) Authorize(_ context.Context, q Query) (Decision, error) {
	// Belt to Query.Validate's braces: the middleware validates before
	// dispatching, so reaching this means an engine was called directly.
	if err := q.Validate(); err != nil {
		return Decision{}, err
	}

	resource := types.NewEntityUID(types.EntityType(q.ResourceType), types.String(q.ResourceID))
	principalUID := types.NewEntityUID(PrincipalType, types.String(q.Principal))

	request := cedar.Request{
		Principal: principalUID,
		Action:    types.NewEntityUID(ActionType, types.String(q.Action)),
		Resource:  resource,
		Context:   requestContext(q),
	}

	// The resource entity is supplied per request rather than stored, so the
	// bundle holds roles and action groups and never has to mirror the database.
	// The principal entity is presented the same way, holding the roles this
	// bundle's mappings grant q (overlayPrincipal, below). Both are an overlay
	// rather than a copied map: the stored entities are shared across every
	// concurrent request and must not be written to.
	//
	// Claims stop here. parentsFor reads them and returns role UIDs;
	// requestContext is built from q.Issuer and q.Method only, and the
	// principal carries no attributes, so no claim reaches Cedar.
	view := principalAndResourceOverlay{
		base: l.entities,
		resource: types.Entity{
			UID:        resource,
			Attributes: types.NewRecord(nil),
		},
		principal: overlayPrincipal(l.entities, principalUID, l.mappings.parentsFor(q, l.logger)),
	}

	decision, diagnostic := cedar.Authorize(l.policies, view, request)

	reasons := make([]string, 0, len(diagnostic.Reasons))
	for _, reason := range diagnostic.Reasons {
		reasons = append(reasons, l.policyName(reason.PolicyID))
	}
	// Cedar reports evaluation errors alongside a decision. A Deny with errors
	// is still a Deny, but an Allow reached while some policy errored is not
	// something to serve on.
	if len(diagnostic.Errors) > 0 && decision == cedar.Allow {
		return Decision{}, fmt.Errorf("policy evaluation errored: %v", diagnostic.Errors)
	}

	return Decision{Allow: decision == cedar.Allow, Reasons: reasons}, nil
}

// requestContext is the Cedar context for q: its issuer and method, and
// nothing else. In particular never q.Claims, whatever keys they carry.
func requestContext(q Query) types.Record {
	return types.NewRecord(types.RecordMap{
		ContextKeyIssuer: types.String(q.Issuer),
		ContextKeyMethod: types.String(q.Method),
	})
}

// Ready reports whether the bundle is loaded and can answer for this build.
// NewLocal already refused everything below, so this is an invariant check
// rather than a network round trip — but it is the same check, from one
// function, so the gate and the loader cannot come to mean different things.
func (l *Local) Ready(context.Context) error {
	if l == nil || l.policies == nil || !hasAny(l.policies) {
		return errors.New("the policy bundle is not loaded")
	}
	return checkEntityStore(l.entities, nil, l.mappings.len())
}

// principalAndResourceOverlay presents the stored entities plus this
// request's synthesized resource and principal entities, without allocating
// a merged map per request. Both sit beside each other over the shared,
// never-written-to base map.
type principalAndResourceOverlay struct {
	base      types.EntityMap
	resource  types.Entity
	principal types.Entity
}

func (o principalAndResourceOverlay) Get(uid types.EntityUID) (types.Entity, bool) {
	if uid == o.resource.UID {
		return o.resource, true
	}
	if uid == o.principal.UID {
		return o.principal, true
	}
	return o.base.Get(uid)
}

// overlayPrincipal builds the synthesized principal entity for one request:
// uid, with no attributes, holding mapped, the roles roleMappings.json grants
// this caller. The bundle declares no principal (checkEntityStore), so nothing
// stored contributes to it.
//
// A mapped role is included only if base actually contains it; anything else
// is dropped, not passed through. cedar-go's entityInOne/entityInSet ancestry
// walk (cedar-go internal/eval/evalers.go) checks fe.Parents.Contains directly
// against the synthesized entity's own parent set -- it never calls Get on a
// parent to confirm it is itself a loaded entity. The load already refuses a
// mapping that grants a role roles.json does not declare, from the same
// generation, so this filter drops nothing in practice; it holds the invariant
// where the parent set is built.
func overlayPrincipal(base types.EntityMap, uid types.EntityUID, mapped []types.EntityUID) types.Entity {
	parents := make([]types.EntityUID, 0, len(mapped))
	for _, roleUID := range mapped {
		if _, declared := base.Get(roleUID); !declared {
			continue
		}
		parents = append(parents, roleUID)
	}

	return types.Entity{
		UID:        uid,
		Parents:    types.NewEntityUIDSet(parents...),
		Attributes: types.NewRecord(nil),
	}
}

// hasAny reports whether the set holds at least one policy. It iterates rather
// than counting, because one element is all that is needed: an empty set refuses
// every request, and how many there are beyond the first does not matter.
func hasAny(set *cedar.PolicySet) bool {
	for range set.All() {
		return true
	}
	return false
}

// policyName resolves cedar-go's positional id to the @id the source carries,
// falling back to the positional one for a policy that has no annotation.
func (l *Local) policyName(id cedar.PolicyID) string {
	if name, ok := l.names[id]; ok {
		return name
	}
	return string(id)
}
