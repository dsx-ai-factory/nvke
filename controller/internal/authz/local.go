// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
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
// It is the alternative to CedarAgent, behind the same interface, and it reads
// the same bundle — but the bundle's *sources*, not the assembled form: cedar-go
// parses a multi-policy .cedar document directly, where the sidecar needs one
// statement per JSON entry. Nothing here renders an entity literal as text, so
// there is no quoting to get right and no identifier to refuse.
//
// Immutable after construction and safe for concurrent use.
type Local struct {
	policies *cedar.PolicySet
	entities types.EntityMap

	// names maps cedar-go's positional policy id to the @id annotation the
	// source carries. Without it a diagnostic reads "policy0", which changes
	// meaning the moment a policy is inserted above it — and the sidecar reports
	// the @id, so the two engines would log different names for the same rule.
	names map[cedar.PolicyID]string
}

var _ Authorizer = (*Local)(nil)

func (l *Local) Name() string { return "cedar-go" }

// NewLocal loads the bundle from dir.
//
// A file that is missing, unparseable, or empty is an error. That is the whole
// of the readiness story: the bundle is loaded on CWE's own startup path, so
// "it did not load" is an ordinary Go error rather than a state that has to be
// interrogated out of another process afterwards.
func NewLocal(dir string) (*Local, error) {
	if dir == "" {
		return nil, errors.New("local authorizer requires a bundle directory")
	}

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", policiesFile, err)
	}
	policies, err := cedar.NewPolicySetFromBytes(policiesFile, source)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", policiesFile, err)
	}
	if !hasAny(policies) {
		// An empty policy set refuses every request, which is indistinguishable
		// at request time from a policy nobody wrote.
		return nil, fmt.Errorf("%s declares no policies", policiesFile)
	}
	if err := checkPolicyShape(policies); err != nil {
		return nil, fmt.Errorf("%s: %w", policiesFile, err)
	}

	entities := types.EntityMap{}
	declaredIn := map[types.EntityUID]string{}
	for _, name := range entityFiles {
		if err := loadEntities(filepath.Join(dir, name), entities, declaredIn); err != nil {
			return nil, err
		}
	}
	if err := checkEntityStore(entities); err != nil {
		return nil, err
	}

	names := map[cedar.PolicyID]string{}
	for id, policy := range policies.All() {
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			names[id] = string(annotated)
		}
	}

	return &Local{policies: policies, entities: entities, names: names}, nil
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
func loadEntities(path string, dst types.EntityMap, declaredIn map[types.EntityUID]string) error {
	name := filepath.Base(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}

	var declarations []json.RawMessage
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	for i, declaration := range declarations {
		var head struct {
			UID types.EntityUID `json:"uid"`
		}
		if err := json.Unmarshal(declaration, &head); err != nil {
			return fmt.Errorf("parsing %s[%d]: %w", name, i, err)
		}
		if where, duplicate := declaredIn[head.UID]; duplicate {
			return fmt.Errorf("%s[%d] redeclares %s, already declared in %s", name, i, head.UID, where)
		}
		declaredIn[head.UID] = name
	}

	var loaded types.EntityMap
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	for uid, entity := range loaded {
		dst[uid] = entity
	}
	return nil
}

// Authorize evaluates one query.
//
// Every value handed to cedar-go is typed, so a subject or an identifier
// containing a quote is data rather than syntax. The entity-literal refusal
// CedarAgent needs has no counterpart here.
func (l *Local) Authorize(_ context.Context, q Query) (Decision, error) {
	// Belt to Query.Validate's braces: the middleware validates before
	// dispatching, so reaching this means an engine was called directly.
	if err := q.Validate(); err != nil {
		return Decision{}, err
	}

	resource := types.NewEntityUID(types.EntityType(q.ResourceType), types.String(q.ResourceID))

	request := cedar.Request{
		Principal: types.NewEntityUID(PrincipalType, types.String(q.Principal)),
		Action:    types.NewEntityUID(ActionType, types.String(q.Action)),
		Resource:  resource,
		Context: types.NewRecord(types.RecordMap{
			ContextKeyIssuer: types.String(q.Issuer),
			ContextKeyMethod: types.String(q.Method),
		}),
	}

	// The resource entity is supplied per request rather than stored, so the
	// bundle holds roles and action groups and never has to mirror the database.
	// An overlay rather than a copied map: the stored entities are shared across
	// every concurrent request and must not be written to.
	view := resourceOverlay{
		base: l.entities,
		resource: types.Entity{
			UID:        resource,
			Attributes: types.NewRecord(nil),
		},
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
// entities. cedar-go's ancestry walk (cedar-go
// internal/eval/evalers.go, entityInOne/entityInSet) checks
// fe.Parents.Contains directly against an entity's own stored parent set and
// never calls Get to confirm a parent is itself loaded. Claim-derived roles
// have the equivalent filter on their own side of the principal overlay —
// overlayPrincipal, landing later in this same capability stack
// (capability/3-role-assignment), not this branch. The static half never went
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
