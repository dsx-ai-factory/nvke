// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// mappingRoles is the declared role set the compile tests check grants
// against.
func mappingRoles() types.EntityMap {
	m := types.EntityMap{}
	for _, name := range []string{"reader", "operator", "nodeOperator"} {
		uid := types.NewEntityUID(RoleType, types.String(name))
		m[uid] = types.Entity{UID: uid, Parents: types.NewEntityUIDSet(), Attributes: types.NewRecord(nil)}
	}
	return m
}

func mustCompileMappings(t *testing.T, raw string) *roleMappings {
	t.Helper()
	m, errs := compileRoleMappings([]byte(raw), mappingRoles())
	require.Empty(t, errs)
	require.NotNil(t, m)
	return m
}

func roleIDs(uids []types.EntityUID) []string {
	ids := make([]string, 0, len(uids))
	for _, u := range uids {
		ids = append(ids, string(u.ID))
	}
	return ids
}

// ---------------------------------------------------------------------------
// Claim matching.
// ---------------------------------------------------------------------------

func TestClaimMatches(t *testing.T) {
	t.Parallel()

	c := &claimMatch{Name: "groups", Value: "eng-a"}
	tests := []struct {
		name   string
		claims map[string]any
		want   bool
	}{
		{"claim absent", map[string]any{}, false},
		{"nil claims", nil, false},
		{"a matching string claim", map[string]any{"groups": "eng-a"}, true},
		{"a present but different string claim", map[string]any{"groups": "eng-b"}, false},
		{"an array claim containing the value, Entra's shape", map[string]any{"groups": []any{"eng-b", "eng-a"}}, true},
		{"an array claim not containing the value", map[string]any{"groups": []any{"eng-b"}}, false},
		{"a []string claim containing the value", map[string]any{"groups": []string{"eng-a"}}, true},
		{"the wrong JSON type: a number", map[string]any{"groups": float64(7)}, false},
		{"the wrong JSON type: an object", map[string]any{"groups": map[string]any{"eng-a": true}}, false},
		{"the wrong JSON type: a bool", map[string]any{"groups": true}, false},
		{
			// The whole claim fails, not just the bad element: "eng-a" is
			// present, so this tells "rejected the claim" from "matched the
			// element that happened to be a string".
			"a mixed-type array matches nothing, not a partial match",
			map[string]any{"groups": []any{"eng-a", 7}},
			false,
		},
		{"a different claim carrying the value", map[string]any{"roles": "eng-a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, claimMatches(tt.claims, c))
		})
	}
}

// ---------------------------------------------------------------------------
// parentsFor: service accounts, SPIFFE, and the union across entries.
// ---------------------------------------------------------------------------

func TestParentsForServiceAccount(t *testing.T) {
	t.Parallel()

	m := mustCompileMappings(t, `[
 {"issuer":"k8s","serviceAccount":{"namespace":"ci","name":"deploy-bot"},"grants":[{"type":"NKE::Role","id":"operator"}]},
 {"issuer":"k8s","serviceAccount":{"namespace":"readers"},"grants":[{"type":"NKE::Role","id":"reader"}]}
]`)

	tests := []struct {
		name string
		sa   *authn.ServiceAccount
		want []string
	}{
		{"an exact match", &authn.ServiceAccount{Namespace: "ci", Name: "deploy-bot"}, []string{"operator"}},
		{"namespace matches, name does not: no partial credit", &authn.ServiceAccount{Namespace: "ci", Name: "other"}, []string{}},
		{"a namespace-only entry matches every name in it", &authn.ServiceAccount{Namespace: "readers", Name: "anyone"}, []string{"reader"}},
		{"a namespace-only entry matches nothing outside it", &authn.ServiceAccount{Namespace: "elsewhere", Name: "anyone"}, []string{}},
		{"no verified service account", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := m.parentsFor(Query{Issuer: "k8s", Principal: "whatever", ServiceAccount: tt.sa}, nil)
			assert.Equal(t, tt.want, roleIDs(got))
		})
	}
}

// A SPIFFE path convention scoped to a trust domain, with an expression that
// returns bool.
const nodeOperatorExpression = `path.startsWith('/ns/nke-system/sa/') && path.split('/')[4].startsWith('node-')`

func TestParentsForSPIFFE(t *testing.T) {
	t.Parallel()

	m := mustCompileMappings(t, `[{"issuer":"spire",
  "spiffe":{"trustDomain":"nvidia.com","expression":"`+nodeOperatorExpression+`"},
  "grants":[{"type":"NKE::Role","id":"nodeOperator"}]}]`)

	tests := []struct {
		name    string
		subject string
		want    []string
	}{
		{"a member of the trust domain the expression accepts", "spiffe://nvidia.com/ns/nke-system/sa/node-lifecycle", []string{"nodeOperator"}},
		{"a member the expression rejects", "spiffe://nvidia.com/ns/nke-system/sa/nke-controller", []string{}},
		{
			// A suffix match must not count as membership. The path alone would
			// satisfy the expression, which is why the domain is checked in Go
			// first and never left to CEL.
			"a suffix-matching but distinct trust domain",
			"spiffe://nvidia.com.attacker.test/ns/nke-system/sa/node-lifecycle",
			[]string{},
		},
		{"not a SPIFFE ID at all", "system:serviceaccount:nke-system:node-lifecycle", []string{}},
		{"empty subject", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []types.EntityUID
			assert.NotPanics(t, func() { got = m.parentsFor(Query{Issuer: "spire", Principal: tt.subject}, nil) })
			assert.Equal(t, tt.want, roleIDs(got))
		})
	}
}

// Entries for one issuer may mix mechanisms, and everything that matches
// contributes. The one issuer's entries never apply to another's caller.
func TestParentsForUnionsEveryMatchingEntry(t *testing.T) {
	t.Parallel()

	m := mustCompileMappings(t, `[
 {"issuer":"k8s","claim":{"name":"groups","value":"ops"},"grants":[{"type":"NKE::Role","id":"operator"}]},
 {"issuer":"k8s","claim":{"name":"groups","value":"readers"},"grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"k8s","serviceAccount":{"namespace":"ci"},"grants":[{"type":"NKE::Role","id":"nodeOperator"}]},
 {"issuer":"other","claim":{"name":"groups","value":"ops"},"grants":[{"type":"NKE::Role","id":"nodeOperator"}]}
]`)

	q := Query{
		Issuer:         "k8s",
		Principal:      "system:serviceaccount:ci:builder",
		Claims:         map[string]any{"groups": []any{"ops", "readers"}},
		ServiceAccount: &authn.ServiceAccount{Namespace: "ci", Name: "builder"},
	}
	assert.ElementsMatch(t, []string{"operator", "reader", "nodeOperator"}, roleIDs(m.parentsFor(q, nil)))

	q.Issuer = "unmapped"
	assert.Empty(t, m.parentsFor(q, nil), "an issuer with no entries grants nothing")
}

func TestParentsForOnNilMappingsGrantsNothing(t *testing.T) {
	t.Parallel()
	var m *roleMappings
	assert.Nil(t, m.parentsFor(Query{Issuer: "k8s", Claims: map[string]any{"groups": "ops"}}, nil))
	assert.Zero(t, m.len())
	assert.NoError(t, m.checkIssuers(nil))
}

// ---------------------------------------------------------------------------
// CEL.
// ---------------------------------------------------------------------------

// The load-time type check guarantees bool statically; the runtime check is
// defense in depth, exercised directly because no accepted expression can
// reach it.
func TestEvalSPIFFEExpressionRejectsNonBoolResults(t *testing.T) {
	t.Parallel()

	env, err := spiffeCELEnv()
	require.NoError(t, err)
	ast, issues := env.Compile(`path.size()`)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)

	matched, _, err := evalSPIFFEExpression(program, "/ns/nke-system")
	require.NoError(t, err)
	assert.False(t, matched)
}

// An expression that errors at evaluation does not match, and the error is
// logged at debug level with the entry's index in the file and the issuer.
func TestSPIFFEEvaluationErrorIsLoggedAtDebug(t *testing.T) {
	t.Parallel()

	m := mustCompileMappings(t, `[
 {"issuer":"k8s","claim":{"name":"groups","value":"ops"},"grants":[{"type":"NKE::Role","id":"operator"}]},
 {"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"path.split('/')[4] == 'node'"},
  "grants":[{"type":"NKE::Role","id":"nodeOperator"}]}
]`)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// path.split('/')[4] is out of range for a two-segment path.
	var got []types.EntityUID
	assert.NotPanics(t, func() {
		got = m.parentsFor(Query{Issuer: "spire", Principal: "spiffe://nvidia.com/ns/nke-system"}, logger)
	})
	assert.Empty(t, got)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "exactly one JSON log line: %s", buf.String())
	assert.Equal(t, "DEBUG", line["level"])
	assert.InDelta(t, 1, line["entry"], 0, "the index in the file, not within the issuer's entries")
	assert.Equal(t, "spire", line["issuer"])
	assert.NotEmpty(t, line["error"])
}

// costlySPIFFEExpression never matches and spends most of spiffeCELCostLimit
// getting there: 800 iterations of a comprehension, then a path comparison
// that fails.
const costlySPIFFEExpression = `[0,1,2,3,4,5,6,7,8,9].all(a, [0,1,2,3,4,5,6,7,8,9].all(b, ` +
	`[0,1,2,3,4,5,6,7].all(c, a+b+c >= 0))) && path == '/never'`

// spiffeBudgetBundle is n costly spire entries, then one cheap entry that
// grants nodeOperator to spiffe://nvidia.com/ns/nke-system, then a claim entry
// for the same issuer that grants reader.
func spiffeBudgetBundle(n int) string {
	var b strings.Builder
	b.WriteString("[")
	for range n {
		b.WriteString(`{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"` +
			costlySPIFFEExpression + `"},"grants":[{"type":"NKE::Role","id":"operator"}]},`)
	}
	b.WriteString(`{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"path == '/ns/nke-system'"},` +
		`"grants":[{"type":"NKE::Role","id":"nodeOperator"}]},`)
	b.WriteString(`{"issuer":"spire","claim":{"name":"groups","value":"readers"},` +
		`"grants":[{"type":"NKE::Role","id":"reader"}]}]`)
	return b.String()
}

func budgetQuery() Query {
	return Query{
		Issuer:    "spire",
		Principal: "spiffe://nvidia.com/ns/nke-system",
		Claims:    map[string]any{"groups": "readers"},
	}
}

// The costly expression really does sit near the per-expression limit, so
// the tests below spend the budget by evaluation and not by construction.
func TestCostlySPIFFEExpressionCostsNearTheLimit(t *testing.T) {
	t.Parallel()

	env, err := spiffeCELEnv()
	require.NoError(t, err)
	program, err := compileSPIFFEExpression(env, costlySPIFFEExpression)
	require.NoError(t, err)
	matched, cost, err := evalSPIFFEExpression(program, "/ns/nke-system")
	require.NoError(t, err)
	assert.False(t, matched)
	assert.Greater(t, cost, uint64(spiffeCELCostLimit/2))
	assert.LessOrEqual(t, cost, uint64(spiffeCELCostLimit))
}

// Enough costly entries to spend spiffeCELRequestBudget: the cheap spiffe
// entry after them would grant, but evaluation has stopped, so it does not
// match. The claim entry after it runs no CEL and still grants. One warning
// names the issuer and the entry where evaluation stopped.
func TestSPIFFERequestBudgetStopsEvaluation(t *testing.T) {
	t.Parallel()

	n := spiffeCELRequestBudget/(spiffeCELCostLimit/2) + 2
	m := mustCompileMappings(t, spiffeBudgetBundle(n))

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	got := m.parentsFor(budgetQuery(), logger)
	assert.Equal(t, []string{"reader"}, roleIDs(got), "the spiffe entry past the budget must not grant")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "exactly one log line per request: %s", buf.String())
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &line))
	assert.Equal(t, "WARN", line["level"])
	assert.Equal(t, "spire", line["issuer"])
	entry, ok := line["entry"].(float64)
	require.True(t, ok)
	assert.Greater(t, int(entry), spiffeCELRequestBudget/spiffeCELCostLimit-1,
		"no fewer than budget/limit evaluations fit before the budget trips")
	assert.Less(t, int(entry), n+1, "evaluation stopped before the granting entry")
	assert.GreaterOrEqual(t, line["spent"], float64(spiffeCELRequestBudget))

	// The budget is per request: a second request starts from zero and
	// stops in the same place.
	buf.Reset()
	assert.Equal(t, []string{"reader"}, roleIDs(m.parentsFor(budgetQuery(), logger)))
	assert.Contains(t, buf.String(), `"level":"WARN"`)
}

// A bundle whose spiffe entries fit in the budget, including a few costly
// ones, evaluates every entry and logs nothing.
func TestSPIFFERequestBudgetLeavesANormalBundleAlone(t *testing.T) {
	t.Parallel()

	m := mustCompileMappings(t, spiffeBudgetBundle(3))

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	got := m.parentsFor(budgetQuery(), logger)
	assert.ElementsMatch(t, []string{"nodeOperator", "reader"}, roleIDs(got))
	assert.Empty(t, buf.String())
}

// split() is not in CEL's default environment. spiffeCELEnv adds
// ext.Strings() for it; a bare environment refuses it.
func TestSPIFFESplitRequiresExtStrings(t *testing.T) {
	t.Parallel()

	bare, err := cel.NewEnv(cel.Variable(spiffeCELPathVar, cel.StringType))
	require.NoError(t, err)
	_, issues := bare.Compile(`path.split("/")[0] == ""`)
	require.Error(t, issues.Err())
	assert.Contains(t, issues.Err().Error(), "split")

	env, err := spiffeCELEnv()
	require.NoError(t, err)
	_, err = compileSPIFFEExpression(env, `path.split("/")[0] == ""`)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Load-time refusals.
// ---------------------------------------------------------------------------

func TestCompileRoleMappingsRefuses(t *testing.T) {
	t.Parallel()

	const reader = `"grants":[{"type":"NKE::Role","id":"reader"}]`
	tests := []struct {
		name string
		raw  string
		want string
	}{
		// Strict decoding. Each of these would load under encoding/json.
		{"an unknown key", `[{"issuer":"k8s","serviceAcount":{"namespace":"ci"},` + reader + `}]`, "serviceAcount"},
		{
			// encoding/json would match NAME to name case-insensitively and let
			// the later null win, widening a one-account grant to the whole
			// namespace.
			"a case-mismatched key",
			`[{"issuer":"k8s","serviceAccount":{"namespace":"ci","name":"builder","NAME":null},` + reader + `}]`,
			"NAME",
		},
		{
			"a duplicate key",
			`[{"issuer":"k8s","serviceAccount":{"namespace":"ci","name":"builder","name":"other"},` + reader + `}]`,
			"duplicate",
		},
		{"a JSON object rather than an array", `{"issuer":"k8s"}`, "JSON array"},
		{"null rather than an array", `null`, "JSON array"},
		{"malformed JSON", `[{"issuer":`, roleMappingsFile},

		// Entry shape.
		{"an empty issuer", `[{"issuer":"","claim":{"name":"groups","value":"x"},` + reader + `}]`, "issuer is required"},
		{"a padded issuer", `[{"issuer":" k8s","claim":{"name":"groups","value":"x"},` + reader + `}]`, "whitespace"},
		{"no mechanism", `[{"issuer":"k8s",` + reader + `}]`, "exactly one of"},
		{
			"two mechanisms in one entry",
			`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},"serviceAccount":{"namespace":"ci"},` + reader + `}]`,
			"exactly one of",
		},

		// claim.
		{"an empty claim name", `[{"issuer":"k8s","claim":{"name":"","value":"x"},` + reader + `}]`, "name is required"},
		{"an empty claim value", `[{"issuer":"k8s","claim":{"name":"groups","value":""},` + reader + `}]`, "empty value can never grant"},
		{"a padded claim name", `[{"issuer":"k8s","claim":{"name":"groups ","value":"x"},` + reader + `}]`, "has leading or trailing whitespace; it is used verbatim"},
		{"a claim entry naming aud", `[{"issuer":"k8s","claim":{"name":"aud","value":"https://nke"},` + reader + `}]`, "requesting an extra audience"},

		// serviceAccount.
		{"a service account with no namespace", `[{"issuer":"k8s","serviceAccount":{"namespace":""},` + reader + `}]`, "namespace is required"},
		{
			// A decoder maps null to the same nil as an absent name, which
			// would widen a one-account grant to the whole namespace.
			"a service account with a null name",
			`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},` + reader + `},` +
				`{"issuer":"k8s","serviceAccount":{"namespace":"ci","name":null},` + reader + `}]`,
			"[1]: serviceAccount: name is present but null",
		},
		{"a service account with an empty name", `[{"issuer":"k8s","serviceAccount":{"namespace":"ci","name":""},` + reader + `}]`, "present but empty"},
		{"an invalid namespace", `[{"issuer":"k8s","serviceAccount":{"namespace":"Not_A_Namespace"},` + reader + `}]`, "not a valid namespace name"},

		// spiffe.
		{
			// TrustDomainFromString alone would accept this and keep only
			// nvidia.com, silently.
			"a trust domain given as a full SPIFFE ID",
			`[{"issuer":"spire","spiffe":{"trustDomain":"spiffe://nvidia.com/ns/nke-system","expression":"true"},` + reader + `}]`,
			"bare trust domain",
		},
		{"an empty trust domain", `[{"issuer":"spire","spiffe":{"trustDomain":"","expression":"true"},` + reader + `}]`, "trustDomain is required"},
		{"an uppercase trust domain", `[{"issuer":"spire","spiffe":{"trustDomain":"NVIDIA.com","expression":"true"},` + reader + `}]`, "not a valid SPIFFE trust domain"},
		{"an empty expression", `[{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":""},` + reader + `}]`, "expression is required"},
		{"an expression with a syntax error", `[{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"path ==="},` + reader + `}]`, "expression"},
		{"an expression naming an undeclared variable", `[{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"subject == 'x'"},` + reader + `}]`, "undeclared reference"},
		{"an expression returning a string", `[{"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"path"},` + reader + `}]`, "must evaluate to bool"},

		// grants.
		{"no grants", `[{"issuer":"k8s","claim":{"name":"groups","value":"x"},"grants":[]}]`, "grants is required"},
		{"grants absent", `[{"issuer":"k8s","claim":{"name":"groups","value":"x"}}]`, "grants is required"},
		{
			// roles.json names the roles a role nests in under "parents"; a mapping names what it grants under "grants".
			"a parents key in place of grants",
			`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},"parents":[{"type":"NKE::Role","id":"reader"}]}]`,
			`unknown field "[0].parents"; the field is "grants"`,
		},
		{
			"a grant that is not a role",
			`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},"grants":[{"type":"NKE::Principal","id":"reader"}]}]`,
			"grants roles only",
		},
		{
			"a grant roles.json does not declare",
			`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},"grants":[{"type":"NKE::Role","id":"raeder"}]}]`,
			`"raeder"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, errs := compileRoleMappings([]byte(tt.raw), mappingRoles())
			require.NotEmpty(t, errs, "this mapping file must not load")
			joined := make([]string, 0, len(errs))
			for _, err := range errs {
				joined = append(joined, err.Error())
			}
			assert.Contains(t, strings.Join(joined, "\n"), tt.want)
		})
	}
}

// Every bad entry is reported, each naming its index, so an operator fixing
// the file by hand sees the whole list.
func TestCompileRoleMappingsReportsEveryBadEntry(t *testing.T) {
	t.Parallel()

	_, errs := compileRoleMappings([]byte(`[
 {"issuer":"k8s","claim":{"name":"aud","value":"x"},"grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"k8s","claim":{"name":"groups","value":"x"},"grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"k8s","claim":{"name":"groups","value":"y"},"grants":[{"type":"NKE::Role","id":"ghost"}]}
]`), mappingRoles())
	require.Len(t, errs, 2)
	assert.Contains(t, errs[0].Error(), roleMappingsFile+"[0]")
	assert.Contains(t, errs[1].Error(), roleMappingsFile+"[2]")
}

// Only an unknown parents key is pointed at grants; any other unknown key is
// reported as the decoder wrote it.
func TestUnknownParentsKeyNamesGrants(t *testing.T) {
	t.Parallel()

	_, errs := compileRoleMappings([]byte(`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},`+
		`"parents":[{"type":"NKE::Role","id":"reader"}]}]`), mappingRoles())
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), `unknown field "[0].parents"; the field is "grants"`)

	_, errs = compileRoleMappings([]byte(`[{"issuer":"k8s","claim":{"name":"groups","value":"x"},`+
		`"grant":[{"type":"NKE::Role","id":"reader"}]}]`), mappingRoles())
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), `unknown field "[0].grant"`)
	assert.NotContains(t, errs[0].Error(), "the field is")
}

func TestCompileRoleMappingsAcceptsAnEmptyList(t *testing.T) {
	t.Parallel()
	m := mustCompileMappings(t, "[]\n")
	assert.Zero(t, m.len())
}

// ---------------------------------------------------------------------------
// The issuer check, at load and on reload (reload_test.go has the reload half
// through the real reloadable).
// ---------------------------------------------------------------------------

func TestAnUnknownIssuerFailsTheLoad(t *testing.T) {
	t.Parallel()

	mappings := `[{"issuer":"entra","claim":{"name":"groups","value":"eng"},"grants":[{"type":"NKE::Role","id":"reader"}]}]`

	t.Run("an issuer the authentication config does not name", func(t *testing.T) {
		t.Parallel()
		_, err := NewLocal(shippedBundleWithMappings(t, mappings), []string{"local-kubernetes", "spire"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"entra"`)
		assert.Contains(t, err.Error(), "local-kubernetes", "the message lists what is configured")
		assert.Contains(t, err.Error(), "fix the name or add the issuer to AuthenticationConfiguration")
	})

	t.Run("no issuers at all", func(t *testing.T) {
		t.Parallel()
		_, err := NewLocal(shippedBundleWithMappings(t, mappings), nil)
		require.Error(t, err, "with no issuer configured, any mapping entry fails the load")
		assert.Contains(t, err.Error(), "authentication is disabled, so no issuer is configured")
	})

	t.Run("a configured issuer loads", func(t *testing.T) {
		t.Parallel()
		_, err := NewLocal(shippedBundleWithMappings(t, mappings), []string{"entra"})
		require.NoError(t, err)
	})
}

func TestAMissingRoleMappingsFileFailsTheLoad(t *testing.T) {
	t.Parallel()

	dir := writeBundle(t, aPolicy, shippedActions(t), oneReader)
	require.NoError(t, os.Remove(filepath.Join(dir, roleMappingsFile)))

	_, err := NewLocal(dir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading "+roleMappingsFile)
	assert.Contains(t, err.Error(), "apiAuthz.roleMappingsConfigMap",
		"the error must name the chart value that supplies the file")
	assert.Contains(t, err.Error(), "nke-authz-role-mappings")

	errs := ValidateBundle(dir)
	require.Len(t, errs, 1, "%v", errs)
	assert.NotContains(t, errs[0].Error(), "has no entries",
		"a file that did not read has no known entry count")
}

// Setup is where cmd/worker hands over the authentication issuers; a mapping
// keyed on one of them loads only when it is passed through.
func TestSetupChecksMappingIssuersAgainstOptions(t *testing.T) {
	t.Parallel()

	dir := shippedBundleWithMappings(t,
		`[{"issuer":"k8s","serviceAccount":{"namespace":"ci"},"grants":[{"type":"NKE::Role","id":"reader"}]}]`)

	_, err := Setup(t.Context(), Options{BundleDir: dir}, testLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"k8s"`)

	provider, err := Setup(t.Context(), Options{BundleDir: dir, IssuerNames: []string{"k8s"}}, testLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)
}
