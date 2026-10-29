// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
)

const grantsReader = `"grants": [{"type": "NVKE::Role", "id": "reader"}]`

// Each case is a role mapping that New must refuse.
func TestNewRefusesBadRoleMappings(t *testing.T) {
	t.Parallel()

	entry := func(fields string) string { return `[{"issuer": "keycloak", ` + fields + `, ` + grantsReader + `}]` }
	tests := []struct {
		name     string
		mappings string
		wantErr  string
	}{
		{name: "not an array", mappings: `{}`, wantErr: "parsing roleMappings.json: json: cannot unmarshal object"},
		{name: "no role mappings", mappings: `[]`, wantErr: "roleMappings.json has no role mappings, so no caller would hold a role"},
		{name: "an unknown field", mappings: entry(`"claims": {"name": "a", "value": "b"}`), wantErr: `unknown field "[0].claims"`},
		{
			name:     "two spellings of one key",
			mappings: entry(`"serviceAccount": {"namespace": "ci", "name": "builder", "NAME": null}`),
			wantErr:  `unknown field "[0].serviceAccount.NAME"`,
		},
		{
			name:     "a null service account name",
			mappings: entry(`"serviceAccount": {"namespace": "ci", "name": null}`),
			wantErr:  "roleMappings.json[0]: serviceAccount: name is null",
		},
		{
			name:     "an issuer authN doesn't configure",
			mappings: `[{"issuer": "github", "claim": {"name": "a", "value": "b"}, ` + grantsReader + `}]`,
			wantErr:  `roleMappings.json[0]: issuer "github" isn't in the authN config`,
		},
		{name: "no signal", mappings: `[{"issuer": "keycloak", ` + grantsReader + `}]`, wantErr: "set exactly one of claim and serviceAccount"},
		{
			name:     "two signals",
			mappings: entry(`"claim": {"name": "a", "value": "b"}, "serviceAccount": {"namespace": "ci"}`),
			wantErr:  "set exactly one of claim and serviceAccount",
		},
		{name: "an aud claim", mappings: entry(`"claim": {"name": "aud", "value": "nvke"}`), wantErr: `claim: name can't be "aud"`},
		{name: "an empty claim name", mappings: entry(`"claim": {"name": "", "value": "b"}`), wantErr: "claim: name is empty"},
		{name: "a padded claim name", mappings: entry(`"claim": {"name": "groups ", "value": "b"}`), wantErr: "has leading or trailing whitespace"},
		{name: "an empty claim value", mappings: entry(`"claim": {"name": "groups", "value": ""}`), wantErr: "claim: value is empty"},
		{name: "a bad namespace", mappings: entry(`"serviceAccount": {"namespace": "Not_A_Namespace"}`), wantErr: "serviceAccount: namespace"},
		{name: "no grants", mappings: `[{"issuer": "keycloak", "claim": {"name": "a", "value": "b"}, "grants": []}]`, wantErr: "grants names no role"},
		{
			name:     "an undeclared role",
			mappings: `[{"issuer": "keycloak", "claim": {"name": "a", "value": "b"}, "grants": [{"type": "NVKE::Role", "id": "admin"}]}]`,
			wantErr:  `grants[0] is NVKE::Role::"admin", which isn't a declared NVKE::Role`,
		},
		{
			name:     "a grant of an action",
			mappings: `[{"issuer": "keycloak", "claim": {"name": "a", "value": "b"}, "grants": [{"type": "NVKE::Action", "id": "read"}]}]`,
			wantErr:  `grants[0] is NVKE::Action::"read", which isn't a declared NVKE::Role`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := validBundle()
			f["roleMappings.json"] = tt.mappings
			_, err := newLocalBundle(t, f)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// New reports every bad role mapping in one error, each with its index.
func TestNewReportsEveryBadRoleMapping(t *testing.T) {
	t.Parallel()

	f := validBundle()
	f["roleMappings.json"] = `[
  {"issuer": "github", "claim": {"name": "a", "value": "b"}, ` + grantsReader + `},
  {"issuer": "keycloak", "claim": {"name": "aud", "value": "b"}, ` + grantsReader + `}]`
	_, err := newLocalBundle(t, f)
	require.ErrorContains(t, err, `roleMappings.json[0]: issuer "github"`)
	assert.ErrorContains(t, err, `roleMappings.json[1]: claim: name can't be "aud"`)
}

// Each kind of role mapping grants its roles only to a matching caller from
// its own issuer.
func TestRoleMappingsGrantRoles(t *testing.T) {
	t.Parallel()

	const mappings = `[
  {"issuer": "keycloak", "claim": {"name": "groups", "value": "readers"}, "grants": [{"type": "NVKE::Role", "id": "reader"}]},
  {"issuer": "keycloak", "claim": {"name": "groups", "value": "admins"}, "grants": [{"type": "NVKE::Role", "id": "admin"}]},
  {"issuer": "cluster-sa", "serviceAccount": {"namespace": "ci", "name": "builder"}, ` + grantsReader + `},
  {"issuer": "cluster-sa", "serviceAccount": {"namespace": "ops"}, ` + grantsReader + `}
]`
	serviceAccount := func(namespace, name, subject string) authn.Principal {
		return authn.Principal{IssuerName: "cluster-sa", Subject: subject, Claims: map[string]any{
			"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]any{"name": name}},
		}}
	}
	keycloak := func(groups any) authn.Principal {
		return authn.Principal{IssuerName: "keycloak", Subject: "alice", Claims: map[string]any{"groups": groups}}
	}
	tests := []struct {
		name      string
		principal authn.Principal
		want      authz.Outcome
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
			principal: serviceAccount("ops", "fleet", "system:serviceaccount:ops:fleet"),
			want:      authz.OutcomeAllow,
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

			f := validBundle()
			f["roles.json"] = `[` + reader + `,
  {"uid": {"type": "NVKE::Role", "id": "admin"}, "parents": [{"type": "NVKE::Role", "id": "reader"}]}]`
			f["roleMappings.json"] = mappings
			lb, err := newLocalBundle(t, f)
			require.NoError(t, err)
			d, err := lb.Authorize(t.Context(), authz.Query{
				Principal: tt.principal,
				Action:    "getCluster",
				Resource:  authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeGlobal},
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d.Outcome)
		})
	}
}
