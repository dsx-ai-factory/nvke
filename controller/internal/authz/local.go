// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// hash is sha256(policies.cedar || actions.json || roles.json) over the
	// raw bytes this Local was built from. Used only by the hot-reload path
	// (reload.go) to decide whether a successful reload's content actually
	// changed from what was live before it -- see loadedBundle.hash's doc
	// comment for why it exists and what it does not claim to be.
	hash [sha256.Size]byte
}

var _ Authorizer = (*Local)(nil)

func (l *Local) Name() string { return "cedar-go" }

// NewLocal loads the bundle from dir.
//
// A file that is missing, unparseable, or empty is an error. That is the whole
// of the readiness story: the bundle is loaded on CWE's own startup path, so
// "it did not load" is an ordinary Go error rather than a state that has to be
// interrogated out of another process afterwards.
//
// Delegates the actual loading and checking to loadBundle -- the single
// implementation this shares with ValidateBundle (validate.go), so the two
// cannot drift into agreeing a bundle is fine when the other would refuse it.
// loadBundle always runs the entity-instance checks too, which is what closes
// the gap where a bundle applied outside CI (a hand-edited ConfigMap, a
// kustomize apply) used to never go through them.
func NewLocal(dir string) (*Local, error) {
	b, errs := loadBundle(dir)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Local{policies: b.policies, entities: b.entities, names: b.names, hash: b.hash}, nil
}

// loadEntities merges one entity file into dst, refusing a duplicate: the later
// one would win, so a role redefined in the wrong file would quietly change who
// holds what.
//
// declaredIn records where each uid was first seen, and the check runs over the
// file's raw list rather than over the parsed entity map. That is the whole
// difference between catching a duplicate and not: an entity map is a map, so
// json.Unmarshal collapses two entries with the same uid to the last one before
// anything here could compare them. A roles.json repeating a principal with a
// wider set of parents therefore loaded clean and silently granted the wider
// set — the entity count gate, the schema validator and the conformance matrix
// all stayed green, because by then there was only one entity.
// Returns the raw bytes read, alongside the usual error, so a caller
// accumulating a content hash over every file this load touched (the
// hot-reload path's change-detection gauge, design-authz-hot-reload.md §5)
// does not have to re-open the file a second time.
func loadEntities(path string, dst types.EntityMap, declaredIn map[types.EntityUID]string) ([]byte, error) {
	name := filepath.Base(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	var declarations []json.RawMessage
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	for i, declaration := range declarations {
		var head struct {
			UID types.EntityUID `json:"uid"`
		}
		if err := json.Unmarshal(declaration, &head); err != nil {
			return nil, fmt.Errorf("parsing %s[%d]: %w", name, i, err)
		}
		if where, duplicate := declaredIn[head.UID]; duplicate {
			return nil, fmt.Errorf("%s[%d] redeclares %s, already declared in %s", name, i, head.UID, where)
		}
		declaredIn[head.UID] = name
	}

	var loaded types.EntityMap
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	for uid, entity := range loaded {
		dst[uid] = entity
	}
	return raw, nil
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
		Context: types.NewRecord(types.RecordMap{
			ContextKeyIssuer: types.String(q.Issuer),
			ContextKeyMethod: types.String(q.Method),
		}),
	}

	// The resource entity is supplied per request rather than stored, so the
	// bundle holds roles and action groups and never has to mirror the database.
	// The principal entity is presented the same way, with q.ClaimRoles unioned
	// into its ancestry (overlayPrincipal, below) -- an overlay rather than a
	// copied map either way: the stored entities are shared across every
	// concurrent request and must not be written to.
	view := principalAndResourceOverlay{
		base: l.entities,
		resource: types.Entity{
			UID:        resource,
			Attributes: types.NewRecord(nil),
		},
		principal: overlayPrincipal(l.entities, principalUID, q.ClaimRoles),
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

// Ready reports whether the bundle is loaded and can answer for this build.
// NewLocal already refused everything below, so this is an invariant check
// rather than a network round trip — but it is the same check, from one
// function, so the gate and the loader cannot come to mean different things.
func (l *Local) Ready(context.Context) error {
	if l == nil || l.policies == nil || !hasAny(l.policies) {
		return errors.New("the policy bundle is not loaded")
	}
	return checkEntityStore(l.entities)
}

// checkEntityStore refuses an entity store that loaded and cannot do its job.
//
// "Not empty" was the whole of the gate, and it cannot detect either state it
// was there for. The generated action entities alone satisfy it, so a roles.json
// edited to [] loads with 28 entities, passes Ready, logs "API authorization is
// enabled mode=enforcing" and then refuses every request as no_policy_match —
// reproduced. Nobody holds a role, and nothing says so.
//
// The action check is the other half, and it is about a bundle and a binary that
// were built at different times. The bundle is mounted from a ConfigMap; the
// operations table is compiled in. An image that adds an endpoint ships a table
// naming an action the mounted actions.json has no entity for, so that endpoint
// is refused for everyone while the pod reports itself ready — the mirror of the
// case above, and the reason the two are checked together here rather than left
// to whoever notices first.
//
// What this does not catch, by design — it is a shape check, not a semantic
// one, and the scope is deliberately narrower than exhaustive: a role and a
// principal can each be individually non-empty while no principal actually
// holds any role (a roles.json where every principal's parents list names a
// role that itself exists, but the two sets never intersect); a policy set that
// is present and non-empty per hasAny but is all-forbid, or is shaped so no
// request can ever match a permit, so Ready passes and every call is still
// refused; and an action entity whose UID is present, satisfying this loop, but
// whose "read"/"write" action-group parent was stripped, so the entity exists
// and the coarse-role grant that depends on that parent link does not fire.
// None of these are bugs in this gate — they are outside what a startup-time
// shape check can see without evaluating policies against sample requests —
// but they are worth naming so a future reader does not read "Ready" as a
// stronger guarantee than it is.
//
// One thing this loop does check, deliberately, is every entity's static
// Parents set: every UID an entity names as a parent must itself be a key in
// entities. cedar-go's ancestry walk (vendor cedar-go
// internal/eval/evalers.go, entityInOne/entityInSet) checks
// fe.Parents.Contains directly against an entity's own stored parent set and
// never calls Get to confirm a parent is itself loaded — overlayPrincipal's
// own doc comment already explains this for the claim-derived half of the
// principal overlay, and filters on it there. The static half never went
// through that filter: roles.json's parents are copied onto the synthesized
// principal untouched, so a principal whose static parent names a role
// nothing declares would be treated as a member of a role that was never
// loaded, with nothing at request time to catch it. Catching that here, once,
// at load, is this project's own standing convention for anything that is a
// property of the static bundle rather than of one request -- the same
// reason the role/principal counts and the action-vs-route check above run in
// this same function rather than being re-derived per call.
func checkEntityStore(entities types.EntityMap) error {
	var roles, principals int
	for uid := range entities {
		switch string(uid.Type) {
		case RoleType:
			roles++
		case PrincipalType:
			principals++
		}
	}
	if roles == 0 {
		return fmt.Errorf("%s declares no %s: every request would be refused", rolesFile, RoleType)
	}
	if principals == 0 {
		return fmt.Errorf("%s declares no %s: no caller would hold a role", rolesFile, PrincipalType)
	}

	for uid, entity := range entities {
		for parent := range entity.Parents.All() {
			if _, declared := entities.Get(parent); !declared {
				return fmt.Errorf(
					"%s names parent %s, which no entity file (%s) declares: cedar-go's ancestry walk "+
						"checks an entity's stored Parents directly and never confirms a parent is itself "+
						"loaded, so this would silently grant membership in a role that does not exist",
					uid, parent, strings.Join(entityFiles, ", "),
				)
			}
		}
	}

	// This loop is also the action/operations-table skew check that used to be
	// documented only in policy_vocabulary_test.go's old TestEveryActionAPolicyNamesExists
	// ("the action check overlaps the validator deliberately, because it also
	// asserts the generated actions.json agrees with the generated operations
	// table"): it runs unconditionally here, not gated behind the vocabulary
	// checks, so that comment's overlap claim moved with the code rather than
	// being lost when that test became a thin wrapper.
	for pattern, op := range operations {
		if _, ok := entities.Get(types.NewEntityUID(ActionType, types.String(op.Action))); !ok {
			return fmt.Errorf(
				"%s declares no %s::%q, which this build authorizes %q with: the mounted bundle is older than "+
					"the image, and every call to that endpoint would be refused",
				actionsFile, ActionType, op.Action, pattern,
			)
		}
	}
	return nil
}

// resourceOverlay presents the stored entities plus this request's resource,
// without allocating a merged map per request.
type resourceOverlay struct {
	base     types.EntityMap
	resource types.Entity
}

func (o resourceOverlay) Get(uid types.EntityUID) (types.Entity, bool) {
	if uid == o.resource.UID {
		return o.resource, true
	}
	return o.base.Get(uid)
}

// principalAndResourceOverlay extends resourceOverlay one field further: a
// synthesized principal entity sits beside the synthesized resource one, both
// presented over the shared, never-written-to base map
// (design-authz-claim-based-roles.md, §3.3).
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
// roles.json's static parents for uid (if any) unioned with one
// NKE::Role::<name> UID per claimRoles entry, so that a principal with both a
// static role and a matched claim keeps both (§4's "union, not replace").
//
// Two invariants, both required rather than incidental:
//
//   - The parent set is a new, immutable types.EntityUIDSet, built fresh from
//     base's own parents plus claimRoles -- never base's Parents reused or
//     mutated. base is shared across every concurrent request, the same
//     reason resourceOverlay never writes to it.
//   - A claimRoles entry is included only if base actually contains the
//     matching NKE::Role::<name>; anything else is dropped, not passed
//     through. cedar-go's entityInOne/entityInSet ancestry walk
//     (vendor cedar-go internal/eval/evalers.go) check fe.Parents.Contains
//     directly against the synthesized entity's own parent set -- they never
//     call Get on a parent to confirm it is itself a loaded entity. A
//     dangling or misspelled role name placed straight into Parents would
//     satisfy `principal in NKE::Role::"X"` for a role that was never loaded,
//     with nothing in cedar-go's own evaluator to catch it. Config-time
//     validation (auth.Config.Validate, checking against
//     DeclaredRoleNames) is what should catch an unknown role name in
//     config; this is what protects the case validation can't -- an issuer's
//     config was valid when written and roles.json was later edited to drop
//     the role.
func overlayPrincipal(base types.EntityMap, uid types.EntityUID, claimRoles []string) types.Entity {
	static, ok := base.Get(uid)

	parents := make([]types.EntityUID, 0, len(claimRoles))
	attrs := types.NewRecord(nil)
	if ok {
		attrs = static.Attributes
		for p := range static.Parents.All() {
			parents = append(parents, p)
		}
	}
	for _, name := range claimRoles {
		roleUID := types.NewEntityUID(RoleType, types.String(name))
		if _, declared := base.Get(roleUID); !declared {
			continue
		}
		parents = append(parents, roleUID)
	}

	return types.Entity{
		UID:        uid,
		Parents:    types.NewEntityUIDSet(parents...),
		Attributes: attrs,
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
