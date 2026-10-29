// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package localbundle is the LocalBundle authorization backend. It evaluates
// Cedar policy from a bundle of files the deployer mounts, usually from a
// ConfigMap: the policies, the roles, the actions and the role mappings.
//
// A caller holds the roles that matching role mappings grant. LocalBundle
// gives no caller a tenant, so it allows only a request for a global
// resource.
package localbundle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// The Cedar names a policy uses. Every entity type is in the NVKE namespace:
// a Resource of Type "Cluster" has the entity type NVKE::Cluster.
const (
	namespace     = "NVKE"
	principalType = namespace + "::Principal"
	actionType    = namespace + "::Action"
	roleType      = namespace + "::Role"

	// contextIssuer is the one attribute of the Cedar context: the name of
	// the caller's issuer in the authN config.
	contextIssuer = "issuer"
)

// Options configures a LocalBundle.
type Options struct {
	// Dir holds the bundle files: policies.cedar, roles.json, actions.json
	// and roleMappings.json. A mounted ConfigMap changes them all at once. A
	// plain directory must be changed the same way, or a reload can load
	// files from two versions.
	Dir string

	// Issuers are the issuer names in the authN config. The load fails if a
	// role mapping names any other issuer, because the mapping would match
	// nobody.
	Issuers []string

	// Actions are the actions the API asks about. New fails if actions.json
	// lacks one, because the bundle would refuse that action to every caller,
	// or makes one an action group. Authorize refuses any other action.
	Actions []string

	// ResourceTypes are the types of the resources the API asks about, such
	// as "Cluster" and "Collection". A policy may name only these, the
	// principal, actions and roles, so a misspelled type can't make a forbid
	// silently never apply.
	ResourceTypes []string
}

// LocalBundle is a backend that evaluates the live bundle. Reload replaces the
// live bundle with a new load that passes every check. It is safe for
// concurrent use.
type LocalBundle struct {
	opts Options
	live atomic.Pointer[bundle]

	// reloading serializes reloads, so a slower load can't replace a newer
	// one.
	reloading sync.Mutex
}

var _ authz.Authorizer = (*LocalBundle)(nil)

// New loads and checks the bundle in opts.Dir. Unless a file can't be read
// or parsed, the error reports every problem found.
func New(opts Options) (*LocalBundle, error) {
	if len(opts.Actions) == 0 || len(opts.ResourceTypes) == 0 {
		return nil, errors.New("the API's actions and resource types must both be set")
	}
	for _, t := range opts.ResourceTypes {
		if t == "" || slices.Contains([]string{principalType, actionType, roleType}, namespace+"::"+t) {
			return nil, fmt.Errorf("resource type %q is empty or names the principal, an action or a role", t)
		}
	}
	b, err := load(opts)
	if err != nil {
		return nil, err
	}
	lb := &LocalBundle{opts: opts}
	lb.live.Store(b)
	return lb, nil
}

// Reload loads the bundle again. If the new load passes every check, it
// replaces the live bundle; otherwise the live bundle stays. It reports
// whether the bundle's files changed. An API calls it on an interval and on
// demand.
func (lb *LocalBundle) Reload() (bool, error) {
	if lb == nil {
		return false, errors.New("no bundle is loaded")
	}
	lb.reloading.Lock()
	defer lb.reloading.Unlock()

	b, err := load(lb.opts)
	if err != nil {
		return false, err
	}
	return lb.live.Swap(b).hash != b.hash, nil
}

// Name returns "localBundle".
func (*LocalBundle) Name() string { return "localBundle" }

// Ready reports whether a bundle is loaded.
func (lb *LocalBundle) Ready(context.Context) error {
	_, err := lb.bundle()
	return err
}

// bundle returns the live bundle, or an error when none is loaded.
func (lb *LocalBundle) bundle() (*bundle, error) {
	if lb == nil {
		return nil, errors.New("no bundle is loaded")
	}
	b := lb.live.Load()
	if b == nil {
		return nil, errors.New("no bundle is loaded")
	}
	return b, nil
}

// Authorize runs the isolation check, grants roles from the role mappings,
// then evaluates the policies.
func (lb *LocalBundle) Authorize(_ context.Context, q authz.Query) (authz.Decision, error) {
	b, err := lb.bundle()
	if err != nil {
		return authz.Decision{}, err
	}
	if err := q.Validate(); err != nil {
		return authz.Decision{}, err
	}
	// Every caller has no tenant and isn't an operator.
	var caller authz.Caller
	if refusal := authz.CheckIsolation(caller, q.Resource); refusal != "" {
		return authz.Decision{Outcome: refusal, Caller: caller}, nil
	}
	return b.decide(q, b.mappings.rolesFor(q.Principal))
}

// decide evaluates q against the policies, with the principal holding roles.
func (b *bundle) decide(q authz.Query, roles []types.EntityUID) (authz.Decision, error) {
	if !slices.Contains(b.resourceTypes, q.Resource.Type) {
		return authz.Decision{}, fmt.Errorf("resource type %q isn't one of the API's resource types", q.Resource.Type)
	}
	if !slices.Contains(b.actions, q.Action) {
		return authz.Decision{}, fmt.Errorf("action %q isn't one of the API's actions", q.Action)
	}
	action := types.NewEntityUID(actionType, types.String(q.Action))

	principal := types.NewEntityUID(principalType, types.String(q.Principal.Subject))
	resource := types.NewEntityUID(types.EntityType(namespace+"::"+q.Resource.Type), types.String(q.Resource.ID))
	request := cedar.Request{
		Principal: principal,
		Action:    action,
		Resource:  resource,
		Context:   types.NewRecord(types.RecordMap{contextIssuer: types.String(q.Principal.IssuerName)}),
	}
	view := requestEntities{
		stored:    b.entities,
		principal: types.Entity{UID: principal, Parents: types.NewEntityUIDSet(roles...), Attributes: types.NewRecord(nil)},
		resource:  types.Entity{UID: resource, Parents: types.NewEntityUIDSet(), Attributes: types.NewRecord(nil)},
	}
	decision, diagnostic := cedar.Authorize(b.policies, view, request)

	// Cedar skips a policy whose condition errors. An allow beside an error
	// may be one a skipped forbid would have stopped. A deny stays a deny.
	if decision == cedar.Allow && len(diagnostic.Errors) > 0 {
		msgs := make([]string, 0, len(diagnostic.Errors))
		for _, e := range diagnostic.Errors {
			msgs = append(msgs, fmt.Sprintf("policy %s failed to evaluate: %s", b.policyName(e.PolicyID), e.Message))
		}
		slices.Sort(msgs)
		return authz.Decision{}, errors.New(strings.Join(msgs, "\n"))
	}
	d := authz.Decision{Outcome: authz.OutcomeDeny}
	if decision == cedar.Allow {
		d.Outcome = authz.OutcomeAllow
	}
	for _, reason := range diagnostic.Reasons {
		d.Reasons = append(d.Reasons, b.policyName(reason.PolicyID))
	}
	slices.Sort(d.Reasons)
	return d, nil
}

// requestEntities is the stored entities plus the request's principal and
// resource, without copying the stored map, which every request shares.
// Neither can replace a stored entity: stored entities are actions and roles,
// and the resource type can be neither.
type requestEntities struct {
	stored    types.EntityMap
	principal types.Entity
	resource  types.Entity
}

// Get returns the request's principal or resource, and looks up any other
// entity in the bundle.
func (e requestEntities) Get(uid types.EntityUID) (types.Entity, bool) {
	switch uid {
	case e.principal.UID:
		return e.principal, true
	case e.resource.UID:
		return e.resource, true
	default:
		return e.stored.Get(uid)
	}
}
