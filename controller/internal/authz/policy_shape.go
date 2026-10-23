// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"fmt"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/x/exp/ast"
)

// entityVariables are the three Cedar variables that name an entity. context is
// deliberately absent: it is the one thing in a request whose attributes this
// engine populates.
var entityVariables = map[string]bool{ //nolint:gochecknoglobals // a fixed table
	VariablePrincipal: true,
	VariableAction:    true,
	VariableResource:  true,
}

// checkPolicyShape refuses a policy that reads an attribute of an entity.
//
// Nothing populates them. The generated schema declares no attribute on any
// entity type, the entity files carry empty records, and the resource is
// synthesized per request from the path — this middleware runs before the
// handler and has not read a database, so it does not know what site a cluster
// is in. A policy that reads one is therefore not a rule that is sometimes
// false. It is a rule that is never anything.
//
// That would be harmless if it failed closed, and it does not:
//
//	forbid(principal, action in NKE::Action::"write", resource)
//	when { resource has site && resource.site == "prod" };
//
// reads as a restriction, type-checks against the schema — measured, both with
// the attribute declared and without — loads, keeps the readiness gate green,
// and never applies. `resource has site` is false, so the forbid is skipped and
// the permit it existed to override wins. Written without the `has` guard it is
// worse in a subtler way: the attribute access errors, Cedar skips the policy
// and reports the error beside the decision, and it is only the errored-Allow
// refusal in Authorize that turns that into a denial rather than the Allow the
// permit would otherwise have won. So the strict validator rejects the shape
// that fails closed and accepts the shape that fails open, which steers an
// author towards the wrong one.
//
// Refusing at load rather than in a test, unlike the schema type-check: this is
// the only class of policy mistake here that fails open. A typo'd action name
// matches nothing and denies; a rule that silently grants what it was written to
// forbid is the one an operator most needs to be told about, including when they
// hand-edited the ConfigMap in a cluster rather than going through CI.
//
// If an attribute ever gets populated — a resource looked up before the decision,
// or a principal attribute in roles.json that the generated schema declares —
// this is what has to be revisited, deliberately, rather than a policy quietly
// starting to work.
func checkPolicyShape(set *cedar.PolicySet) error {
	for id, policy := range set.All() {
		name := string(id)
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			name = string(annotated)
		}
		for _, condition := range policy.AST().Conditions {
			if read, found := entityAttributeRead(condition.Body); found {
				return fmt.Errorf(
					"policy %s reads %s.%s, and nothing populates entity attributes: "+
						"the rule would never apply, and a forbid written this way silently grants "+
						"what it was written to refuse",
					name, read.variable, read.attribute,
				)
			}
		}
	}
	return nil
}

// entityAttributeRead returns the first entity attribute a condition reads.
//
// The walk is cedar-go's own Inspect, which knows every node type the parser can
// produce. Enumerating them here instead would mean a node kind added upstream
// is one this stops looking inside — and a check that quietly stops firing is
// the shape of the defect it exists to catch.
func entityAttributeRead(body ast.IsNode) (attributeRead, bool) {
	var read attributeRead
	var found bool

	ast.Inspect(ast.NewNode(body), func(node ast.IsNode) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		// `resource.site` and `resource has site`.
		case ast.NodeTypeAccess:
			read, found = readOf(n.Arg, string(n.Value))
		case ast.NodeTypeHas:
			read, found = readOf(n.Arg, string(n.Value))
		// `resource.getTag(x)` and `resource hasTag x`. Tags are a second
		// attribute namespace, populated by nothing here for the same reason.
		case ast.NodeTypeGetTag:
			read, found = readOf(n.Left, tagAttribute)
		case ast.NodeTypeHasTag:
			read, found = readOf(n.Left, tagAttribute)
		default:
			// Every other node kind reads no attribute itself; Inspect walks
			// into its children.
		}
		return !found
	})
	return read, found
}

// attributeRead is what a policy reached for: the variable, and the attribute of
// it.
type attributeRead struct {
	variable  string
	attribute string
}

const (
	// tagAttribute stands in for a tag name, which is an expression rather than
	// a literal and so has nothing to quote back at the author.
	tagAttribute = "<tag>"
)

// readOf reports the entity variable an access is rooted at, if it is one.
// A nested access such as principal.a.b is caught at its inner node, so only the
// direct case has to be recognized here.
func readOf(target ast.IsNode, attribute string) (attributeRead, bool) {
	name, ok := target.(ast.NodeTypeVariable)
	if !ok || !entityVariables[string(name.Name)] {
		return attributeRead{}, false
	}
	return attributeRead{variable: string(name.Name), attribute: attribute}, true
}
