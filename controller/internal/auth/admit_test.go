// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
)

// serviceAccountPrincipal is a verified principal: admission only ever sees
// claims that have already passed signature and claim validation.
func serviceAccountPrincipal(namespace, name string) *auth.Principal {
	return &auth.Principal{
		IssuerName: k8sName,
		Issuer:     k8sIssuer,
		Subject:    "system:serviceaccount:" + namespace + ":" + name,
		Claims:     ServiceAccountClaims(namespace, name),
	}
}

func spiffePrincipal(id string) *auth.Principal {
	return &auth.Principal{
		IssuerName: spireName,
		Issuer:     spireIssuer,
		Subject:    id,
		Claims:     map[string]any{"sub": id},
	}
}

func TestAdmitServiceAccounts(t *testing.T) {
	t.Parallel()

	// Name is a pointer so "any name in the namespace" stays distinct from "this
	// one name".
	runner := "runner"
	runnerRule := auth.ServiceAccountRule{Namespace: "nke-clients", Name: &runner}

	tests := []struct {
		name      string
		admit     *auth.Admit
		principal *auth.Principal
		want      bool
	}{
		{
			name:      "a namespace rule with no name admits any service account in it",
			admit:     &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}}},
			principal: serviceAccountPrincipal("nke-clients", "anything-at-all"),
			want:      true,
		},
		{
			name:      "a named rule admits the matching service account",
			admit:     &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{runnerRule}},
			principal: serviceAccountPrincipal("nke-clients", "runner"),
			want:      true,
		},
		{
			name:      "a named rule does not admit another name in the admitted namespace",
			admit:     &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{runnerRule}},
			principal: serviceAccountPrincipal("nke-clients", "intruder"),
			want:      false,
		},
		{
			name:      "a named rule does not admit the same name in another namespace",
			admit:     &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{runnerRule}},
			principal: serviceAccountPrincipal("default", "runner"),
			want:      false,
		},
		{
			name: "any rule in the list may match",
			admit: &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{
				runnerRule,
				{Namespace: "nke-system"},
			}},
			principal: serviceAccountPrincipal("nke-system", "operator"),
			want:      true,
		},
		{
			// Nil claims and a string-valued kubernetes.io fail the same first type
			// assertion.
			name:  "a kubernetes.io claim of the wrong shape admits nobody",
			admit: &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}}},
			principal: &auth.Principal{
				Subject: "system:serviceaccount:nke-clients:runner",
				Claims:  map[string]any{"kubernetes.io": "nke-clients"},
			},
			want: false,
		},
		{
			// A later return than the case above: the namespace is there, the
			// serviceaccount object is not.
			name:  "a kubernetes.io claim with no serviceaccount object admits nobody",
			admit: &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}}},
			principal: &auth.Principal{
				Claims: map[string]any{"kubernetes.io": map[string]any{"namespace": "nke-clients"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.admit.Admits(tt.principal))
		})
	}
}

// Admission matches the nested "kubernetes.io" claim but everything downstream
// reads Principal.Subject, so the two must agree or a token can be admitted as
// one identity and logged and authorized as another. The apiserver writes them
// consistently, so requiring it refuses nothing legitimate.
func TestAdmitRequiresTheSubjectToMatchTheNestedClaim(t *testing.T) {
	t.Parallel()

	admit := &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}}}

	tests := []struct {
		name    string
		subject string
		want    bool
	}{
		{
			name:    "the subject the claim implies",
			subject: "system:serviceaccount:nke-clients:runner",
			want:    true,
		},
		{
			// The confused-deputy case: any disagreement is the same string
			// comparison, so the closest miss is the one worth stating.
			name:    "a subject naming another account in the admitted namespace",
			subject: "system:serviceaccount:nke-clients:someone-else",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			principal := &auth.Principal{
				IssuerName: k8sName,
				Issuer:     k8sIssuer,
				Subject:    tt.subject,
				Claims:     ServiceAccountClaims("nke-clients", "runner"),
			}
			assert.Equal(t, tt.want, admit.Admits(principal))
		})
	}
}

func TestAdmitSPIFFE(t *testing.T) {
	t.Parallel()

	rule := func(trustDomain string, prefixes ...string) *auth.Admit {
		return &auth.Admit{SPIFFE: &auth.SPIFFERule{TrustDomain: trustDomain, PathPrefixes: prefixes}}
	}

	tests := []struct {
		name      string
		admit     *auth.Admit
		principal *auth.Principal
		want      bool
	}{
		{
			name:      "an ID under the prefix in the trust domain is admitted",
			admit:     rule("example.org", "/nke-service/"),
			principal: spiffePrincipal("spiffe://example.org/nke-service/cloud-workflow-engine"),
			want:      true,
		},
		{
			name:      "the prefix matches segment-wise, so -evil is not under it",
			admit:     rule("example.org", "/nke-service/"),
			principal: spiffePrincipal("spiffe://example.org/nke-service-evil/cloud-workflow-engine"),
			want:      false,
		},
		{
			name:      "a prefix written without a trailing slash matches the same way",
			admit:     rule("example.org", "/nke-service"),
			principal: spiffePrincipal("spiffe://example.org/nke-service/cloud-workflow-engine"),
			want:      true,
		},
		{
			name:      "a prefix written without a trailing slash still does not match -evil",
			admit:     rule("example.org", "/nke-service"),
			principal: spiffePrincipal("spiffe://example.org/nke-service-evil/cloud-workflow-engine"),
			want:      false,
		},
		{
			// Written without the trailing slash, a prefix names the segment
			// itself as well as everything under it.
			name:      "a path equal to a prefix written without a trailing slash is admitted",
			admit:     rule("example.org", "/nke-service"),
			principal: spiffePrincipal("spiffe://example.org/nke-service"),
			want:      true,
		},
		{
			// With the slash a prefix means "things under this", and SPIRE can
			// register /nke-service itself as a workload nobody named.
			name:      "a path equal to a prefix written with a trailing slash is not admitted",
			admit:     rule("example.org", "/nke-service/"),
			principal: spiffePrincipal("spiffe://example.org/nke-service"),
			want:      false,
		},
		{
			name:      "a path outside every prefix is not admitted",
			admit:     rule("example.org", "/nke-service/", "/nke-infra/"),
			principal: spiffePrincipal("spiffe://example.org/tenant-workload/whatever"),
			want:      false,
		},
		{
			name:      "any prefix in the list may match",
			admit:     rule("example.org", "/nke-service/", "/nke-infra/"),
			principal: spiffePrincipal("spiffe://example.org/nke-infra/agent"),
			want:      true,
		},
		{
			name:      "a trust domain that merely extends the configured one is not admitted",
			admit:     rule("example.org", "/nke-service/"),
			principal: spiffePrincipal("spiffe://example.org.attacker.test/nke-service/cwe"),
			want:      false,
		},
		{
			// Anything spiffeid.FromString refuses lands here: a service account
			// subject, an empty one, or this near-miss.
			name:      "an https URL that looks like the ID is not admitted",
			admit:     rule("example.org", "/nke-service/"),
			principal: spiffePrincipal("https://example.org/nke-service/cloud-workflow-engine"),
			want:      false,
		},
		{
			name:      "a prefix of / admits the whole trust domain",
			admit:     rule("example.org", "/"),
			principal: spiffePrincipal("spiffe://example.org/anything/at/all"),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.admit.Admits(tt.principal))
		})
	}
}

func oidcPrincipal(groups []any) *auth.Principal {
	return &auth.Principal{
		IssuerName: "keycloak",
		Issuer:     "http://keycloak.nke.test:8180/realms/nke",
		Subject:    "user-1",
		Claims:     map[string]any{"sub": "user-1", "groups": groups},
	}
}

func TestAdmitGroups(t *testing.T) {
	t.Parallel()

	rule := &auth.Admit{Groups: []string{"nke-operators"}}

	tests := []struct {
		name      string
		admit     *auth.Admit
		principal *auth.Principal
		want      bool
	}{
		{
			name:      "a matching group in a string list is admitted",
			admit:     rule,
			principal: oidcPrincipal([]any{"nke-operators"}),
			want:      true,
		},
		{
			name:      "any listed group is enough",
			admit:     &auth.Admit{Groups: []string{"nke-readers", "nke-operators"}},
			principal: oidcPrincipal([]any{"nke-operators"}),
			want:      true,
		},
		{
			name:      "a different group is not admitted",
			admit:     rule,
			principal: oidcPrincipal([]any{"other"}),
			want:      false,
		},
		{
			name:      "a missing groups claim is not admitted",
			admit:     rule,
			principal: &auth.Principal{Subject: "user-1", Claims: map[string]any{"sub": "user-1"}},
			want:      false,
		},
		{
			name:      "a service-account principal is not admitted by a groups rule",
			admit:     rule,
			principal: serviceAccountPrincipal("nke-clients", "nke-client"),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.admit.Admits(tt.principal))
		})
	}
}

// The rule sets that must admit nobody, plus what a set carrying both rule kinds
// does. The "both" case shows that a SPIFFE rule beside a service account rule
// restricts nothing, which is why Admit.validate refuses the combination at
// startup rather than why it is supported. See TestLoadValidationAdmit.
func TestAdmitDefaultDeny(t *testing.T) {
	t.Parallel()

	both := &auth.Admit{
		ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}},
		SPIFFE:          &auth.SPIFFERule{TrustDomain: "example.org", PathPrefixes: []string{"/nke-service/"}},
	}

	tests := []struct {
		name      string
		admit     *auth.Admit
		principal *auth.Principal
		want      bool
	}{
		{
			name:      "a nil rule set admits nobody",
			admit:     nil,
			principal: serviceAccountPrincipal("nke-clients", "runner"),
			want:      false,
		},
		{
			name:      "an empty rule set admits nobody",
			admit:     &auth.Admit{},
			principal: serviceAccountPrincipal("nke-clients", "runner"),
			want:      false,
		},
		{
			name:      "a nil principal is admitted by nothing",
			admit:     both,
			principal: nil,
			want:      false,
		},
		{
			// The SPIFFE rule refuses this subject (TestAdmitSPIFFE) and the set
			// admits it anyway, so the SPIFFE rule beside it restricts nothing.
			name:      "both rule kinds present: a subject the SPIFFE rule refuses is admitted by its claims",
			admit:     both,
			principal: serviceAccountPrincipal("nke-clients", "runner"),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.admit.Admits(tt.principal))
		})
	}
}

// Admission can only report a parse failure by admitting nobody, so a trustDomain
// that passes "non-empty, no slash" and fails to parse is an unannounced
// deny-all.
func TestUnparseableTrustDomainFailsAtStartupNotPerRequest(t *testing.T) {
	t.Parallel()

	admitted := "spiffe://example.org/nke-service/cloud-workflow-engine"

	// The other unparseable spellings are TestLoadValidationAdmit's business; what
	// matters here is what admission does with one that got through.
	const td = "EXAMPLE.ORG"

	_, err := auth.Load(writeConfig(t, spiffeConfig(
		`{trustDomain: "`+td+`", pathPrefixes: ["/nke-service/"]}`,
	)))
	require.Error(t, err, "an unparseable trust domain must be a startup failure")
	assert.Contains(t, err.Error(), td, "the message has to name the value to fix")

	// What it would do instead: no error, nobody admitted. Built directly,
	// because Load refuses.
	rule := &auth.Admit{SPIFFE: &auth.SPIFFERule{
		TrustDomain:  td,
		PathPrefixes: []string{"/nke-service/"},
	}}
	assert.False(t, rule.Admits(spiffePrincipal(admitted)),
		"the silent deny-all startup validation now prevents")
}

// Both layers refuse an all-slash prefix, so neither has to cover for the other.
// Startup validation rejects the config, and admission admits nobody even when
// handed a rule that never went through it -- Admits is exported and documents
// that its receiver may be unvalidated. That "/" is accepted by both is covered
// where each layer is tested.
func TestSlashPrefixesAcrossValidationAndAdmission(t *testing.T) {
	t.Parallel()

	admitted := "spiffe://example.org/tenant-workload/anything"

	for _, prefix := range []string{"", "//", "///"} {
		t.Run("prefix "+strconv.Quote(prefix), func(t *testing.T) {
			t.Parallel()

			_, err := auth.Load(writeConfig(t, spiffeConfig(
				`{trustDomain: example.org, pathPrefixes: ["`+prefix+`"]}`,
			)))
			require.Error(t, err, "an all-slash prefix must not survive startup")

			// Built directly, because Load refuses to produce one. Only the
			// deliberate "/" means the whole trust domain; everything else that
			// trims to empty is a malformed rule and admits nobody.
			rule := &auth.Admit{SPIFFE: &auth.SPIFFERule{
				TrustDomain:  "example.org",
				PathPrefixes: []string{prefix},
			}}
			assert.False(t, rule.Admits(spiffePrincipal(admitted)),
				"%q is not %q and must admit nobody, validated or not", prefix, "/")
		})
	}
}
