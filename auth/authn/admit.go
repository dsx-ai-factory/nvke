// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
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
	groups          []string
}

type spiffeRules struct {
	trustDomain  spiffeid.TrustDomain
	pathPrefixes []string
}

// resolve builds the rule set admission matches against. Only the trust domain
// can fail, and SPIFFERule.validate has already refused a config where it does.
func (a *Admit) resolve() (*admitRules, error) {
	rules := &admitRules{serviceAccounts: a.ServiceAccounts, groups: a.Groups}
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
	if r.admitsGroups(p) {
		return true
	}
	return r.spiffe.admits(p.Subject)
}

// admitsGroups reports whether the token's groups claim holds one of the
// rule's groups.
func (r *admitRules) admitsGroups(p *Principal) bool {
	items, _ := p.Claims["groups"].([]any)
	for _, item := range items {
		g, ok := item.(string)
		if !ok || g == "" {
			continue
		}
		for _, want := range r.groups {
			if g == want {
				return true
			}
		}
	}
	return false
}

// admitsServiceAccount reports whether the token is a service account's that
// one of the rules matches.
func (r *admitRules) admitsServiceAccount(p *Principal) bool {
	sa, ok := p.ServiceAccount()
	if !ok {
		return false
	}
	for _, rule := range r.serviceAccounts {
		if rule.Matches(sa) {
			return true
		}
	}
	return false
}

// ServiceAccount is a bound service account token's namespace and name.
type ServiceAccount struct {
	Namespace string
	Name      string
}

// serviceAccountFromClaims reports false for anything but a bound service account
// token.
func serviceAccountFromClaims(claims map[string]any) (ServiceAccount, bool) {
	k8s, ok := claims[kubernetesClaim].(map[string]any)
	if !ok {
		return ServiceAccount{}, false
	}
	namespace, ok := k8s[namespaceClaim].(string)
	if !ok || namespace == "" {
		return ServiceAccount{}, false
	}
	sa, ok := k8s[serviceAccountClaim].(map[string]any)
	if !ok {
		return ServiceAccount{}, false
	}
	name, ok := sa[serviceAccountName].(string)
	if !ok || name == "" {
		return ServiceAccount{}, false
	}
	return ServiceAccount{Namespace: namespace, Name: name}, true
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
