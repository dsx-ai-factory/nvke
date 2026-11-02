// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// Each case is an authorization.yaml that New must refuse.
func TestNewRefusesABadConfig(t *testing.T) {
	t.Parallel()

	// keycloak is an authorization.yaml with settings, a YAML flow mapping,
	// for the keycloak issuer.
	keycloak := func(settings string) string { return configHeader + `issuers: {keycloak: ` + settings + `}` }
	// claim is keycloak, with the claim mapping's fields, which grants reader.
	claim := func(fields string) string {
		return keycloak(`{tenant: {claim: org}, claims: [{` + fields + `, grants: [reader]}]}`)
	}
	// serviceAccount is cluster-sa, with the service account mapping's fields.
	serviceAccount := func(fields string) string {
		return configHeader + `issuers: {cluster-sa: {tenant: {fixed: tenant-a}, serviceAccounts: [{` + fields + `}]}}`
	}
	operators := func(settings string) string {
		return configHeader + "operatorTenant: " + operatorTenant + "\n" + `issuers: {keycloak: ` + settings + `}`
	}
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		// The file. strictyaml's tests cover the other file format problems.
		{name: "not a mapping", config: "[]", wantErr: "parse authorization.yaml"},
		{
			name:    "no apiVersion",
			config:  "kind: AuthorizationConfiguration\nissuers: {keycloak: {tenant: {claim: org}}}",
			wantErr: `authorization.yaml: apiVersion is required; this build accepts "auth.nvke.nvidia.com/v1alpha1"`,
		},
		{
			name:    "another apiVersion",
			config:  strings.Replace(keycloak(`{tenant: {claim: org}}`), "auth.nvke.nvidia.com/v1alpha1", "auth.nke.nvidia.com/v1alpha1", 1),
			wantErr: `authorization.yaml: apiVersion "auth.nke.nvidia.com/v1alpha1" is not supported; this build accepts "auth.nvke.nvidia.com/v1alpha1"`,
		},
		{
			name:    "no kind",
			config:  "apiVersion: auth.nvke.nvidia.com/v1alpha1\nissuers: {keycloak: {tenant: {claim: org}}}",
			wantErr: `authorization.yaml: kind is required; this build accepts "AuthorizationConfiguration"`,
		},
		{
			name:    "the authN config's kind",
			config:  strings.Replace(keycloak(`{tenant: {claim: org}}`), "AuthorizationConfiguration", "AuthenticationConfiguration", 1),
			wantErr: `authorization.yaml: kind "AuthenticationConfiguration" is not supported; this build accepts "AuthorizationConfiguration"`,
		},
		{name: "an unknown field", config: keycloak(`{tenant: {claim: org}}`) + "\noperator: x", wantErr: `unknown field "operator"`},
		{
			name:    "a duplicate key",
			config:  keycloak(`{tenant: {claim: org}, operators: false, operators: true}`),
			wantErr: `parse authorization.yaml: yaml: unmarshal errors:` + "\n" + `  line 3: key "operators" already set in map`,
		},
		{
			name:    "two spellings of a service account's name",
			config:  serviceAccount(`namespace: ci, name: builder, NAME: null, grants: [reader]`),
			wantErr: `unknown field "issuers.cluster-sa.serviceAccounts[0].NAME"`,
		},

		// Issuers.
		{name: "no issuers", config: configHeader, wantErr: "authorization.yaml: issuers is empty, so no caller would have a tenant or a role"},
		{
			name:    "an issuer not in the authN config",
			config:  configHeader + `issuers: {github: {tenant: {claim: org}}}`,
			wantErr: `authorization.yaml: issuers.github: isn't an issuer in the authN config`,
		},

		// Tenancy.
		{name: "no tenant source", config: keycloak(`{}`), wantErr: "authorization.yaml: issuers.keycloak.tenant: set exactly one of claim and fixed"},
		{
			name:    "two tenant sources",
			config:  keycloak(`{tenant: {claim: org, fixed: tenant-a}}`),
			wantErr: "issuers.keycloak.tenant: set exactly one of claim and fixed",
		},
		{name: "an aud tenant claim", config: keycloak(`{tenant: {claim: aud}}`), wantErr: `tenant: claim can't be "aud"`},
		{name: "a malformed fixed tenant", config: keycloak(`{tenant: {fixed: "a\"b"}}`), wantErr: "issuers.keycloak.tenant: fixed holds U+0022 at byte 1"},
		{
			name:    "a malformed operator tenant",
			config:  configHeader + "operatorTenant: \" x\"\nissuers: {keycloak: {tenant: {claim: org}}}",
			wantErr: "authorization.yaml: operatorTenant has leading or trailing whitespace",
		},
		{
			name:    "operators with no operator tenant",
			config:  keycloak(`{tenant: {claim: org}, operators: true}`),
			wantErr: "authorization.yaml: issuers.keycloak: operators is set, but operatorTenant isn't",
		},
		{
			name:    "operators with another fixed tenant",
			config:  operators(`{tenant: {fixed: tenant-a}, operators: true}`),
			wantErr: `issuers.keycloak: operators is set, but tenant.fixed "tenant-a" isn't operatorTenant "` + operatorTenant + `"`,
		},
		{
			name:    "the operator tenant fixed without operators",
			config:  operators(`{tenant: {fixed: ` + operatorTenant + `}}`),
			wantErr: "issuers.keycloak: tenant.fixed is operatorTenant, but operators isn't set, so every caller would be an impostor",
		},

		// Claim mappings.
		{
			name:    "an aud claim",
			config:  claim(`name: aud, value: nvke`),
			wantErr: `issuers.keycloak.claims[0]: name can't be "aud", because the caller chooses its value`,
		},
		{name: "a nonce claim", config: claim(`name: nonce, value: x`), wantErr: `issuers.keycloak.claims[0]: name can't be "nonce"`},
		{name: "an empty claim name", config: claim(`name: "", value: b`), wantErr: "issuers.keycloak.claims[0]: name is empty"},
		{name: "a padded claim name", config: claim(`name: "groups ", value: b`), wantErr: `name "groups " has leading or trailing whitespace`},
		{name: "an empty claim value", config: claim(`name: groups, value: ""`), wantErr: "issuers.keycloak.claims[0]: value is empty"},
		{
			name:    "a padded claim value",
			config:  claim(`name: groups, value: " readers"`),
			wantErr: `issuers.keycloak.claims[0]: value " readers" has leading or trailing whitespace`,
		},
		{
			name:    "a claim mapping with no grants",
			config:  keycloak(`{tenant: {claim: org}, claims: [{name: groups, value: b, grants: []}]}`),
			wantErr: "issuers.keycloak.claims[0]: grants is empty",
		},
		{
			name:    "a null grant",
			config:  keycloak(`{tenant: {claim: org}, claims: [{name: groups, value: b, grants: [reader, ~]}]}`),
			wantErr: "issuers.keycloak.claims[0]: grants[1] is empty",
		},
		{
			name:    "a padded grant",
			config:  keycloak(`{tenant: {claim: org}, claims: [{name: groups, value: b, grants: ["reader "]}]}`),
			wantErr: `issuers.keycloak.claims[0]: grants[0] "reader " has leading or trailing whitespace`,
		},
		{
			name: "a claim mapping granting an undeclared role",
			config: keycloak(`{tenant: {claim: org}, claims: [{name: groups, value: a, grants: [reader]},
  {name: groups, value: b, grants: [raeder]}]}`),
			wantErr: `authorization.yaml: issuers.keycloak.claims[1]: grants[0] "raeder" isn't a role roles.json declares`,
		},
		{
			name:    "a grant in the entity reference form",
			config:  keycloak(`{tenant: {claim: org}, claims: [{name: groups, value: b, grants: [{type: "NVKE::Role", id: reader}]}]}`),
			wantErr: "parse authorization.yaml",
		},

		// Service account mappings.
		{name: "a bad namespace", config: serviceAccount(`namespace: Not_A_Namespace, grants: [reader]`), wantErr: "serviceAccounts[0]: namespace"},
		{
			name:    "an empty service account name",
			config:  serviceAccount(`namespace: ci, name: "", grants: [reader]`),
			wantErr: "issuers.cluster-sa.serviceAccounts[0]: name is present but empty",
		},
		{
			name: "a name with no value",
			config: configHeader + `issuers:
  cluster-sa:
    tenant: {fixed: tenant-a}
    serviceAccounts:
      - namespace: ci
        name:
        grants: [reader]
`,
			wantErr: "issuers.cluster-sa.serviceAccounts[0]: name is null",
		},
		{
			name:    "a service account mapping with no grants",
			config:  serviceAccount(`namespace: ci`),
			wantErr: "issuers.cluster-sa.serviceAccounts[0]: grants is empty",
		},
		{
			name:    "a service account mapping granting an undeclared role",
			config:  serviceAccount(`namespace: ci, grants: [admin]`),
			wantErr: `issuers.cluster-sa.serviceAccounts[0]: grants[0] "admin" isn't a role roles.json declares`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := validBundle()
			f["authorization.yaml"] = tt.config
			_, err := newLocalBundle(t, f)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// New reports every problem in authorization.yaml in one error, each with its
// path, with issuers in sorted order. A map of more than eight issuers is
// almost never iterated in sorted order, so a load that doesn't sort fails.
func TestNewReportsEveryConfigProblem(t *testing.T) {
	t.Parallel()

	config := configHeader + `issuers:
  keycloak:
    tenant: {}
    claims:
      - {name: aud, value: b, grants: [raeder]}
  cluster-sa:
    tenant: {fixed: tenant-a}
    serviceAccounts:
      - {namespace: ci, grants: []}
`
	unknown := map[string]string{}
	for _, issuer := range []string{"auth0", "dex", "github", "gitlab", "okta", "onelogin", "ping", "zitadel"} {
		config += "  " + issuer + ": {tenant: {claim: org}}\n"
		unknown[issuer] = "authorization.yaml: issuers." + issuer + `: isn't an issuer in the authN config (["keycloak" "entra" "cluster-sa"])`
	}
	f := validBundle()
	f["authorization.yaml"] = config
	_, err := newLocalBundle(t, f)
	assert.EqualError(t, err, strings.Join([]string{
		unknown["auth0"],
		"authorization.yaml: issuers.cluster-sa.serviceAccounts[0]: grants is empty",
		unknown["dex"],
		unknown["github"],
		unknown["gitlab"],
		"authorization.yaml: issuers.keycloak.tenant: set exactly one of claim and fixed",
		`authorization.yaml: issuers.keycloak.claims[0]: name can't be "aud", because the caller chooses its value`,
		`authorization.yaml: issuers.keycloak.claims[0]: grants[0] "raeder" isn't a role roles.json declares`,
		unknown["okta"],
		unknown["onelogin"],
		unknown["ping"],
		unknown["zitadel"],
	}, "\n"))
}

// New reports a problem in authorization.yaml and one in policies.cedar in
// one error. After roles.json fails, it doesn't check grants against it.
func TestNewJoinsConfigErrorsWithOtherFiles(t *testing.T) {
	t.Parallel()

	f := validBundle()
	f["policies.cedar"] += `permit(principal, action, resource) when { principal.c == 3 };`
	f["roles.json"] = `{}`
	f["authorization.yaml"] = configHeader + `issuers: {github: {tenant: {claim: org}, claims: [{name: g, value: v, grants: [reader]}]}}`
	_, err := newLocalBundle(t, f)
	require.ErrorContains(t, err, `reads attribute "c" of principal`)
	require.ErrorContains(t, err, "parse roles.json")
	require.ErrorContains(t, err, "authorization.yaml: issuers.github: isn't an issuer in the authN config")
	assert.NotContains(t, err.Error(), "isn't a role roles.json declares")
}

// Each claim and service account mapping grants its roles only to a matching
// caller from its own issuer, and a caller holds the roles of every mapping it
// matches.
func TestAuthorizeGrantsRoles(t *testing.T) {
	t.Parallel()

	f := validBundle()
	f["policies.cedar"] += `@id("auditors-and-readers-may-delete")
permit(principal in NVKE::Role::"auditor", action == NVKE::Action::"deleteCluster", resource)
when { principal in NVKE::Role::"reader" };`
	f["roles.json"] = `[` + reader + `,
  {"uid": {"type": "NVKE::Role", "id": "admin"}, "parents": [{"type": "NVKE::Role", "id": "reader"}]},
  {"uid": {"type": "NVKE::Role", "id": "auditor"}, "parents": []}]`
	f["authorization.yaml"] = configHeader + `issuers:
  keycloak:
    tenant: {claim: organization}
    claims:
      - {name: groups, value: auditors, grants: [auditor]}
      - {name: groups, value: readers, grants: [reader]}
      - {name: groups, value: admins, grants: [admin]}
  cluster-sa:
    tenant: {fixed: tenant-a}
    serviceAccounts:
      - {namespace: ci, name: builder, grants: [reader]}
      - {namespace: ops, grants: [reader]}
      - {namespace: ops, name: fleet, grants: [auditor]}
`
	lb, err := newLocalBundle(t, f)
	require.NoError(t, err)

	serviceAccount := func(namespace, name, subject string) authn.Principal {
		return authn.Principal{IssuerName: "cluster-sa", Subject: subject, Claims: map[string]any{
			"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]any{"name": name}},
		}}
	}
	keycloak := func(groups any) authn.Principal {
		return authn.Principal{IssuerName: "keycloak", Subject: "alice", Claims: map[string]any{"groups": groups}}
	}
	tests := []struct {
		name       string
		principal  authn.Principal
		action     string
		want       authz.Outcome
		wantReason string
	}{
		{name: "a claim equal to the value", principal: keycloak("readers"), want: authz.OutcomeAllow},
		{name: "a claim list holding the value", principal: keycloak([]any{"devs", "readers"}), want: authz.OutcomeAllow},
		{name: "a claim list holding a non-string", principal: keycloak([]any{"readers", 1}), want: authz.OutcomeDeny},
		{name: "a claim with another value", principal: keycloak("devs"), want: authz.OutcomeDeny},
		{name: "no claim", principal: authn.Principal{IssuerName: "keycloak", Subject: "alice"}, want: authz.OutcomeDeny},
		{
			name:      "the claim from another issuer",
			principal: authn.Principal{IssuerName: "entra", Subject: "alice", Claims: map[string]any{"groups": "readers"}},
			want:      authz.OutcomeDeny,
		},
		{name: "a granted role nested in the permitted role", principal: keycloak("admins"), want: authz.OutcomeAllow},
		{
			name:       "two matching claim mappings",
			principal:  keycloak([]any{"auditors", "readers"}),
			action:     "deleteCluster",
			want:       authz.OutcomeAllow,
			wantReason: "auditors-and-readers-may-delete",
		},
		{name: "a claim matching only one of two needed mappings", principal: keycloak("auditors"), action: "deleteCluster", want: authz.OutcomeDeny},
		{
			name:      "the named service account",
			principal: serviceAccount("ci", "builder", "system:serviceaccount:ci:builder"),
			want:      authz.OutcomeAllow,
		},
		{
			name:      "another service account in its namespace",
			principal: serviceAccount("ci", "tester", "system:serviceaccount:ci:tester"),
			want:      authz.OutcomeDeny,
		},
		{
			name:      "any service account in a namespace",
			principal: serviceAccount("ops", "other", "system:serviceaccount:ops:other"),
			want:      authz.OutcomeAllow,
		},
		{
			name:       "two matching service account mappings",
			principal:  serviceAccount("ops", "fleet", "system:serviceaccount:ops:fleet"),
			action:     "deleteCluster",
			want:       authz.OutcomeAllow,
			wantReason: "auditors-and-readers-may-delete",
		},
		{
			name:      "a service account claim that disagrees with the subject",
			principal: serviceAccount("ci", "builder", "system:serviceaccount:ci:tester"),
			want:      authz.OutcomeDeny,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			action := tt.action
			if action == "" {
				action = "getCluster"
			}
			d, err := lb.Authorize(t.Context(), authz.Query{
				Principal: tt.principal,
				Action:    action,
				Resource:  authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeGlobal},
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d.Outcome)
			if tt.wantReason != "" {
				assert.Equal(t, []string{tt.wantReason}, d.Reasons)
			}
		})
	}
}

// The conformance suite covers isolation and operators. These are the
// callers its fixture doesn't cover.
func TestAuthorizeReadsTheCallersTenancy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, conformanceBundle())
	lb, err := newConformanceBundle(dir)
	require.NoError(t, err)

	cluster := authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeObject, Tenant: "tenant-a"}
	global := authz.Resource{Type: "Collection", ID: "releases", Scope: authz.ScopeGlobal}
	tests := []struct {
		name       string
		principal  authn.Principal
		resource   authz.Resource
		want       authz.Outcome
		wantCaller authz.Caller
	}{
		{
			name:       "an operator whose tenant comes from a claim",
			principal:  authn.Principal{IssuerName: "entra", Subject: "ops", Claims: map[string]any{"tid": operatorTenant}},
			resource:   cluster,
			want:       authz.OutcomeAllow,
			wantCaller: authz.Caller{Tenant: operatorTenant, Operator: true},
		},
		{
			name:       "an operator issuer's caller in another tenant",
			principal:  authn.Principal{IssuerName: "entra", Subject: "eve", Claims: map[string]any{"tid": "tenant-b"}},
			resource:   cluster,
			want:       authz.OutcomeNotFound,
			wantCaller: authz.Caller{Tenant: "tenant-b"},
		},
		{
			name:      "a tenant claim that isn't a string",
			principal: authn.Principal{IssuerName: "keycloak", Subject: "alice", Claims: map[string]any{"organization": []any{"tenant-a"}}},
			resource:  cluster,
			want:      authz.OutcomeNoTenant,
		},
		{
			name:       "a malformed tenant, on a global resource",
			principal:  keycloakIn("alice", " tenant-a"),
			resource:   global,
			want:       authz.OutcomeNoTenant,
			wantCaller: authz.Caller{Tenant: " tenant-a"},
		},
		{
			name:      "an issuer authorization.yaml doesn't list",
			principal: authn.Principal{IssuerName: "github", Subject: "alice", Claims: map[string]any{"organization": "tenant-a"}},
			resource:  cluster,
			want:      authz.OutcomeNoTenant,
		},
		{
			name:       "an impostor, on a global resource",
			principal:  keycloakIn("mallory", operatorTenant),
			resource:   global,
			want:       authz.OutcomeDeny,
			wantCaller: authz.Caller{Tenant: operatorTenant},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d, err := lb.Authorize(t.Context(), authz.Query{Principal: tt.principal, Action: "getCluster", Resource: tt.resource})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d.Outcome)
			assert.Equal(t, tt.wantCaller, d.Caller)
		})
	}
}
