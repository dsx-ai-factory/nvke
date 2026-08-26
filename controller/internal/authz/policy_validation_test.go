// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	expast "github.com/cedar-policy/cedar-go/x/exp/ast"
	"github.com/cedar-policy/cedar-go/x/exp/schema"
	"github.com/cedar-policy/cedar-go/x/exp/schema/validate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Type-checking the bundle against its own schema.
//
// A policy naming an action that does not exist, or reading an attribute that
// is not declared, parses cleanly and then never matches — a rule that looks
// enforced and is not. Cedar's validator catches that class, and cedar-go ships
// one under x/exp/schema/validate.
//
// It runs here rather than in NewLocal on purpose. Deliberately test-only: the
// validator is experimental API, and a build-time check gets the whole benefit
// without the release binary depending on it. The trade is that a bundle
// hand-edited in a cluster is not re-checked — see the design's open questions.
//
// What is validated is the bundle's shared artifacts — the schema, the
// policies, the entities — not the engine that will read them.

func validator(t *testing.T) *validate.Validator {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(bundleDir(t), schemaFile))
	require.NoError(t, err)

	var s schema.Schema
	require.NoError(t, s.UnmarshalJSON(raw), "%s is not a valid Cedar JSON schema", schemaFile)

	// Resolve expands type references and action groups. A schema that parses
	// but does not resolve — a cycle, a dangling memberOf — is one whose
	// type-checking would be meaningless, so the failure belongs here rather
	// than in whatever it lets through.
	resolved, err := s.Resolve()
	require.NoError(t, err, "%s does not resolve", schemaFile)

	// Strict is the default, and the right setting for a generated schema: there
	// is no legacy policy to accommodate.
	return validate.New(resolved)
}

func bundleEntities(t *testing.T) types.EntityMap {
	t.Helper()

	entities := types.EntityMap{}
	for _, name := range entityFiles {
		raw, err := os.ReadFile(filepath.Join(bundleDir(t), name))
		require.NoError(t, err)

		var loaded types.EntityMap
		require.NoError(t, json.Unmarshal(raw, &loaded), "%s is not a valid entity file", name)
		for uid, entity := range loaded {
			entities[uid] = entity
		}
	}
	return entities
}

// Every policy must type-check: known actions, known entity types, declared
// attributes of the right type, and a scope some action can actually satisfy.
func TestEveryPolicyTypeChecks(t *testing.T) {
	v := validator(t)

	source, err := os.ReadFile(filepath.Join(bundleDir(t), policiesFile))
	require.NoError(t, err)

	set, err := cedar.NewPolicySetFromBytes(policiesFile, source)
	require.NoError(t, err)

	checked := 0
	for policyID, policy := range set.All() {
		id := string(policyID)
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			id = string(annotated)
		}
		// cedar.Policy is a defined type over x/exp/ast.Policy, so the
		// conversion is free; the validator only accepts the latter.
		// assert, not require: a bundle with two broken policies should report
		// both, not stop at the first.
		//nolint:testifylint // deliberate: report every failing policy
		assert.NoErrorf(t, v.Policy(id, (*expast.Policy)(policy.AST())), "policy %s does not type-check", id)
		checked++
	}
	require.NotZero(t, checked, "no policies were checked; the test would pass vacuously")
}

// Every entity must conform: a declared type, a parent type the schema allows,
// and attributes of the declared shape.
//
// Checked one at a time rather than through Validator.Entities, which collapses
// every failure into one generic sentence. Which entity is wrong is the only
// part worth reporting.
func TestEveryEntityConformsToTheSchema(t *testing.T) {
	v := validator(t)

	entities := bundleEntities(t)
	require.NotEmpty(t, entities, "no entities were checked; the test would pass vacuously")

	for uid, entity := range entities {
		assert.NoErrorf(t, v.Entity(entity), "entity %s does not conform to %s", uid, schemaFile)
	}
}

// The query the middleware builds must be one the schema accepts, for every
// operation the router serves.
//
// This is the join between three generated things: the action table drives the
// query, the schema declares what an action applies to, and both come out of the
// same pass over api/openapi.yaml. A resource type derived one way and declared
// another shows up here and nowhere else — the engine would simply deny.
func TestEveryOperationProducesAValidRequest(t *testing.T) {
	v := validator(t)

	for pattern, op := range operations {
		resourceType := op.ResourceType
		resourceID := "11111111-1111-1111-1111-111111111111"
		if op.IDParam == "" {
			resourceType, resourceID = CollectionType, op.Collection
		}

		request := cedar.Request{
			Principal: types.NewEntityUID(PrincipalType, types.String(subjectReader)),
			Action:    types.NewEntityUID(ActionType, types.String(op.Action)),
			Resource:  types.NewEntityUID(types.EntityType(resourceType), types.String(resourceID)),
			Context: types.NewRecord(types.RecordMap{
				ContextKeyIssuer: types.String("kube-apiserver"),
				ContextKeyMethod: types.String("GET"),
			}),
		}
		assert.NoErrorf(t, v.Request(request),
			"%s builds a request the schema rejects", pattern)
	}
}

// The checks above only mean something if they fail on a bad bundle. Each of
// these is a mistake that parses cleanly and silently never matches.
func TestValidationCatchesTheSilentMistakes(t *testing.T) {
	v := validator(t)

	tests := []struct {
		name   string
		policy string
	}{
		{"an action that does not exist", `permit(principal in NKE::Role::"reader", action == NKE::Action::"getClusterr", resource);`},
		{"an attribute that is not declared", `permit(principal, action, resource) when { resource.sight == "az51" };`},
		{"an entity type that does not exist", `permit(principal in NKE::Gremlin::"x", action, resource);`},
		{"an action applied to the wrong resource", `permit(principal, action == NKE::Action::"getCluster", resource == NKE::Site::"s1");`},
		{"an attribute compared to the wrong type", `permit(principal, action, resource) when { principal.sites == "az51" };`},
		{"a context key that does not exist", `permit(principal, action, resource) when { context.isuer == "x" };`},
		{"a context value of the wrong type", `permit(principal, action, resource) when { context.method == 5 };`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := cedar.NewPolicySetFromBytes("bad.cedar", []byte(tt.policy))
			require.NoError(t, err, "this policy is meant to parse and fail validation, not fail to parse")

			var failed bool
			for id, p := range set.All() {
				if v.Policy(string(id), (*expast.Policy)(p.AST())) != nil {
					failed = true
				}
			}
			assert.True(t, failed, "validation accepted a policy that can never match")
		})
	}
}

func TestValidationCatchesBrokenEntities(t *testing.T) {
	v := validator(t)

	tests := []struct {
		name   string
		entity types.Entity
	}{
		{"an entity type that does not exist", types.Entity{
			UID: types.NewEntityUID("NKE::Gremlin", "x"),
		}},
		// The exact mistake that shipped a broken bundle during development: a
		// Role parent on a type the schema does not allow one on.
		{"a parent the schema does not allow", types.Entity{
			UID:     types.NewEntityUID("NKE::Cluster", "c1"),
			Parents: types.NewEntityUIDSet(types.NewEntityUID("NKE::Role", "reader")),
		}},
		{"an attribute of the wrong type", types.Entity{
			UID:        types.NewEntityUID(PrincipalType, "p1"),
			Attributes: types.NewRecord(types.RecordMap{"sites": types.String("az51")}),
		}},
		{"an attribute that is not declared", types.Entity{
			UID:        types.NewEntityUID(PrincipalType, "p1"),
			Attributes: types.NewRecord(types.RecordMap{"nope": types.String("x")}),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, v.Entity(tt.entity), "validation accepted a malformed entity")
		})
	}
}
