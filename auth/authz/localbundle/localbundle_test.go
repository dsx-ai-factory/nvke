// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// LocalBundle doesn't run authztest.Run: the suite needs callers in two
// tenants, and LocalBundle gives no caller a tenant.
package localbundle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"
)

// files is a bundle, as file name to content.
type files map[string]string

// testOptions are the actions and resource types the test API asks about.
func testOptions(dir string) localbundle.Options {
	return localbundle.Options{
		Dir:           dir,
		Issuers:       []string{"keycloak", "entra", "cluster-sa"},
		Actions:       []string{"getCluster", "deleteCluster", "listReleases"},
		ResourceTypes: []string{"Cluster", "Collection"},
	}
}

const (
	reader      = `{"uid": {"type": "NVKE::Role", "id": "reader"}, "attrs": {}, "parents": []}`
	readGroup   = `{"uid": {"type": "NVKE::Action", "id": "read"}, "attrs": {}, "parents": []}`
	readActions = readGroup + `,
  {"uid": {"type": "NVKE::Action", "id": "getCluster"}, "attrs": {}, "parents": [{"type": "NVKE::Action", "id": "read"}]},
  {"uid": {"type": "NVKE::Action", "id": "listReleases"}, "attrs": {}, "parents": [{"type": "NVKE::Action", "id": "read"}]}`
	deleteCluster = `{"uid": {"type": "NVKE::Action", "id": "deleteCluster"}, "attrs": {}, "parents": []}`
)

// validBundle returns a bundle that loads, for each test to change.
func validBundle() files {
	return files{
		"policies.cedar": `
@id("readers-may-read")
permit(principal in NVKE::Role::"reader", action in NVKE::Action::"read", resource);

@id("anyone-may-list-releases")
permit(principal, action == NVKE::Action::"listReleases", resource);
`,
		"roles.json":   `[` + reader + `]`,
		"actions.json": `[` + readActions + `, ` + deleteCluster + `]`,
		"roleMappings.json": `[{"issuer": "keycloak", "claim": {"name": "groups", "value": "readers"},
  "grants": [{"type": "NVKE::Role", "id": "reader"}]}]`,
	}
}

// writeBundle writes each file in f to dir, and leaves dir's other files in
// place.
func writeBundle(t *testing.T, dir string, f files) {
	t.Helper()
	for name, content := range f {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
}

// newLocalBundle writes f to a new directory and loads it with testOptions.
func newLocalBundle(t *testing.T, f files) (*localbundle.LocalBundle, error) {
	t.Helper()
	dir := t.TempDir()
	writeBundle(t, dir, f)
	return localbundle.New(testOptions(dir))
}

// Each case breaks a valid bundle in one way, and New must refuse it.
func TestNewRefusesABadBundle(t *testing.T) {
	t.Parallel()

	policy := func(p string) func(files) {
		return func(f files) { f["policies.cedar"] += "\n" + p }
	}
	roles := func(r string) func(files) {
		return func(f files) { f["roles.json"] = r }
	}
	actions := func(a string) func(files) {
		return func(f files) { f["actions.json"] = a }
	}
	tests := []struct {
		name    string
		edit    func(files)
		wantErr string
	}{
		{name: "no policies file", edit: func(f files) { delete(f, "policies.cedar") }, wantErr: "reading policies.cedar"},
		{name: "no roles file", edit: func(f files) { delete(f, "roles.json") }, wantErr: "reading roles.json"},
		{name: "no actions file", edit: func(f files) { delete(f, "actions.json") }, wantErr: "reading actions.json"},
		{name: "policies that don't parse", edit: policy("permit("), wantErr: "parsing policies.cedar"},
		{name: "no policies", edit: func(f files) { f["policies.cedar"] = "" }, wantErr: "policies.cedar declares no policies"},
		{name: "no roles", edit: roles(`[]`), wantErr: "roles.json declares no roles"},

		// Entity files.
		{
			name:    "a principal in roles.json",
			edit:    roles(`[` + reader + `, {"uid": {"type": "NVKE::Principal", "id": "alice"}, "parents": []}]`),
			wantErr: `roles.json[1] is NVKE::Principal::"alice"; the file holds NVKE::Role entities only`,
		},
		{
			name:    "a role in actions.json",
			edit:    actions(`[` + readActions + `, ` + deleteCluster + `, ` + reader + `]`),
			wantErr: `actions.json[4] is NVKE::Role::"reader"; the file holds NVKE::Action entities only`,
		},
		{name: "a role declared twice", edit: roles(`[` + reader + `, ` + reader + `]`), wantErr: `roles.json[1] declares NVKE::Role::"reader" again`},
		{
			name: "a duplicate parents key",
			edit: actions(`[` + readActions + `, {"uid": {"type": "NVKE::Action", "id": "deleteCluster"},
  "parents": [{"type": "NVKE::Action", "id": "read"}], "parents": []}]`),
			wantErr: `parsing actions.json: duplicate field "[3].parents"`,
		},
		{
			name:    "a misspelled parents key",
			edit:    roles(`[{"uid": {"type": "NVKE::Role", "id": "reader"}, "parent": []}]`),
			wantErr: `parsing roles.json: unknown field "[0].parent"`,
		},
		{
			name:    "attributes",
			edit:    roles(`[{"uid": {"type": "NVKE::Role", "id": "reader"}, "attrs": {"level": 3}, "parents": []}]`),
			wantErr: "roles.json[0] has attributes, which no policy may read",
		},
		{
			name:    "a parent no file declares",
			edit:    roles(`[{"uid": {"type": "NVKE::Role", "id": "reader"}, "parents": [{"type": "NVKE::Role", "id": "admin"}]}]`),
			wantErr: `NVKE::Role::"reader" has parent NVKE::Role::"admin", which isn't a declared NVKE::Role`,
		},
		{
			name:    "a role nested in an action",
			edit:    roles(`[{"uid": {"type": "NVKE::Role", "id": "reader"}, "parents": [{"type": "NVKE::Action", "id": "read"}]}]`),
			wantErr: `NVKE::Role::"reader" has parent NVKE::Action::"read", which isn't a declared NVKE::Role`,
		},
		{
			name:    "an action in a role",
			edit:    actions(`[` + readActions + `, {"uid": {"type": "NVKE::Action", "id": "deleteCluster"}, "parents": [{"type": "NVKE::Role", "id": "reader"}]}]`),
			wantErr: `NVKE::Action::"deleteCluster" has parent NVKE::Role::"reader", which isn't a declared NVKE::Action`,
		},
		{
			name:    "an action the API asks about and the bundle lacks",
			edit:    actions(`[` + readActions + `]`),
			wantErr: `actions.json doesn't declare the API's action "deleteCluster"`,
		},
		{
			name: "an API action as an action group",
			edit: actions(`[` + readActions + `, {"uid": {"type": "NVKE::Action", "id": "deleteCluster"},
  "parents": [{"type": "NVKE::Action", "id": "getCluster"}]}]`),
			wantErr: `actions.json makes the API's action "getCluster" a parent of NVKE::Action::"deleteCluster"; ` +
				`an API action can't be an action group`,
		},

		// Names in policies.
		{
			name:    "an undeclared action",
			edit:    policy(`@id("typo") permit(principal, action == NVKE::Action::"getClustre", resource);`),
			wantErr: `policy typo names NVKE::Action::"getClustre", which no entity file declares`,
		},
		{
			name:    "an undeclared role in a condition",
			edit:    policy(`@id("typo") permit(principal, action, resource) when { principal in NVKE::Role::"raeder" };`),
			wantErr: `policy typo names NVKE::Role::"raeder"`,
		},
		{
			name:    "an undeclared role in an is-in scope",
			edit:    policy(`@id("typo") permit(principal is NVKE::Principal in NVKE::Role::"raeder", action, resource);`),
			wantErr: `policy typo names NVKE::Role::"raeder"`,
		},
		{
			name:    "a role no policy names",
			edit:    roles(`[` + reader + `, {"uid": {"type": "NVKE::Role", "id": "auditor"}, "parents": []}]`),
			wantErr: `roles.json declares NVKE::Role::"auditor", but no policy names it or a role it's nested in`,
		},
		{
			name:    "an action without the namespace",
			edit:    policy(`@id("f") forbid(principal, action == Action::"listReleases", resource);`),
			wantErr: "policy f compares the action with Action",
		},
		{
			name:    "a misspelled action type in a list",
			edit:    policy(`@id("f") forbid(principal, action in [NVKE::Actions::"listReleases"], resource);`),
			wantErr: "policy f compares the action with NVKE::Actions",
		},
		{
			name:    "a resource type without the namespace",
			edit:    policy(`@id("f") forbid(principal, action, resource is Cluster);`),
			wantErr: "policy f compares the resource with Cluster",
		},
		{
			name:    "a misspelled resource type in a condition",
			edit:    policy(`@id("f") forbid(principal, action, resource) when { resource is NVKE::Clustr };`),
			wantErr: "policy f names type NVKE::Clustr, which isn't the principal, an action, a role or an API resource type",
		},
		{
			name:    "a principal compared with a role",
			edit:    policy(`@id("f") forbid(principal == NVKE::Role::"reader", action, resource);`),
			wantErr: "policy f compares the principal with NVKE::Role",
		},
		{
			name: "two policies with one @id",
			edit: policy(`@id("anyone-may-list-releases")
forbid(principal, action == NVKE::Action::"deleteCluster", resource);`),
			wantErr: `two policies have @id "anyone-may-list-releases"`,
		},

		// Attribute reads.
		{
			name:    "a resource attribute",
			edit:    policy(`@id("prod") forbid(principal, action, resource) when { resource has site && resource.site == "prod" };`),
			wantErr: `policy prod reads attribute "site" of resource; a policy may read only context.issuer`,
		},
		{
			name:    "an attribute read followed by context.issuer",
			edit:    policy(`forbid(principal, action, resource) when { resource has site && context has issuer };`),
			wantErr: `reads attribute "site" of resource`,
		},
		{name: "a principal attribute", edit: policy(`permit(principal, action, resource) when { principal.team == "a" };`), wantErr: `reads attribute "team" of principal`},
		{name: "another context attribute", edit: policy(`permit(principal, action, resource) when { context has method };`), wantErr: `reads attribute "method" of context`},
		{name: "a tag test", edit: policy(`permit(principal, action, resource) when { resource.hasTag("a") };`), wantErr: "reads a tag of resource"},
		{name: "a tag read", edit: policy(`permit(principal, action, resource) when { resource.getTag("a") == "b" };`), wantErr: "reads a tag of resource"},
		{
			name:    "an attribute of a literal",
			edit:    policy(`permit(principal, action, resource) when { NVKE::Cluster::"c1".site == "a" };`),
			wantErr: `reads attribute "site" of NVKE::Cluster::"c1"`,
		},
		{
			name:    "an attribute of an expression",
			edit:    policy(`permit(principal, action, resource) when { (if true then resource else principal).site == "a" };`),
			wantErr: `reads attribute "site" of an expression`,
		},
		{name: "an attribute of the issuer", edit: policy(`permit(principal, action, resource) when { context.issuer.x == "a" };`), wantErr: `reads attribute "x" of an expression`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := validBundle()
			tt.edit(f)
			_, err := newLocalBundle(t, f)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// New reports a policy problem and an entity problem in one error.
func TestNewJoinsCheckErrors(t *testing.T) {
	t.Parallel()

	f := validBundle()
	f["policies.cedar"] += `permit(principal, action, resource) when { principal.c == 3 };`
	f["roles.json"] = `[]`
	_, err := newLocalBundle(t, f)
	require.ErrorContains(t, err, `reads attribute "c" of principal`)
	assert.ErrorContains(t, err, "roles.json declares no roles")
}

// New refuses Options that leave out a required field or name a reserved
// resource type.
func TestNewRefusesBadOptions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, validBundle())
	for name, tt := range map[string]struct {
		opts    localbundle.Options
		wantErr string
	}{
		"no directory": {
			opts:    localbundle.Options{Actions: []string{"getCluster"}, ResourceTypes: []string{"Cluster"}},
			wantErr: "the bundle directory is not set",
		},
		"no actions": {
			opts:    localbundle.Options{Dir: dir, ResourceTypes: []string{"Cluster"}},
			wantErr: "the API's actions and resource types must both be set",
		},
		"no resource types": {
			opts:    localbundle.Options{Dir: dir, Actions: []string{"getCluster"}},
			wantErr: "the API's actions and resource types must both be set",
		},
		"a role resource type": {
			opts:    localbundle.Options{Dir: dir, Actions: []string{"getCluster"}, ResourceTypes: []string{"Role"}},
			wantErr: `resource type "Role"`,
		},
		"an action group": {
			opts:    localbundle.Options{Dir: dir, Actions: []string{"getCluster", "read"}, ResourceTypes: []string{"Cluster"}},
			wantErr: `actions.json makes the API's action "read" a parent of NVKE::Action::"getCluster"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := localbundle.New(tt.opts)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// A role that no policy names is reachable when it's nested in a role that a
// policy names.
func TestNewAcceptsARoleNestedInANamedRole(t *testing.T) {
	t.Parallel()

	f := validBundle()
	f["roles.json"] = `[` + reader + `,
  {"uid": {"type": "NVKE::Role", "id": "admin"}, "parents": [{"type": "NVKE::Role", "id": "reader"}]}]`
	_, err := newLocalBundle(t, f)
	assert.NoError(t, err)
}

// New reads a mounted ConfigMap's files through its "..data" link.
func TestNewReadsTheCurrentConfigMapGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generation := filepath.Join(dir, "..2026_10_27_00_00_00.1")
	require.NoError(t, os.Mkdir(generation, 0o700))
	writeBundle(t, generation, validBundle())
	require.NoError(t, os.Symlink(filepath.Base(generation), filepath.Join(dir, "..data")))
	// The top-level policies.cedar doesn't parse, so New succeeds only if it
	// reads through "..data".
	writeBundle(t, dir, files{"policies.cedar": "not cedar"})

	_, err := localbundle.New(testOptions(dir))
	assert.NoError(t, err)
}

// Authorize refuses an invalid query, a request the isolation check stops,
// and an action or resource type outside the API's lists. Otherwise it
// returns Cedar's decision, or an error for an allow beside a failed policy.
func TestAuthorize(t *testing.T) {
	t.Parallel()

	keycloak := authn.Principal{IssuerName: "keycloak", Subject: "alice"}
	query := func(action string, r authz.Resource) authz.Query {
		return authz.Query{Principal: keycloak, Action: action, Resource: r}
	}
	global := func(typ, id string) authz.Resource {
		return authz.Resource{Type: typ, ID: id, Scope: authz.ScopeGlobal}
	}
	releases := global("Collection", "releases")
	issuerPolicy := func(f files) {
		f["policies.cedar"] += `@id("keycloak-may-delete")
permit(principal, action == NVKE::Action::"deleteCluster", resource) when { context.issuer == "keycloak" };`
	}
	tests := []struct {
		name        string
		edit        func(files)
		query       authz.Query
		want        authz.Outcome
		wantReasons []string
		wantErr     string
	}{
		{name: "a policy allows", query: query("listReleases", releases), want: authz.OutcomeAllow, wantReasons: []string{"anyone-may-list-releases"}},
		{name: "no policy allows", query: query("getCluster", global("Cluster", "c1")), want: authz.OutcomeDeny},
		{
			name: "several policies allow",
			edit: func(f files) {
				f["policies.cedar"] += `@id("a-anyone-may-read") permit(principal, action in NVKE::Action::"read", resource);`
			},
			query:       query("listReleases", releases),
			want:        authz.OutcomeAllow,
			wantReasons: []string{"a-anyone-may-read", "anyone-may-list-releases"},
		},
		{
			name:  "a tenant's object, from a caller with no tenant",
			query: query("listReleases", authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeObject, Tenant: "acme"}),
			want:  authz.OutcomeNoTenant,
		},
		{name: "an invalid query", query: query("", releases), wantErr: "authorization query is not valid"},
		{
			name:    "an action the API doesn't declare",
			edit:    func(f files) { f["policies.cedar"] += `permit(principal, action, resource);` },
			query:   query("deleteRelease", releases),
			wantErr: `action "deleteRelease" isn't one of the API's actions`,
		},
		{
			name:    "an action group",
			edit:    func(f files) { f["policies.cedar"] += `permit(principal, action, resource);` },
			query:   query("read", releases),
			wantErr: `action "read" isn't one of the API's actions`,
		},
		{
			name:    "a resource type the API doesn't declare",
			query:   query("listReleases", global("Role", "reader")),
			wantErr: `resource type "Role" isn't one of the API's resource types`,
		},
		{
			name:        "a policy naming the caller's issuer",
			edit:        issuerPolicy,
			query:       query("deleteCluster", global("Cluster", "c1")),
			want:        authz.OutcomeAllow,
			wantReasons: []string{"keycloak-may-delete"},
		},
		{
			name: "a policy naming another issuer",
			edit: issuerPolicy,
			query: authz.Query{
				Principal: authn.Principal{IssuerName: "entra", Subject: "alice"},
				Action:    "deleteCluster",
				Resource:  global("Cluster", "c1"),
			},
			want: authz.OutcomeDeny,
		},
		{
			name: "an allow while forbids error",
			edit: func(f files) {
				f["policies.cedar"] += `@id("b-broken") forbid(principal, action, resource) when { context.issuer < 1 };
@id("a-broken") forbid(principal, action, resource) when { context.issuer < 2 };`
			},
			query: query("listReleases", releases),
			wantErr: "policy a-broken failed to evaluate: type error: expected comparable value, got string\n" +
				"policy b-broken failed to evaluate",
		},
		{
			name: "a deny while a permit errors",
			edit: func(f files) {
				f["policies.cedar"] += `permit(principal, action, resource) when { context.issuer < 1 };`
			},
			query: query("getCluster", global("Cluster", "c1")),
			want:  authz.OutcomeDeny,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := validBundle()
			if tt.edit != nil {
				tt.edit(f)
			}
			lb, err := newLocalBundle(t, f)
			require.NoError(t, err)
			d, err := lb.Authorize(t.Context(), tt.query)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, d.Outcome)
			assert.Equal(t, authz.Caller{}, d.Caller, "LocalBundle gives no caller a tenant")
			assert.Equal(t, tt.wantReasons, d.Reasons)
		})
	}
}

// A nil or zero LocalBundle refuses every request.
func TestAnUnloadedLocalBundleRefuses(t *testing.T) {
	t.Parallel()

	for name, lb := range map[string]*localbundle.LocalBundle{"nil": nil, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.ErrorContains(t, lb.Ready(t.Context()), "no bundle is loaded")
			_, err := lb.Authorize(t.Context(), authz.Query{})
			assert.ErrorContains(t, err, "no bundle is loaded")
		})
	}
}
