// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/authtoken"
)

// The claim-based-role tier of plan-authz-e2e-tests.md §3/§5 (project docs,
// cwe-api-oidc-auth): roles derived at request time from a verified token's
// claims (internal/auth/roles.go), on top of roles.json's static list. Built
// as of slice/6-claim-based-roles, per the plan.
//
// Why e2e and not just internal/auth's own unit tests: RoleMapping.resolve and
// rolesFor are unit-tested against a hand-built jwt.MapClaims and a hand-built
// subject string. What those tests cannot see is whether Authenticate
// (verify.go) actually threads the SAME verified subject into rolesFor that
// Admits used to admit the request in the first place -- two different
// functions, over the same *Principal, that have to agree for the mechanism to
// be safe. That disagreement is exactly what the fix this session closed:
// admitsServiceAccount and serviceAccountRolesFor now both require
// subject == serviceAccountSubjectPrefix+sa.namespace+":"+sa.name before
// trusting the "kubernetes.io" claim, but the only way to prove that check
// applies to what a *real, live-admitted* request actually carries -- and not
// just to a struct this suite assembles by hand -- is to send one through the
// real middleware chain.
//
// The PSAT subject-consistency rejection case (plan §3, "highest-value case in
// this whole catalog") is deliberately not implemented here. A bound
// Kubernetes service account token's "sub" and its nested "kubernetes.io"
// claim are minted together by the same TokenRequest call and are therefore
// always consistent by construction -- there is no lever in the real
// TokenRequest API (internal/authtoken's only token source) to make the
// apiserver issue a token where they disagree. The parenthetical alternative
// the plan names -- "a SPIFFE-admitted token that happens to also carry a
// kubernetes.io claim" -- needs a JWT-SVID with a custom claim, which SPIRE's
// Workload API does not hand this environment's exec-auth plugin control over,
// and needs SPIRE pod attestation wired into CAPD in the first place, which
// capd-e2e.yml documents is not done (see authz_test.go's authz-spiffe
// doc comment). Both routes to constructing the real scenario are closed with
// only real, unmodified e2e infra; forcing one open would mean either forging
// a signature (fake) or hand-building the claims/subject pair the way
// conformance_test.go already does in-process (exactly the thing this suite
// exists to go beyond). The closest honest e2e proxy -- a live PSAT whose
// granted roles are asserted to come only from its own (namespace, name), see
// "grants a role by (namespace, service account) match" below -- is a
// regression signal for the matching logic, not a reproduction of the
// disagreement the fix defends against. That specific regression remains a
// unit-level property (internal/auth/roles_test.go), which is the right level
// for it per the plan's own reasoning about checkPolicyShape (§3, "Validation
// / fail-safe").

// The claim-based-role test subject: a dev-fixture ServiceAccount minted only
// for "grants a role by (namespace, service account) match" below, distinct
// from authn's own default admitted identity (authtoken.DefaultNamespace/
// DefaultServiceAccount). See deploy/k8s/nke-auth-dev.yaml.
const (
	authzClaimSubjectNamespace      = "nke-clients"
	authzClaimSubjectServiceAccount = "nke-claim-subject"
)

var _ = Describe("NKE API authorization by claim", Label("authz", "authz-claims"), func() {
	var (
		restConfig *rest.Config
		clientset  kubernetes.Interface
		audience   string
		info       authzDeploymentInfo
	)

	BeforeEach(func(ctx SpecContext) {
		restConfig, clientset, audience, info = authzSetup(ctx)
	})

	// Kubernetes PSAT grants a role by (namespace, service account) match
	// (internal/auth/roles.go's serviceAccountRolesFor), not by roles.json
	// naming the principal individually the way "the reader role" (authz_test.go)
	// does. This suite can only mint for identities it already has
	// TokenRequest RBAC for (deploy/k8s/nke-auth-dev.yaml), so the case only
	// runs when the deployed auth config's serviceAccounts role rule actually
	// matches one of them -- today that is nke-clients/nke-claim-subject, a
	// dev-fixture identity minted for exactly this spec. It is deliberately
	// not authn's own default admitted identity (authtoken.DefaultNamespace/
	// DefaultServiceAccount): that identity is nke.NewClient's
	// authtoken.Discover fallback, which essentially every non-authz e2e spec
	// authenticates as, so it holds real (operator+nodeOperator) access via
	// roles.json instead -- a claim-based rule granting it only "reader"
	// would take write access away from the rest of the suite. See
	// deploy/k8s/nke-auth-dev.yaml's roles.serviceAccounts comment.
	It("grants a role by (namespace, service account) match", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, authnTimeout)
		defer cancel()

		rules, err := authzServiceAccountRoleRulesFromCluster(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the deployment's auth configuration")
		if len(rules) == 0 {
			Skip("no issuer in this deployment's auth configuration declares a roles.serviceAccounts rule, so there is no claim-based service-account role grant to exercise")
		}

		namespace, name := authzClaimSubjectNamespace, authzClaimSubjectServiceAccount
		rule, found := matchingServiceAccountRoleRule(rules, namespace, name)
		if !found {
			Skip(fmt.Sprintf(
				"none of this deployment's roles.serviceAccounts rules match %s/%s, the only identity this suite has TokenRequest RBAC for; "+
					"exercising a different rule needs a matching ServiceAccount and matching RBAC that this suite does not provision",
				namespace, name,
			))
		}

		// A role granted through roles.json's static list, for the same
		// principal, would make the read below succeed regardless of the claim
		// mechanism under test here -- so this only counts as proof of the
		// claim path when the static bundle grants this principal nothing.
		if staticRoles := authzRolesOf(info.entities, authzServiceAccountSubjectString(namespace, name)); len(staticRoles) > 0 {
			Skip(fmt.Sprintf(
				"roles.json separately grants %s/%s role(s) %v, so a successful read here would not distinguish the claim-based grant from the static one",
				namespace, name, staticRoles,
			))
		}

		source, err := authtoken.New(clientset, authtoken.Options{Namespace: namespace, ServiceAccount: name, Audience: audience})
		Expect(err).NotTo(HaveOccurred(), "build a token source for %s/%s", namespace, name)
		client := authnClient(restConfig, source)

		status, body, _ := authnListClusters(ctx, client)
		Expect(status).To(Equal(http.StatusOK),
			"roles.serviceAccounts rule %+v grants role %q, matching %s/%s by (namespace, service account); "+
				"every role this bundle declares includes read, so this must succeed. response body: %s",
			rule, rule.Role, namespace, name, body,
		)
	})

	// An OIDC "groups"-shaped claim grants a role with no roles.json entry
	// (internal/auth/roles.go's claimRolesFor). No environment this repository
	// stands up for e2e configures an issuer this suite can mint a controlled
	// claim for: internal/authtoken mints only bound Kubernetes service account
	// tokens via the real TokenRequest API, whose claim shape ("kubernetes.io")
	// is fixed by the apiserver, not settable per test. Exercising this case for
	// real needs an e2e-reachable IdP (or IdP-shaped issuer) this suite can mint
	// a token from with a chosen claim value -- deploy/k8s/nke-auth-dev.yaml
	// configures none today. This spec is real detection code, not a stub: it
	// reads the deployed auth configuration for a claim-based role rule and
	// reports precisely why it cannot go further, the same "absence is
	// reported, not assumed" discipline authnAbsentf applies elsewhere in this
	// suite.
	It("grants a role by OIDC claim match", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, authnTimeout)
		defer cancel()

		rules, err := authzClaimRoleRulesFromCluster(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the deployment's auth configuration")
		if len(rules) == 0 {
			Skip("no issuer in this deployment's auth configuration declares a roles.claim rule, so there is no claim-based role grant to exercise")
		}

		Skip(fmt.Sprintf(
			"issuer %q declares a roles.claim rule on claim %q, but this suite has no way to mint a token from that issuer with a controlled claim value "+
				"(internal/authtoken mints only bound Kubernetes service account tokens, whose claim shape the apiserver fixes); "+
				"exercising this case for real needs an e2e-reachable IdP this suite can mint a claim-bearing token from",
			rules[0].Issuer, rules[0].ClaimName,
		))
	})
})

// authzServiceAccountRoleRule is one issuers[].roles.serviceAccounts entry,
// with the issuer name that declared it kept alongside for Skip messages.
type authzServiceAccountRoleRule struct {
	Issuer    string
	Namespace string
	Name      *string
	Role      string
}

// authzClaimRoleRule is one issuers[].roles.claim entry.
type authzClaimRoleRule struct {
	Issuer      string
	ClaimName   string
	ValueToRole map[string]string
}

// authzAuthConfig is the slice of auth.yaml this file reads: just enough of
// each issuer's roles block to build authzServiceAccountRoleRule/
// authzClaimRoleRule, mirroring how authn_spire_test.go's parseSpireAdmission
// reads only the fields one spec needs rather than internal/auth's whole
// schema.
type authzAuthConfig struct {
	Issuers []struct {
		Name  string `json:"name"`
		Roles *struct {
			Claim *struct {
				Name        string            `json:"name"`
				ValueToRole map[string]string `json:"valueToRole"`
			} `json:"claim"`
			ServiceAccounts []struct {
				Namespace string  `json:"namespace"`
				Name      *string `json:"name"`
				Role      string  `json:"role"`
			} `json:"serviceAccounts"`
		} `json:"roles"`
	} `json:"issuers"`
}

func authzServiceAccountRoleRulesFromCluster(ctx context.Context, clientset kubernetes.Interface) ([]authzServiceAccountRoleRule, error) {
	parsed, found, err := parseAuthzAuthConfig(ctx, clientset)
	if err != nil || !found {
		return nil, err
	}

	var rules []authzServiceAccountRoleRule
	for _, issuer := range parsed.Issuers {
		if issuer.Roles == nil {
			continue
		}
		for _, rule := range issuer.Roles.ServiceAccounts {
			rules = append(rules, authzServiceAccountRoleRule{
				Issuer: issuer.Name, Namespace: rule.Namespace, Name: rule.Name, Role: rule.Role,
			})
		}
	}
	return rules, nil
}

func authzClaimRoleRulesFromCluster(ctx context.Context, clientset kubernetes.Interface) ([]authzClaimRoleRule, error) {
	parsed, found, err := parseAuthzAuthConfig(ctx, clientset)
	if err != nil || !found {
		return nil, err
	}

	var rules []authzClaimRoleRule
	for _, issuer := range parsed.Issuers {
		if issuer.Roles == nil || issuer.Roles.Claim == nil {
			continue
		}
		rules = append(rules, authzClaimRoleRule{
			Issuer: issuer.Name, ClaimName: issuer.Roles.Claim.Name, ValueToRole: issuer.Roles.Claim.ValueToRole,
		})
	}
	return rules, nil
}

func parseAuthzAuthConfig(ctx context.Context, clientset kubernetes.Interface) (authzAuthConfig, bool, error) {
	raw, found, err := authtoken.RawConfig(ctx, clientset)
	if err != nil || !found {
		return authzAuthConfig{}, found, err
	}
	var parsed authzAuthConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return authzAuthConfig{}, false, fmt.Errorf("parsing %s: %w", authtoken.ConfigMapKey, err)
	}
	return parsed, true, nil
}

// matchingServiceAccountRoleRule finds the first rule this (namespace, name)
// satisfies: an exact name match, or a namespace-wide rule (Name == nil).
func matchingServiceAccountRoleRule(rules []authzServiceAccountRoleRule, namespace, name string) (authzServiceAccountRoleRule, bool) {
	for _, rule := range rules {
		if rule.Namespace != namespace {
			continue
		}
		if rule.Name == nil || *rule.Name == name {
			return rule, true
		}
	}
	return authzServiceAccountRoleRule{}, false
}
