// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	k8svalidation "k8s.io/apimachinery/pkg/api/validation"
)

// Claim keys of the nested object in a bound service account token.
// "kubernetes.io" is a literal key, not a dotted path into a flat claim set.
const (
	kubernetesClaim     = "kubernetes.io"
	namespaceClaim      = "namespace"
	serviceAccountClaim = "serviceaccount"
	serviceAccountName  = "name"

	// serviceAccountSubjectPrefix begins the "sub" of every bound service
	// account token: system:serviceaccount:<namespace>:<name>.
	serviceAccountSubjectPrefix = "system:serviceaccount:"
)

// Admits reports whether p matches any rule in the set. Default-deny: an empty
// rule set, an unparseable subject and a nil receiver all admit nobody. It
// resolves on every call because the value may never have been validated; the
// request path uses NewVerifier's resolution instead.
func (a *Admit) Admits(p *Principal) bool {
	if a == nil || p == nil {
		return false
	}
	rules, err := a.resolve()
	if err != nil {
		return false
	}
	return rules.admits(p)
}

// admitRules is an Admit with its trust domain parsed, so admission compares
// values rather than parsing a constant per request.
type admitRules struct {
	serviceAccounts []ServiceAccountRule
	spiffe          *spiffeRules
}

type spiffeRules struct {
	trustDomain  spiffeid.TrustDomain
	pathPrefixes []string
}

// resolve builds the rule set admission matches against. Only the trust domain
// can fail, and SPIFFERule.validate has already refused a config where it does.
func (a *Admit) resolve() (*admitRules, error) {
	rules := &admitRules{serviceAccounts: a.ServiceAccounts}
	if a.SPIFFE == nil {
		return rules, nil
	}
	trustDomain, err := spiffeid.TrustDomainFromString(strings.TrimSpace(a.SPIFFE.TrustDomain))
	if err != nil {
		return nil, fmt.Errorf("trustDomain %q is not a valid SPIFFE trust domain: %w", a.SPIFFE.TrustDomain, err)
	}
	rules.spiffe = &spiffeRules{trustDomain: trustDomain, pathPrefixes: a.SPIFFE.PathPrefixes}
	return rules, nil
}

// admits matches either rule kind, which is why Admit.validate refuses a set
// carrying both.
func (r *admitRules) admits(p *Principal) bool {
	if r.admitsServiceAccount(p) {
		return true
	}
	return r.spiffe.admits(p.Subject)
}

// admitsServiceAccount matches the nested "kubernetes.io" claim rather than
// parsing "sub", whose shape a colon in a namespace name would break in the
// permissive direction. The two must then agree, or a token could be admitted on
// one identity while every consumer of the Principal reads the other.
func (r *admitRules) admitsServiceAccount(p *Principal) bool {
	if len(r.serviceAccounts) == 0 {
		return false
	}
	sa, ok := serviceAccountFromClaims(p.Claims)
	if !ok {
		return false
	}
	if p.Subject != serviceAccountSubjectPrefix+sa.namespace+":"+sa.name {
		return false
	}
	for _, rule := range r.serviceAccounts {
		if matchesServiceAccount(rule.Namespace, rule.Name, sa) {
			return true
		}
	}
	return false
}

// validateServiceAccountSelector checks a (namespace, optional name) selector
// the one way both an admission rule (ServiceAccountRule) and a role rule
// (ServiceAccountRoleRule, config.go) need it checked: the same Kubernetes
// naming rules, against the same shaped pair. Extracted so the two config
// types that mirror ServiceAccountRule's shape (§3.1a of
// design-authz-claim-based-roles.md) don't carry two hand-copies of the same
// DNS-label/DNS-subdomain checks that inevitably drift.
func validateServiceAccountSelector(namespace string, name *string) error {
	if strings.TrimSpace(namespace) == "" {
		return errors.New("namespace is required and must not be empty")
	}
	if err := requireUnpadded("namespace", namespace); err != nil {
		return err
	}
	// NameIsDNSLabel is what upstream's ValidateNamespaceName resolves to, so a
	// name that could never exist fails at startup rather than becoming a rule
	// that matches nothing forever.
	if msgs := k8svalidation.NameIsDNSLabel(namespace, false); len(msgs) > 0 {
		return fmt.Errorf("namespace %q is not a valid namespace name: %s", namespace, strings.Join(msgs, "; "))
	}

	if name == nil {
		return nil
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("name is present but empty; remove the key to admit any service account in namespace %q", namespace)
	}
	if err := requireUnpadded("name", *name); err != nil {
		return err
	}
	if msgs := k8svalidation.NameIsDNSSubdomain(*name, false); len(msgs) > 0 {
		return fmt.Errorf("name %q is not a valid service account name: %s", *name, strings.Join(msgs, "; "))
	}
	return nil
}

// matchesServiceAccount reports whether sa is the one (namespace, optional
// name) selector names -- an absent name means "any name in the namespace",
// exactly like ServiceAccountRule.AllowsAnyName and
// ServiceAccountRoleRule.AllowsAnyName (config.go). Shared by
// admitsServiceAccount, above, and serviceAccountRolesFor (roles.go), which is
// what keeps admission's and role derivation's pair-matching semantics from
// disagreeing about what "the same service account" means.
func matchesServiceAccount(namespace string, name *string, sa serviceAccountIdentity) bool {
	if namespace != sa.namespace {
		return false
	}
	return name == nil || *name == sa.name
}

type serviceAccountIdentity struct {
	namespace string
	name      string
}

// serviceAccountFromClaims reports false for anything but a bound service account
// token.
func serviceAccountFromClaims(claims map[string]any) (serviceAccountIdentity, bool) {
	k8s, ok := claims[kubernetesClaim].(map[string]any)
	if !ok {
		return serviceAccountIdentity{}, false
	}
	namespace, ok := k8s[namespaceClaim].(string)
	if !ok || namespace == "" {
		return serviceAccountIdentity{}, false
	}
	sa, ok := k8s[serviceAccountClaim].(map[string]any)
	if !ok {
		return serviceAccountIdentity{}, false
	}
	name, ok := sa[serviceAccountName].(string)
	if !ok || name == "" {
		return serviceAccountIdentity{}, false
	}
	return serviceAccountIdentity{namespace: namespace, name: name}, true
}

// admits reports whether subject is a SPIFFE ID in the rule's trust domain under
// one of its prefixes. Parsed rather than string-matched: a prefix test against
// "spiffe://example.org/nke-service/" would also match
// "spiffe://example.org.attacker.test/nke-service/".
func (s *spiffeRules) admits(subject string) bool {
	if s == nil {
		return false
	}
	id, err := spiffeid.FromString(subject)
	if err != nil {
		return false
	}
	if !id.MemberOf(s.trustDomain) {
		return false
	}
	for _, prefix := range s.pathPrefixes {
		if pathHasPrefix(id.Path(), prefix) {
			return true
		}
	}
	return false
}

// pathHasPrefix compares whole segments, so "/nke-service" does not admit
// "/nke-service-evil/cwe". A trailing slash is honored: "/nke-service/" admits
// only what is under the segment, "/nke-service" admits the segment too, which
// SPIRE can mint as a workload of its own.
func pathHasPrefix(path, prefix string) bool {
	const separator = "/"
	trimmed := strings.TrimRight(prefix, separator)
	if trimmed == "" {
		// Exactly "/" admits the whole trust domain. Every other all-slash
		// spelling, and the empty string, is a malformed rule and admits nobody
		// -- the same default-deny this file applies to a nil receiver, a nil
		// principal and an unparseable subject. SPIFFERule.validate refuses them
		// at startup too, but Admits is exported and documents that it may run
		// against a value that was never validated, so it cannot rely on that.
		return prefix == separator
	}
	if strings.HasSuffix(prefix, separator) {
		return strings.HasPrefix(path, trimmed+separator)
	}
	return path == trimmed || strings.HasPrefix(path, trimmed+separator)
}
