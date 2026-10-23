// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

	// mappings is roleMappings.json, compiled, from the same load as policies
	// and entities. Keeping it on the Local is what makes a reload swap
	// policies, entities and mappings together, as one generation.
	mappings *roleMappings

	// groupOnlyWrites is loadedBundle.groupOnlyWrites from the same load,
	// carried so the load that publishes this Local can report it.
	groupOnlyWrites []string

	// logger receives the debug line for a role mapping expression that fails
	// to evaluate. Nil logs nothing.
	logger *slog.Logger
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
func NewLocal(dir string, issuerNames []string) (*Local, error) {
	return newLocal(dir, newIssuerSet(issuerNames), nil)
}

// newLocal is NewLocal's implementation, taking the issuer names as a set and
// the logger the Local logs to; nil logs nothing. Setup builds its engine
// through here so the Local logs where the rest of authorization does.
//
// It delegates the loading and checking to loadBundle, the implementation it
// shares with ValidateBundle (validate.go), so the two cannot disagree about
// whether a bundle is fine. loadBundle runs the entity-instance checks too, so
// a bundle applied outside CI (a hand-edited ConfigMap, a kustomize apply) goes
// through them.
//
// The one check loadBundle cannot make is here: every roleMappings.json
// entry names an issuer in issuers. It needs the authentication config, which
// ValidateBundle does not have.
func newLocal(dir string, issuers issuerSet, logger *slog.Logger) (*Local, error) {
	b, errs := loadBundle(dir)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := b.mappings.checkIssuers(issuers); err != nil {
		return nil, err
	}
	return &Local{
		policies: b.policies, entities: b.entities, names: b.names, mappings: b.mappings,
		groupOnlyWrites: b.groupOnlyWrites, logger: logger,
	}, nil
}

// loadEntities merges one entity file into dst, refusing a duplicate: the later
// one would win, so a role redefined in the wrong file would quietly change who
// holds what.
//
// declaredIn records where each uid was first seen, and the check runs over the
// file's raw list rather than over the parsed entity map. That is the whole
// difference between catching a duplicate and not: an entity map is a map, so
// json.Unmarshal collapses two entries with the same uid to the last one before
// anything here could compare them. A roles.json repeating a role with a
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

const (
	// mappingCountUnknown tells checkEntityStore that roleMappings.json failed
	// to read or decode, so it does not add a misleading "has no entries" on
	// top of the error already reported.
	mappingCountUnknown = -1
)

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
// one, and the scope is deliberately narrower than exhaustive: a
// roleMappings.json whose entries match no caller that ever arrives; a policy
// set that is present and non-empty per hasAny but is all-forbid, or is shaped
// so no request can ever match a permit, so Ready passes and every call is
// still refused; and an action entity whose UID is present, satisfying this
// loop, but whose "read"/"write" action-group parent was stripped, so the
// entity exists and the coarse-role grant that depends on that parent link
// does not fire.
// None of these are bugs in this gate — they are outside what a startup-time
// shape check can see without evaluating policies against sample requests —
// but they are worth naming so a future reader does not read "Ready" as a
// stronger guarantee than it is.
//
// One thing this loop does check, deliberately, is every entity's stored
// Parents set: every UID an entity names as a parent must itself be a key in
// entities. cedar-go's ancestry walk (cedar-go
// internal/eval/evalers.go, entityInOne/entityInSet) checks
// fe.Parents.Contains directly against an entity's own stored parent set and
// never calls Get to confirm a parent is itself loaded — overlayPrincipal
// (below) explains this for the principal, and filters on it there. A role's
// parents in roles.json never go through that filter, so a role whose parent
// names a role nothing declares would make its holders members of a role that
// was never loaded, with nothing at request time to catch it. Catching that
// here, once, at load, is this project's own standing convention for anything
// that is a property of the static bundle rather than of one request -- the
// same reason the role and mapping counts and the action-vs-route check run in
// this same function rather than being re-derived per call.
//
// Every grant lives in roleMappings.json, where each entry names an issuer, so
// the store holds no NKE::Principal. A principal entity would hold its roles
// for its subject string from every issuer. declaredIn maps each entity to the
// file that declared it, for the error; nil names no file. mappingEntries is
// how many roleMappings.json entries the same bundle holds, and it must be at
// least one. mappingCountUnknown skips that rule, for a file whose failure to
// load is already reported.
func checkEntityStore(entities types.EntityMap, declaredIn map[types.EntityUID]string, mappingEntries int) error {
	var roles int
	principals := map[string][]string{}
	for uid := range entities {
		switch string(uid.Type) {
		case RoleType:
			roles++
		case PrincipalType:
			file := declaredIn[uid]
			if file == "" {
				file = "an entity file"
			}
			principals[file] = append(principals[file], uid.String())
		}
	}
	if roles == 0 {
		return fmt.Errorf("%s declares no %s: every request would be refused", rolesFile, RoleType)
	}
	if len(principals) > 0 {
		var errs []error
		for _, file := range slices.Sorted(maps.Keys(principals)) {
			errs = append(errs, fmt.Errorf("%s declares %s, which would hold its roles for that subject from every "+
				"issuer; remove it and grant its roles with a %s entry naming the caller's issuer",
				file, capList(principals[file], maxListed), roleMappingsFile))
		}
		return errors.Join(errs...)
	}
	if mappingEntries == 0 {
		return fmt.Errorf("%s has no entries: no caller would hold a role. Add an entry granting a role to a "+
			"caller from an issuer in AuthenticationConfiguration, in %s", roleMappingsFile, roleMappingsSource)
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

	// This loop is also the action/operations-table skew check: it asserts the
	// generated actions.json agrees with the generated operations table. It
	// overlaps checkActionVocabulary deliberately and runs unconditionally, not
	// gated behind the vocabulary checks.
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

const (
	// maxListed is how many items capList names before summarizing the rest.
	maxListed = 5
)

// capList sorts items and joins at most limit of them, then "and N more".
func capList(items []string, limit int) string {
	slices.Sort(items)
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:limit], ", "), len(items)-limit)
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
