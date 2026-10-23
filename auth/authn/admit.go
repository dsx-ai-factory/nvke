// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"bytes"
	"encoding/json"
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

// admits matches any rule kind the set carries, which is why Admit.validate
// refuses a set carrying more than one.
func (r *admitRules) admits(p *Principal) bool {
	if r.admitsServiceAccount(p) {
		return true
	}
	if r.admitsGroups(p) {
		return true
	}
	return r.spiffe.admits(p.Subject)
}

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

// admitsServiceAccount matches Principal.ServiceAccount's verified identity;
// see there for why the nested claim is read and checked against the subject.
func (r *admitRules) admitsServiceAccount(p *Principal) bool {
	if len(r.serviceAccounts) == 0 {
		return false
	}
	sa, ok := p.ServiceAccount()
	if !ok {
		return false
	}
	for _, rule := range r.serviceAccounts {
		if sa.Matches(rule.Namespace, rule.Name) {
			return true
		}
	}
	return false
}

// ServiceAccount is a bound service account token's verified identity.
type ServiceAccount struct {
	Namespace string
	Name      string
}

// Matches reports whether sa is the one a (namespace, optional name) selector
// names. A nil name matches every name in the namespace, as
// ServiceAccountRule.AllowsAnyName does. Admission and the authorization
// bundle's role mappings both match through it, so the two cannot disagree
// about what "the same service account" means.
func (sa ServiceAccount) Matches(namespace string, name *string) bool {
	if namespace != sa.Namespace {
		return false
	}
	return name == nil || *name == sa.Name
}

// ValidateServiceAccountSelector checks a (namespace, optional name) selector
// against the Kubernetes naming rules. Admission rules and the authorization
// bundle's service account role mappings both call it, so a selector one
// accepts the other accepts too.
func ValidateServiceAccountSelector(namespace string, name *string) error {
	// Ordered so the most specific message wins: empty, then padded, then
	// whether Kubernetes would accept the name at all.
	if strings.TrimSpace(namespace) == "" {
		return errors.New("namespace is required and must not be empty")
	}
	if err := requireUnpadded("namespace", namespace); err != nil {
		return err
	}
	// NameIsDNSLabel and NameIsDNSSubdomain are what upstream's
	// ValidateNamespaceName and ValidateServiceAccountName resolve to, so a
	// name that could never exist fails at startup rather than becoming a rule
	// that matches nothing forever.
	if msgs := k8svalidation.NameIsDNSLabel(namespace, false); len(msgs) > 0 {
		return fmt.Errorf("namespace %q is not a valid namespace name: %s", namespace, strings.Join(msgs, "; "))
	}

	if name == nil {
		return nil
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("name is present but empty; remove the key to match any service account in namespace %q", namespace)
	}
	if err := requireUnpadded("name", *name); err != nil {
		return err
	}
	if msgs := k8svalidation.NameIsDNSSubdomain(*name, false); len(msgs) > 0 {
		return fmt.Errorf("name %q is not a valid service account name: %s", *name, strings.Join(msgs, "; "))
	}
	return nil
}

// RefuseNullServiceAccountName refuses a (namespace, optional name) selector,
// given as its raw JSON keys, whose name is present but null. A decoder maps
// null to the same nil *string as an absent key, and nil means every service
// account in the namespace, so `name:` left blank in YAML would widen a rule
// meant for one account to the whole namespace. Admission rules and the
// authorization bundle's service account role mappings both call it.
func RefuseNullServiceAccountName(selector map[string]json.RawMessage) error {
	raw, present := selector["name"]
	if present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("name is present but null; remove the key to match any service account in the namespace, " +
			"or set it to one service account's name")
	}
	return nil
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
