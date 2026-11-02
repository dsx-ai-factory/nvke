// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle

import (
	"fmt"
	"slices"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/cedar-policy/cedar-go/x/exp/ast"
)

// sortedPolicies returns the policy IDs in order, so errors list in the same
// order on every load.
func (b *bundle) sortedPolicies() []cedar.PolicyID {
	var ids []cedar.PolicyID
	for id := range b.policies.All() {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// checkPolicies refuses an empty policy set, which denies every request, two
// policies with one @id, and a policy that reads any attribute but
// context.issuer.
//
// The request's principal and resource have no attributes, and neither do
// the actions and roles. A condition reading one never holds, so a
// forbid guarded by `resource has site` never applies, and the permit it was
// written to limit wins.
func (b *bundle) checkPolicies() []error {
	ids := b.sortedPolicies()
	if len(ids) == 0 {
		return []error{fmt.Errorf("%s declares no policies", policiesFile)}
	}
	var errs []error
	seen := map[string]bool{}
	for _, id := range ids {
		name := b.policyName(id)
		if seen[name] {
			errs = append(errs, fmt.Errorf("two policies have @id %q", name))
		}
		seen[name] = true
		for _, condition := range b.policies.Get(id).AST().Conditions {
			if read := attributeRead(condition.Body); read != "" {
				errs = append(errs, fmt.Errorf("policy %s reads %s; a policy may read only context.%s",
					name, read, contextIssuer))
			}
		}
	}
	return errs
}

// attributeRead describes the first attribute or tag read in body other than
// context.issuer, or returns "". It allows only context.issuer rather than
// listing what to refuse, because Cedar can reach an entity through a
// variable, a literal or any expression.
func attributeRead(body ast.IsNode) string {
	var read string
	ast.Inspect(ast.NewNode(body), func(node ast.IsNode) bool {
		// Returning false skips only this node's children, so stop every
		// later node from replacing a read already found.
		if read != "" {
			return false
		}
		switch n := node.(type) {
		case ast.NodeTypeAccess:
			read = describeRead(n.Arg, string(n.Value))
		case ast.NodeTypeHas:
			read = describeRead(n.Arg, string(n.Value))
		case ast.NodeTypeGetTag:
			read = describeRead(n.Left, "")
		case ast.NodeTypeHasTag:
			read = describeRead(n.Left, "")
		default:
		}
		return read == ""
	})
	return read
}

// describeRead describes a read of attribute, or of a tag when attribute is
// empty, on receiver. It returns "" for context.issuer.
func describeRead(receiver ast.IsNode, attribute string) string {
	what := "a tag"
	if attribute != "" {
		what = fmt.Sprintf("attribute %q", attribute)
	}
	switch r := receiver.(type) {
	case ast.NodeTypeVariable:
		if r.Name == "context" && attribute == contextIssuer {
			return ""
		}
		return what + " of " + string(r.Name)
	case ast.NodeValue:
		return what + " of " + r.Value.String()
	default:
		return what + " of an expression"
	}
}

// checkNames checks what each policy names:
//
//   - Every entity type is the principal, an action, a role or an API
//     resource type. A forbid naming a misspelled type never applies.
//   - The principal scope compares with NVKE::Principal using == or is, and
//     with NVKE::Role using in. The action scope names actions, and the
//     resource scope names API resource types.
//   - Every action named is one of the API's actions, and every role named
//     is declared in roles.json.
//
// It also refuses a role that no policy names, directly or through a role
// it's nested in. Such a role, or a policy naming an undeclared one, grants
// nothing.
func (b *bundle) checkNames() []error {
	resourceTypes := map[types.EntityType]bool{}
	for _, t := range b.resourceTypes {
		resourceTypes[types.EntityType(namespace+"::"+t)] = true
	}
	known := func(t types.EntityType) bool {
		return t == principalType || t == actionType || t == roleType || resourceTypes[t]
	}

	var errs []error
	named := map[types.EntityUID]bool{}
	for _, id := range b.sortedPolicies() {
		policy := (*ast.Policy)(b.policies.Get(id).AST())
		problems := scopeProblems(policy, resourceTypes)
		namedTypes, uids := namedInPolicy(policy)
		for _, t := range namedTypes {
			if !known(t) {
				problems = append(problems, fmt.Sprintf("names type %s, which isn't the principal, an action, "+
					"a role or an API resource type", t))
			}
		}
		for _, uid := range uids {
			if uid.Type != actionType && uid.Type != roleType {
				continue
			}
			named[uid] = true
			if _, declared := b.entities[uid]; !declared && uid.Type == actionType {
				problems = append(problems, fmt.Sprintf("names %s, which isn't one of the API's actions", uid))
			} else if !declared {
				problems = append(problems, fmt.Sprintf("names %s, which %s doesn't declare", uid, rolesFile))
			}
		}
		for _, p := range problems {
			errs = append(errs, fmt.Errorf("policy %s %s", b.policyName(id), p))
		}
	}
	for _, uid := range sortedUIDs(b.entities) {
		if uid.Type == roleType && !namedOrNested(uid, named, b.entities, map[types.EntityUID]bool{}) {
			errs = append(errs, fmt.Errorf("%s declares %s, but no policy names it or a role it's nested in",
				rolesFile, uid))
		}
	}
	return errs
}

// scopeProblems reports each scope that compares with the wrong entity type.
// Such a scope never matches, so a forbid with it never applies.
func scopeProblems(p *ast.Policy, resourceTypes map[types.EntityType]bool) []string {
	var problems []string
	check := func(scope string, t types.EntityType, ok bool) {
		if !ok {
			problems = append(problems, fmt.Sprintf("compares the %s with %s", scope, t))
		}
	}
	switch s := p.Principal.(type) {
	case ast.ScopeTypeEq:
		check("principal", s.Entity.Type, s.Entity.Type == principalType)
	case ast.ScopeTypeIn:
		check("principal", s.Entity.Type, s.Entity.Type == roleType)
	case ast.ScopeTypeIs:
		check("principal", s.Type, s.Type == principalType)
	case ast.ScopeTypeIsIn:
		check("principal", s.Type, s.Type == principalType)
		check("principal", s.Entity.Type, s.Entity.Type == roleType)
	default:
	}
	switch s := p.Action.(type) {
	case ast.ScopeTypeEq:
		check("action", s.Entity.Type, s.Entity.Type == actionType)
	case ast.ScopeTypeIn:
		check("action", s.Entity.Type, s.Entity.Type == actionType)
	case ast.ScopeTypeInSet:
		for _, e := range s.Entities {
			check("action", e.Type, e.Type == actionType)
		}
	default:
	}
	switch s := p.Resource.(type) {
	case ast.ScopeTypeEq:
		check("resource", s.Entity.Type, resourceTypes[s.Entity.Type])
	case ast.ScopeTypeIn:
		check("resource", s.Entity.Type, resourceTypes[s.Entity.Type])
	case ast.ScopeTypeIs:
		check("resource", s.Type, resourceTypes[s.Type])
	case ast.ScopeTypeIsIn:
		check("resource", s.Type, resourceTypes[s.Type])
		check("resource", s.Entity.Type, resourceTypes[s.Entity.Type])
	default:
	}
	return problems
}

// namedInPolicy returns the entity types a policy names with is, and the
// entities it names, in its scope and its conditions.
func namedInPolicy(policy *ast.Policy) ([]types.EntityType, []types.EntityUID) {
	var (
		entityTypes []types.EntityType
		uids        []types.EntityUID
	)
	for _, scope := range []ast.IsScopeNode{policy.Principal, policy.Action, policy.Resource} {
		switch s := scope.(type) {
		case ast.ScopeTypeEq:
			uids = append(uids, s.Entity)
		case ast.ScopeTypeIn:
			uids = append(uids, s.Entity)
		case ast.ScopeTypeInSet:
			uids = append(uids, s.Entities...)
		case ast.ScopeTypeIs:
			entityTypes = append(entityTypes, s.Type)
		case ast.ScopeTypeIsIn:
			entityTypes = append(entityTypes, s.Type)
			uids = append(uids, s.Entity)
		default:
		}
	}
	for _, condition := range policy.Conditions {
		ast.Inspect(ast.NewNode(condition.Body), func(node ast.IsNode) bool {
			switch n := node.(type) {
			case ast.NodeValue:
				if uid, ok := n.Value.(types.EntityUID); ok {
					uids = append(uids, uid)
				}
			case ast.NodeTypeIs:
				entityTypes = append(entityTypes, n.EntityType)
			case ast.NodeTypeIsIn:
				entityTypes = append(entityTypes, n.EntityType)
			default:
			}
			return true
		})
	}
	for _, uid := range uids {
		entityTypes = append(entityTypes, uid.Type)
	}
	return entityTypes, uids
}

// namedOrNested reports whether role, or a role it's nested in, is named.
func namedOrNested(role types.EntityUID, named map[types.EntityUID]bool, entities types.EntityMap, seen map[types.EntityUID]bool) bool {
	if named[role] {
		return true
	}
	if seen[role] {
		return false
	}
	seen[role] = true
	for parent := range entities[role].Parents.All() {
		if namedOrNested(parent, named, entities, seen) {
			return true
		}
	}
	return false
}
