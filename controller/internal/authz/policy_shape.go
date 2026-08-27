// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"fmt"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/cedar-policy/cedar-go/x/exp/ast"
)

// checkPolicyShape refuses a policy that reads an attribute of anything but the
// request context.
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
			if read, found := attributeReadOutsideContext(condition.Body); found {
				return fmt.Errorf(
					"policy %s reads %s.%s, and context is the only thing whose attributes are "+
						"populated: the rule would never apply, and a forbid written this way "+
						"silently grants what it was written to refuse",
					name, read.receiver, read.attribute)
			}
		}
	}
	return nil
}

// attributeReadOutsideContext returns the first attribute or tag operation in a
// condition whose receiver is anything but the context record.
//
// Stated as what is allowed rather than as what is refused, and that is the
// whole design of it. The first version of this check listed the receivers it
// rejected — the principal, action and resource variables — which made the rule
// only as complete as that list. Cedar has more ways to name an entity than a
// variable: an adversarial review found `NKE::Cluster::"…" has site`, whose
// receiver is a literal, and measuring it turned up a second shape the same day,
// `(if 1 == 1 then resource else resource) has site`, whose receiver is neither
// a variable nor a literal. Both loaded, both were skipped at evaluation, and
// both let the permit they existed to override win.
//
// The receivers that can name an entity are an open set, and an open set is one
// this will always be a shape behind. The things whose attributes are populated
// are a closed set with one member. So the question asked here is "is this
// reading the context record", and everything else is refused, whatever
// expression produced it.
//
// The walk is cedar-go's own Inspect, which knows every node type the parser can
// produce. Enumerating them here instead would mean a node kind added upstream
// is one this stops looking inside — and a check that quietly stops firing is
// the shape of the defect it exists to catch.
func attributeReadOutsideContext(body ast.IsNode) (attributeRead, bool) {
	var read attributeRead
	var found bool

	ast.Inspect(ast.NewNode(body), func(node ast.IsNode) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		// `x.site` and `x has site`.
		case ast.NodeTypeAccess:
			read, found = readOf(n.Arg, string(n.Value))
		case ast.NodeTypeHas:
			read, found = readOf(n.Arg, string(n.Value))
		// `x.getTag(k)` and `x hasTag k`. Tags are a second attribute namespace,
		// populated by nothing here for the same reason.
		case ast.NodeTypeGetTag:
			read, found = readOf(n.Left, tagAttribute)
		case ast.NodeTypeHasTag:
			read, found = readOf(n.Left, tagAttribute)
		}
		return !found
	})
	return read, found
}

// attributeRead is what a policy reached for: the receiver, as far as it can be
// named, and the attribute of it.
type attributeRead struct {
	receiver  string
	attribute string
}

const (
	// tagAttribute stands in for a tag name, which is an expression rather than
	// a literal and so has nothing to quote back at the author.
	tagAttribute = "<tag>"
)

// readOf reports the read unless its receiver is the context record.
func readOf(target ast.IsNode, attribute string) (attributeRead, bool) {
	if rootedAtContext(target) {
		return attributeRead{}, false
	}
	return attributeRead{receiver: describeReceiver(target), attribute: attribute}, true
}

// rootedAtContext reports whether an expression is the context record or an
// attribute of it. `context.a.b` is still the context; anything that reaches a
// variable other than context, a literal, or any other kind of node, is not.
func rootedAtContext(node ast.IsNode) bool {
	for {
		switch n := node.(type) {
		case ast.NodeTypeVariable:
			return string(n.Name) == VariableContext
		case ast.NodeTypeAccess:
			node = n.Arg
		default:
			return false
		}
	}
}

// describeReceiver names the receiver in the error, so an author is told which
// expression to change rather than only which attribute.
func describeReceiver(node ast.IsNode) string {
	switch n := node.(type) {
	case ast.NodeTypeVariable:
		return string(n.Name)
	case ast.NodeValue:
		if uid, ok := n.Value.(types.EntityUID); ok {
			return uid.String()
		}
		return n.Value.String()
	default:
		return "an expression"
	}
}
