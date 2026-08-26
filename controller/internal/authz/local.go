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

	entities := types.EntityMap{}
	for _, name := range entityFiles {
		if err := loadEntities(filepath.Join(dir, name), entities); err != nil {
			return nil, err
		}
	}
	if len(entities) == 0 {
		return nil, errors.New("the bundle declares no entities: no principal would hold a role")
	}

	return &Local{policies: policies, entities: entities}, nil
}

// loadEntities merges one entity file into dst, refusing a duplicate: the later
// one would win, so a role redefined in the wrong file would quietly change who
// holds what.
func loadEntities(path string, dst types.EntityMap) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	var loaded types.EntityMap
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	for uid, entity := range loaded {
		if _, ok := dst[uid]; ok {
			return fmt.Errorf("%s redeclares %s", filepath.Base(path), uid)
		}
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
	if q.Principal == "" || q.Action == "" || q.ResourceType == "" || q.ResourceID == "" {
		return Decision{}, fmt.Errorf("%w: a required field is empty", ErrInvalidQuery)
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
		reasons = append(reasons, string(reason.PolicyID))
	}
	// Cedar reports evaluation errors alongside a decision. A Deny with errors
	// is still a Deny, but an Allow reached while some policy errored is not
	// something to serve on.
	if len(diagnostic.Errors) > 0 && decision == cedar.Allow {
		return Decision{}, fmt.Errorf("policy evaluation errored: %v", diagnostic.Errors)
	}

	return Decision{Allow: decision == cedar.Allow, Reasons: reasons}, nil
}

// Ready reports whether the bundle is loaded. NewLocal already refused an empty
// one, so this is a cheap invariant check rather than a network round trip.
func (l *Local) Ready(context.Context) error {
	if l == nil || l.policies == nil || !hasAny(l.policies) || len(l.entities) == 0 {
		return errors.New("the policy bundle is not loaded")
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
