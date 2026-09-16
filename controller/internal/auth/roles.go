// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
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
// already refuses Roles.SPIFFE without Admit.SPIFFE on the same issuer -- plus
// one compiled cel.Program per configured SPIFFERoleRule, same order as
// RoleMapping.SPIFFE (design §3.2, §3.4).
//
// programs is resolve's own compile output, not Config.Validate's discarded
// one -- Validate proves every Expression is well-formed enough that this
// compile cannot itself fail; it never threads a program forward for resolve
// to reuse (design §3.1's "compilation lifecycle" split).
type spiffeRoleRules struct {
	trustDomain spiffeid.TrustDomain
	programs    []cel.Program
}

const (
	// spiffeCELPathVar is the one variable a SPIFFERoleRule.Expression sees:
	// the verified SPIFFE ID's path, never the raw subject and never the
	// trust domain (design §3.4's "why trust-domain matching stays outside
	// CEL").
	spiffeCELPathVar = "path"

	// spiffeCELCostLimit bounds one evaluation of a SPIFFERoleRule.Expression
	// (design §3.4: "bound worst-case cost of something an operator wrote,
	// don't just trust it," the same instinct behind the compile-time checks
	// in §3.1). Both worked examples in §3.4 -- a handful of .split() calls
	// plus indexing and equality checks -- cost on the order of a few dozen
	// cel-go cost units. 10,000 leaves generous headroom for a deployment's
	// own, more elaborate path convention while still refusing an expression
	// that tries to do unbounded work (an oversized comprehension, for
	// instance) on every SPIFFE-issuer request. It is not tuned against a
	// specific pathological expression -- there isn't one this design names
	// -- it is deliberately far above the expected cost of a reasonable rule
	// and far below "no limit at all."
	spiffeCELCostLimit = 10_000
)

// spiffeCELEnv builds the CEL evaluation environment every SPIFFERoleRule.
// Expression compiles and runs against: the path variable above, plus
// ext.Strings() for .split(...), which both of design §3.4's worked examples
// call and which is not part of CEL's core/default environment. Built fresh
// by each caller -- Config.Validate (config.go) and RoleMapping.resolve,
// below -- each need their own *cel.Env for their own compile-and-program
// pass; a *cel.Env is not carried between them (design §3.1's "compilation
// lifecycle" split).
func spiffeCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(spiffeCELPathVar, cel.StringType),
		ext.Strings(),
	)
}

// compileSPIFFEExpression compiles expression against env, refuses anything
// that doesn't type-check to string (design §3.1's ast.OutputType() check),
// and builds the runnable, cost-bounded cel.Program -- the same
// compile-check-program sequence Config.Validate needs (discarding the
// program) and RoleMapping.resolve needs (keeping it).
func compileSPIFFEExpression(env *cel.Env, expression string) (cel.Program, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if ast.OutputType() != cel.StringType {
		return nil, fmt.Errorf("must evaluate to string, got %s", ast.OutputType())
	}
	program, err := env.Program(ast, cel.CostLimit(spiffeCELCostLimit))
	if err != nil {
		return nil, err
	}
	return program, nil
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
		env, err := spiffeCELEnv()
		if err != nil {
			return nil, fmt.Errorf("building the SPIFFE role CEL environment: %w", err)
		}
		programs := make([]cel.Program, 0, len(m.SPIFFE))
		for i, rule := range m.SPIFFE {
			program, err := compileSPIFFEExpression(env, rule.Expression)
			if err != nil {
				// Config.Validate already refused an uncompilable/mistyped
				// expression; reached only if a Config bypassed Validate
				// entirely (design §3.1's "compilation lifecycle" split).
				return nil, fmt.Errorf("spiffe[%d]: expression %q: %w", i, rule.Expression, err)
			}
			programs = append(programs, program)
		}
		return &roleRules{spiffe: &spiffeRoleRules{trustDomain: admit.spiffe.trustDomain, programs: programs}}, nil
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
	if r.spiffe != nil {
		roles = append(roles, spiffeRolesFor(r.spiffe, subject)...)
	}
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

// spiffeRolesFor evaluates each of rules' compiled CEL programs against the
// verified SPIFFE ID's path (design §3.4). Trust domain is checked
// structurally, before any program runs, against rules.trustDomain -- real,
// parsed spiffeid.TrustDomain carried from setup, the same
// parsed-not-string-matched discipline spiffeRules.admits already applies
// (admit.go) -- never folded into the CEL expression itself (design §3.4's
// "why trust-domain matching stays outside CEL"). This has to happen before
// and separately from CEL because id.Path(), the only thing a program ever
// sees, has already had the trust domain stripped out of it by the time it is
// computed below: a rule expression structurally cannot scope on the trust
// domain, correctly or incorrectly, because path never carries it. Checking
// here first is what actually rejects a suffix-matching domain like
// "spiffe://example.org.attacker.test/..." before any program runs; if this
// check were ever dropped in favor of trusting an expression to scope itself,
// there would be nothing in path for it to check against and every trust
// domain would be admitted. subject is parsed a second time here,
// independently of Admit's own parse of the same string: the bounded,
// structural duplication design §3.2 accounts for explicitly rather than
// threading a parsed spiffeid.ID through from admission.
func spiffeRolesFor(rules *spiffeRoleRules, subject string) []string {
	id, err := spiffeid.FromString(subject)
	if err != nil {
		return nil
	}
	if !id.MemberOf(rules.trustDomain) {
		return nil
	}
	path := id.Path()
	var roles []string
	for _, program := range rules.programs {
		if role, ok := evalSPIFFERoleExpression(program, path); ok {
			roles = append(roles, role)
		}
	}
	return roles
}

// evalSPIFFERoleExpression runs one compiled SPIFFERoleRule.Expression
// program against path. Every failure mode -- an evaluation error (a
// cost-limit trip, an out-of-range split index, any other runtime fault), a
// non-string result, or an empty string -- returns ("", false): this rule
// contributes nothing, never a panic and never a denied request (design
// §3.4, §5). Config.Validate's ast.OutputType() check already guarantees the
// static result type is string, so the type assertion here is defense in
// depth, not the primary enforcement of that contract.
func evalSPIFFERoleExpression(program cel.Program, path string) (string, bool) {
	out, _, err := program.Eval(map[string]any{spiffeCELPathVar: path})
	if err != nil {
		return "", false
	}
	role, ok := out.Value().(string)
	if !ok || role == "" {
		return "", false
	}
	return role, true
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
