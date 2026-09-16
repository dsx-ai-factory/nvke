// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// roleRules is a RoleMapping with its trust domain resolved (for the SPIFFE
// case), mirroring admitRules beside it (admit.go). Exactly one of
// claim/spiffe/serviceAccount is set, matching RoleMapping's own "at most one"
// validation (config.go, RoleMapping.validate) -- so in practice exactly one
// of rolesFor's branches ever has anything configured for a given issuer
// (design-authz-claim-based-roles.md, §3.1a, §3.5).
type roleRules struct {
	claim          *claimRoleRules
	spiffe         *spiffeRoleRules
	serviceAccount []serviceAccountRoleRules
}

type claimRoleRules struct {
	name        string
	valueToRole map[string]string
}

// spiffeRoleRules carries the issuer's trust domain, borrowed from
// admitRules.spiffe rather than parsed a second time -- RoleMapping.validate
// already refuses Roles.SPIFFE without Admit.SPIFFE on the same issuer.
//
// It carries no compiled CEL programs. The CEL evaluation engine
// (google/cel-go, design §3.4) is a deliberate follow-on slice, not built
// here (design §8) -- see RoleMapping.SPIFFE's doc comment (config.go).
// rolesFor, below, never derives a role from this: a configured
// Roles.SPIFFE is accepted at startup and contributes nothing at request
// time, which is the fail-safe direction for a mechanism that isn't built
// yet -- an operator gets an inert config, never a silent wrong grant.
type spiffeRoleRules struct {
	trustDomain spiffeid.TrustDomain
}

type serviceAccountRoleRules struct {
	namespace string
	// name is nil for "any name in the namespace", exactly like
	// ServiceAccountRoleRule.Name.
	name *string
	role string
}

// resolve builds the bound role rules for one issuer, the same
// "Validate proves it's well-formed, resolve builds what request time
// actually runs" split Admit.resolve already follows for TrustDomain
// (admit.go, design §3.1, §3.2). admit is the same issuer's already-resolved
// admitRules, consulted only to borrow its parsed trust domain for the
// SPIFFE branch. m is nil for an issuer with no Roles config: resolve
// returns (nil, nil), and rolesFor is nil-safe against it.
func (m *RoleMapping) resolve(admit *admitRules) (*roleRules, error) {
	if m == nil {
		return nil, nil //nolint:nilnil // a nil *roleRules and a nil error is exactly "this issuer has no Roles config"
	}
	switch {
	case m.Claim != nil:
		return &roleRules{claim: &claimRoleRules{
			name:        m.Claim.Name,
			valueToRole: m.Claim.ValueToRole,
		}}, nil
	case m.SPIFFE != nil:
		if admit == nil || admit.spiffe == nil {
			// RoleMapping.validate already refuses this shape at Config.Validate
			// time; reached only if a Config bypassed Validate entirely.
			return nil, fmt.Errorf("roles.spiffe requires admit.spiffe on the same issuer")
		}
		return &roleRules{spiffe: &spiffeRoleRules{trustDomain: admit.spiffe.trustDomain}}, nil
	default:
		rules := make([]serviceAccountRoleRules, 0, len(m.ServiceAccount))
		for _, r := range m.ServiceAccount {
			rules = append(rules, serviceAccountRoleRules{namespace: r.Namespace, name: r.Name, role: r.Role})
		}
		return &roleRules{serviceAccount: rules}, nil
	}
}

// rolesFor computes the Cedar role names claims/subject derive for this
// issuer. Nil-safe: a nil receiver (an issuer with no Roles config)
// contributes nothing, so every caller can call it unconditionally the way
// Authenticate does (verify.go).
//
// Reads only claims and subject -- data Verify already produced (design
// §3.2) -- and never mutates either. The claim and service-account branches
// are mutually exclusive per issuer (RoleMapping.validate), but rolesFor
// appends both into one accumulator regardless: at most one of them is ever
// non-nil for a given issuer, so "appends to whatever the others
// contributed" describes how the branches are wired, not a scenario where
// one token is scored against more than one mechanism (design §3.5).
func (r *roleRules) rolesFor(claims jwt.MapClaims, subject string) []string {
	if r == nil {
		return nil
	}
	var roles []string
	if r.claim != nil {
		roles = append(roles, claimRolesFor(r.claim, claims)...)
	}
	// r.spiffe's branch is deliberately absent here: see spiffeRoleRules' doc
	// comment above. A configured Roles.SPIFFE contributes zero roles in this
	// build.
	if len(r.serviceAccount) > 0 {
		roles = append(roles, serviceAccountRolesFor(r.serviceAccount, claims, subject)...)
	}
	return roles
}

// claimRolesFor implements ClaimRoleMapping's matching: the claim may be a
// JSON string or a JSON array of strings (Entra's "groups" shape), and every
// matching value contributes its role. Claim-absent, the wrong JSON type, and
// a JSON array containing a non-string element all contribute nothing --
// never a panic, and for the mixed-type array, nothing at all rather than a
// partial match on the elements that happened to parse (design §5's
// fail-closed reading of that row).
func claimRolesFor(rules *claimRoleRules, claims jwt.MapClaims) []string {
	value, ok := claims[rules.name]
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case string:
		role, ok := rules.valueToRole[v]
		if !ok {
			return nil
		}
		return []string{role}
	case []any:
		roles := make([]string, 0, len(v))
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				// The whole claim contributes zero roles, not just the bad
				// element -- design §5's mixed-type-array row.
				return nil
			}
			if role, ok := rules.valueToRole[s]; ok {
				roles = append(roles, role)
			}
		}
		return roles
	default:
		return nil
	}
}

// serviceAccountRolesFor derives roles from a PSAT's nested "kubernetes.io"
// claim, after checking subject agrees with what the claim names.
//
// This is the fix for the gap both of design-authz-claim-based-roles.md's
// §3.1a review rounds independently found: admitRules.admits (admit.go) can
// admit a principal via the SPIFFE branch without ever inspecting its
// claims, so a token admitted on one identity could still carry a
// "kubernetes.io" claim naming a different (namespace, name) -- and without
// this check, that claim would be trusted for a role grant it was never
// checked against. subject must equal
// serviceAccountSubjectPrefix+sa.namespace+":"+sa.name, the identical
// comparison admitsServiceAccount performs for admission (admit.go), before
// any rule is evaluated. A mismatch means zero roles from this source, the
// same "adds no additional privilege" outcome as every other
// absent/malformed-claim row in design §5's safety table -- not a distinct
// failure mode.
func serviceAccountRolesFor(rules []serviceAccountRoleRules, claims map[string]any, subject string) []string {
	sa, ok := serviceAccountFromClaims(claims)
	if !ok {
		return nil
	}
	if subject != serviceAccountSubjectPrefix+sa.namespace+":"+sa.name {
		return nil
	}
	var roles []string
	for _, rule := range rules {
		if !matchesServiceAccount(rule.namespace, rule.name, sa) {
			continue
		}
		roles = append(roles, rule.role)
	}
	return roles
}
