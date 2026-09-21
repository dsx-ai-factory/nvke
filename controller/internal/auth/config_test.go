// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
)

// configHeader is the apiVersion/kind preamble every valid file carries.
const configHeader = `apiVersion: auth.nke.nvidia.com/v1alpha1
kind: AuthenticationConfiguration
`

// exampleConfig is what a real deployment writes: one Kubernetes issuer and one
// SPIRE issuer, the second overriding the audience.
const exampleConfig = `
` + configHeader + `audiences: ["https://cwe.example.com"]
issuers:
  - name: local-kubernetes
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256, ES256]
    admit:
      serviceAccounts:
        - namespace: nke-clients

  - name: spire
    issuer: "https://spire.example.com"
    audiences: ["spiffe://example.org/nke-service/cloud-workflow-engine"]
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256, ES256]
    admit:
      spiffe:
        trustDomain: example.org
        pathPrefixes: ["/nke-service/"]
`

// The helpers below vary one thing at a time against an otherwise-valid file. A
// case needing two issuers, or a missing top-level key, writes its own YAML.

// audiencesConfig varies the deployment-wide allow-list and the issuer URL it is
// compared against, both raw YAML.
func audiencesConfig(audiences, issuer string) string {
	return `
` + configHeader + `audiences: ` + audiences + `
issuers:
  - name: k8s
    issuer: ` + issuer + `
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`
}

// issuerConfig varies the sole issuer entry; fields are its YAML mapping keys.
func issuerConfig(fields string) string {
	return `
` + configHeader + `audiences: ["https://cwe.example.com"]
issuers:
  - ` + fields + "\n"
}

// algorithmsConfig varies the signing algorithm allow-list.
func algorithmsConfig(algorithms string) string {
	return issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: ` + algorithms + `
    admit: {serviceAccounts: [{namespace: nke-clients}]}`)
}

// spiffeConfig varies the SPIFFE admission rule.
func spiffeConfig(rule string) string {
	return issuerConfig(`name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit: {spiffe: ` + rule + `}`)
}

// writeConfig writes contents to a fresh temp dir and returns the path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authentication.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestLoadExampleConfig(t *testing.T) {
	t.Parallel()

	cfg, err := auth.Load(writeConfig(t, exampleConfig))
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, []string{"https://cwe.example.com"}, cfg.Audiences)
	require.Len(t, cfg.Issuers, 2)

	k8s := cfg.Issuers[0]
	assert.Equal(t, "local-kubernetes", k8s.Name)
	assert.Equal(t, "https://kubernetes.default.svc.cluster.local", k8s.Issuer)
	assert.Nil(t, k8s.Audiences, "no override, so the deployment-wide audiences apply")
	assert.Equal(t, []string{"https://cwe.example.com"}, k8s.EffectiveAudiences(cfg.Audiences))
	assert.Equal(t, []string{"RS256", "ES256"}, k8s.Algorithms)
	require.NotNil(t, k8s.JWKS)
	assert.Equal(t, "https://kubernetes.default.svc/openid/v1/jwks", k8s.JWKS.URI)
	require.NotNil(t, k8s.Admit)
	require.Len(t, k8s.Admit.ServiceAccounts, 1)
	assert.Equal(t, "nke-clients", k8s.Admit.ServiceAccounts[0].Namespace)
	assert.True(t, k8s.Admit.ServiceAccounts[0].AllowsAnyName())

	spire := cfg.Issuers[1]
	assert.Equal(t, []string{"spiffe://example.org/nke-service/cloud-workflow-engine"}, spire.Audiences)
	assert.Equal(t, []string{"spiffe://example.org/nke-service/cloud-workflow-engine"},
		spire.EffectiveAudiences(cfg.Audiences), "the override is the only list checked for this issuer")
	require.NotNil(t, spire.JWKS)
	require.NotNil(t, spire.Admit)
	require.NotNil(t, spire.Admit.SPIFFE)
	assert.Equal(t, "example.org", spire.Admit.SPIFFE.TrustDomain)
	assert.Equal(t, []string{"/nke-service/"}, spire.Admit.SPIFFE.PathPrefixes)
}

// The rule most easily got backwards: an unset path disables authentication, a
// set-but-unreadable path is fatal.
func TestLoadPathContract(t *testing.T) {
	t.Parallel()

	t.Run("empty path returns ErrEmptyPath so the caller can disable", func(t *testing.T) {
		t.Parallel()

		_, err := auth.Load("")
		require.ErrorIs(t, err, auth.ErrEmptyPath)
	})

	t.Run("whitespace-only path returns ErrEmptyPath", func(t *testing.T) {
		t.Parallel()

		_, err := auth.Load("   ")
		require.ErrorIs(t, err, auth.ErrEmptyPath)
	})

	t.Run("missing file errors and is not ErrEmptyPath", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
		_, err := auth.Load(path)
		require.Error(t, err)
		require.NotErrorIs(t, err, auth.ErrEmptyPath,
			"a typo'd mountPath must be fatal, not silently disabled")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("unreadable file errors and is not ErrEmptyPath", func(t *testing.T) {
		t.Parallel()

		if os.Getuid() == 0 {
			t.Skip("running as root: file mode does not deny reads")
		}
		path := writeConfig(t, exampleConfig)
		require.NoError(t, os.Chmod(path, 0o000))
		_, err := auth.Load(path)
		require.Error(t, err)
		require.NotErrorIs(t, err, auth.ErrEmptyPath)
		require.ErrorIs(t, err, fs.ErrPermission)
	})

	t.Run("empty file errors", func(t *testing.T) {
		t.Parallel()

		_, err := auth.Load(writeConfig(t, ""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		t.Parallel()

		_, err := auth.Load(writeConfig(t, "audience: \"unterminated\nissuers: [\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse authentication config")
	})

	t.Run("second YAML document errors", func(t *testing.T) {
		t.Parallel()

		// A decoder would silently drop the second document.
		_, err := auth.Load(writeConfig(t, exampleConfig+"\n---\naudience: \"https://elsewhere.example.com\"\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than one YAML document")
	})
}

// Strict decoding at every nesting depth. Strictness does not reach into
// map[string]any and does not survive a custom unmarshaler, so each case asserts
// something about the shape of the types. A decoder that stops rejecting is a
// regression, not a case to weaken.
func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			// The only unknown field at the top level.
			name: "unknown top-level field",
			yaml: `
audiences: ["https://cwe.example.com"]
audiance: "typo"
issuers:
  - name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`,
			wantErr: `unknown field "audiance"`,
		},
		{
			name: "unknown field inside an issuer",
			yaml: issuerConfig(`name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwksURI: "https://elsewhere.example.com/keys"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: `unknown field "issuers[0].jwksURI"`,
		},
		{
			name: "unknown field inside jwks",
			yaml: issuerConfig(`name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://k8s.example.com/keys", oidc: {}}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: `unknown field "issuers[0].jwks.oidc"`,
		},
		{
			name: "unknown field inside admit",
			yaml: issuerConfig(`name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit:
      serviceAccounts: [{namespace: nke-clients}]
      users: [alice]`),
			wantErr: `unknown field "issuers[0].admit.users"`,
		},
		{
			name: "unknown field inside a serviceAccounts rule",
			yaml: issuerConfig(`name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit:
      serviceAccounts: [{namespace: nke-clients, serviceAccountName: runner}]`),
			wantErr: `unknown field "issuers[0].admit.serviceAccounts[0].serviceAccountName"`,
		},
		{
			name: "unknown field inside spiffe",
			yaml: issuerConfig(`name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit:
      spiffe:
        trustDomain: example.org
        pathPrefixes: ["/nke-service/"]
        paths: ["/anything/"]`),
			wantErr: `unknown field "issuers[0].admit.spiffe.paths"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := auth.Load(writeConfig(t, tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Case-sensitive key matching. Under a case-insensitive decoder a rule carrying
// both pathPrefixes and pathprefixes loads clean, the lowercase spelling wins,
// and the file reads as narrowly scoped while admitting the whole trust domain.
func TestLoadRejectsCaseVariantKeys(t *testing.T) {
	t.Parallel()

	spire := func(rule string) string {
		return issuerConfig(`name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit:
      spiffe:
        trustDomain: example.org
` + rule)
	}

	tests := []struct {
		name string
		yaml string
	}{
		{
			// Both spellings present: the wrong one wins, invisibly.
			name: "a case-variant duplicate that would widen a SPIFFE rule",
			yaml: spire(`        pathPrefixes: ["/nke-service/"]
        pathprefixes: ["/"]`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := auth.Load(writeConfig(t, tt.yaml))
			require.Error(t, err, "a case-variant key must not load")
			assert.Contains(t, err.Error(), "unknown field",
				"the error has to name the key so an operator can find the typo")
		})
	}
}

// The exactly-cased duplicate, which the YAML layer catches before the JSON layer
// sees it.
func TestLoadRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	_, err := auth.Load(writeConfig(t, `
`+configHeader+`audiences: ["https://cwe.example.com"]
audiences: ["https://cwe.other.example.com"]
issuers:
  - name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`))
	require.Error(t, err, "a duplicate key must not resolve to last-wins")
}

// Admission rules naming an identity that cannot exist. Without this a rule
// reading `name: MyRunner` loads and then matches nothing for ever, because
// Kubernetes has no such service account and never will.
func TestLoadRejectsNamesKubernetesWouldRefuse(t *testing.T) {
	t.Parallel()

	rule := func(ns, name string) string {
		return issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://k8s.example.com/keys"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: ` + ns + `, name: ` + name + `}]}`)
	}

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"uppercase service account", rule(`"nke-clients"`, `"MyRunner"`), "not a valid service account name"},
		{"uppercase namespace", rule(`"CWE-Clients"`, `"runner"`), "not a valid namespace name"},
		{"a dot is legal in a name but not a namespace", rule(`"cwe.clients"`, `"runner"`), "not a valid namespace name"},
		{"a pair Kubernetes accepts", rule(`"nke-clients"`, `"nke-client"`), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := auth.Load(writeConfig(t, tt.yaml))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Whitespace on the identity-bearing fields. Nothing trims before use, so
// `algorithms: ["RS256 "]` would load and then match no token. Values are quoted
// because YAML strips a trailing space from a plain scalar.
func TestLoadRejectsPaddedValues(t *testing.T) {
	t.Parallel()

	const base = `apiVersion: auth.nke.nvidia.com/v1alpha1
kind: AuthenticationConfiguration
audiences: [AUD]
issuers:
  - name: NAME
    issuer: ISS
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [ALG]
    admit: {serviceAccounts: [{namespace: NS, name: SA}]}
`
	unpadded := map[string]string{
		"AUD":  `"https://cwe.example.com"`,
		"NAME": `"k8s"`,
		"ISS":  `"https://k8s.example.com"`,
		"ALG":  `"RS256"`,
		"NS":   `"nke-clients"`,
		"SA":   `"runner"`,
	}

	for _, field := range []string{"AUD", "NAME", "ISS", "ALG", "NS", "SA"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			content := base
			for token, value := range unpadded {
				if token == field {
					value = strings.TrimSuffix(value, `"`) + ` "`
				}
				content = strings.ReplaceAll(content, token, value)
			}

			_, err := auth.Load(writeConfig(t, content))
			require.Error(t, err, "a padded %s must not load", field)
			assert.Contains(t, err.Error(), "whitespace",
				"the error has to name the cause; a padded value is invisible in a diff")
		})
	}
}

// The two fields that say what the document is; checked in Load.
func TestLoadTypeMeta(t *testing.T) {
	t.Parallel()

	// The body below the preamble, so each case supplies its own.
	const body = `audiences: ["https://cwe.example.com"]
issuers:
  - name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`

	tests := []struct {
		name    string
		header  string
		wantErr string
	}{
		{
			name:   "the supported pair loads",
			header: configHeader,
		},
		{
			name:    "apiVersion absent",
			header:  "kind: AuthenticationConfiguration\n",
			wantErr: "apiVersion is required",
		},
		{
			name:    "kind absent",
			header:  "apiVersion: auth.nke.nvidia.com/v1alpha1\n",
			wantErr: "kind is required",
		},
		{
			// The apiserver's own AuthenticationConfiguration shares this kind and
			// is the file most likely to be mounted here by mistake.
			name:    "the right kind from another group",
			header:  "apiVersion: apiserver.config.k8s.io/v1\nkind: AuthenticationConfiguration\n",
			wantErr: `apiVersion "apiserver.config.k8s.io/v1" is not supported`,
		},
		{
			name:    "a version of this group that does not exist yet",
			header:  "apiVersion: auth.nke.nvidia.com/v1\nkind: AuthenticationConfiguration\n",
			wantErr: `apiVersion "auth.nke.nvidia.com/v1" is not supported`,
		},
		{
			name:    "the wrong kind in the right group",
			header:  "apiVersion: auth.nke.nvidia.com/v1alpha1\nkind: AuthorizationConfiguration\n",
			wantErr: `kind "AuthorizationConfiguration" is not supported`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := auth.Load(writeConfig(t, tt.header+body))
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, auth.ConfigAPIVersion, cfg.APIVersion)
				assert.Equal(t, auth.ConfigKind, cfg.Kind)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			// Every rejection names what would be accepted.
			assert.Contains(t, err.Error(), "this build accepts",
				"the error has to say what would be accepted")
		})
	}
}

// apiVersion and kind describe a document on disk, so a Config built in Go
// neither carries them nor needs them, and NewVerifier calls Validate.
func TestValidateDoesNotRequireTypeMeta(t *testing.T) {
	t.Parallel()

	cfg := &auth.Config{
		Audiences: []string{"https://cwe.example.com"},
		Issuers: []auth.Issuer{{
			Name:       "k8s",
			Issuer:     "https://kubernetes.default.svc.cluster.local",
			JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{"RS256"},
			Admit: &auth.Admit{
				ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}},
			},
		}},
	}

	require.NoError(t, cfg.Validate())
}

// An empty wantErr means the configuration must load.
type validationCase struct {
	name    string
	yaml    string
	wantErr string
}

func runValidationCases(t *testing.T, tests []validationCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := auth.Load(writeConfig(t, tt.yaml))
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.NotNil(t, cfg)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Nil(t, cfg)
		})
	}
}

// The audience rules, including the values that would make the check a
// cluster-wide no-op. These carry the invariant NewVerifier depends on:
// golang-jwt checks "aud" only when given an audience to expect, so an empty
// effective list is a check that does not happen, and both a missing
// deployment-wide list and `audiences: []` produce one.
func TestLoadValidationAudience(t *testing.T) {
	t.Parallel()

	const k8sIssuerURL = `"https://kubernetes.default.svc.cluster.local"`

	runValidationCases(t, []validationCase{
		{
			// No audiences key at all, so it cannot come from audiencesConfig.
			name: "audiences absent",
			yaml: "\n" + configHeader + `issuers:
  - name: k8s
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`,
			wantErr: "audiences is required",
		},
		{
			// Reaches golang-jwt as an empty expected-audience list, which skips the
			// check rather than failing it.
			name:    "audiences is an empty list",
			yaml:    audiencesConfig(`[]`, k8sIssuerURL),
			wantErr: "audiences is required",
		},
		{
			// Whitespace rather than "": the check is TrimSpace, not ==.
			name:    "an audience entry is whitespace only",
			yaml:    audiencesConfig(`["   "]`, k8sIssuerURL),
			wantErr: "audiences[0] must not be empty",
		},
		{
			name:    "the second audience entry is what is wrong",
			yaml:    audiencesConfig(`["https://cwe.example.com", ""]`, k8sIssuerURL),
			wantErr: "audiences[1] must not be empty",
		},
		{
			name: "several audiences are accepted",
			yaml: audiencesConfig(`["https://cwe.example.com", "https://cwe.example.net"]`, k8sIssuerURL),
		},
		{
			name:    "audience equals a configured issuer",
			yaml:    audiencesConfig(`[`+k8sIssuerURL+`]`, k8sIssuerURL),
			wantErr: "equals a configured issuer",
		},
		{
			// Rejected even though it is not the first entry.
			name:    "a later audience equals a configured issuer",
			yaml:    audiencesConfig(`["https://cwe.example.com", `+k8sIssuerURL+`]`, k8sIssuerURL),
			wantErr: "audiences[1]",
		},

		// --- wildcards (MK-WID-8) ---
		{
			name:    "audience is a bare wildcard",
			yaml:    audiencesConfig(`["*"]`, k8sIssuerURL),
			wantErr: "contains a wildcard",
		},
		{
			name: "audience equals an issuer declared later in the file",
			yaml: "\n" + configHeader + `audiences: ["https://spire.example.com"]
issuers:
  - name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
  - name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit: {spiffe: {trustDomain: example.org, pathPrefixes: ["/nke-service/"]}}
`,
			wantErr: "equals a configured issuer",
		},
		{
			name:    "audience equals a configured issuer modulo a trailing slash",
			yaml:    audiencesConfig(`["https://k8s.example.com/"]`, `"https://k8s.example.com"`),
			wantErr: "equals a configured issuer",
		},
		{
			// Refused on purpose. Runtime matches "iss" and "aud" byte-for-byte, so
			// this pair is not literally a no-op -- but the guardrail is inferring
			// --api-audiences, which it cannot read, and over-approximating is the
			// safe direction. See foldForClashCheck.
			name:    "audience equals a configured issuer modulo path case",
			yaml:    audiencesConfig(`["https://k8s.example.com/ID"]`, `"https://k8s.example.com/id"`),
			wantErr: "equals a configured issuer",
		},
		{
			name:    "audience is the bare kubernetes.default.svc form",
			yaml:    audiencesConfig(`["https://kubernetes.default.svc"]`, `"https://k8s.example.com"`),
			wantErr: "kubernetes.default.svc form",
		},
		{
			name:    "audience is the kubernetes.default.svc form with a port and trailing slash",
			yaml:    audiencesConfig(`["https://kubernetes.default.svc.cluster.local:443/"]`, `"https://k8s.example.com"`),
			wantErr: "kubernetes.default.svc form",
		},
		{
			name:    "audience is the kubernetes.default.svc form with no scheme",
			yaml:    audiencesConfig(`["kubernetes.default.svc.cluster.local"]`, `"https://k8s.example.com"`),
			wantErr: "kubernetes.default.svc form",
		},
		{
			name: "audience merely containing the substring is fine",
			yaml: audiencesConfig(`["https://cwe.kubernetes.default.svc.cluster.local"]`, `"https://k8s.example.com"`),
		},

		// --- audience, per-issuer override ---
		{
			// If `audiences: []` stopped decoding to a non-nil empty slice, this
			// would start passing as an inherit.
			name: "per-issuer audiences present but an empty list",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    audiences: []
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "audiences is present but empty",
		},
		{
			// The whole guardrail set runs on the override too: one row is enough to
			// prove validateAudiences is wired at the per-issuer call site, and the
			// branches themselves are covered by the deployment-wide rows above.
			name: "per-issuer audience equals a configured issuer",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    audiences: ["https://k8s.example.com"]
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "equals a configured issuer",
		},
		{
			name: "per-issuer audience override accepted",
			yaml: issuerConfig(`name: teleport
    issuer: "https://teleport.example.com"
    audiences: ["https://cwe.idp.example.com", "https://cwe.idp.example.net"]
    jwks: {uri: "https://teleport.example.com/.well-known/jwks.json"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
		},
	})
}

// The issuer list and the discriminated keys block.
func TestLoadValidationIssuersAndKeys(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:    "issuers absent",
			yaml:    configHeader + "audiences: [\"https://cwe.example.com\"]\n",
			wantErr: "issuers is required",
		},
		{
			name: "issuers empty",
			yaml: "\n" + configHeader + `audiences: ["https://cwe.example.com"]
issuers: []
`,
			wantErr: "issuers is required",
		},
		{
			name: "issuer name absent",
			yaml: issuerConfig(`issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "name is required",
		},
		{
			name: "issuer url absent",
			yaml: issuerConfig(`name: k8s
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "issuer is required",
		},
		{
			name: "duplicate issuer name",
			yaml: "\n" + configHeader + `audiences: ["https://cwe.example.com"]
issuers:
  - name: k8s
    issuer: "https://k8s-a.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
  - name: k8s
    issuer: "https://k8s-b.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`,
			wantErr: "duplicate name",
		},
		{
			name: "duplicate issuer url",
			yaml: "\n" + configHeader + `audiences: ["https://cwe.example.com"]
issuers:
  - name: k8s-a
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
  - name: k8s-b
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
`,
			wantErr: "duplicate issuer",
		},

		// --- keys ---
		{
			name: "jwks absent",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "jwks is required",
		},
		{
			name: "jwks without a uri",
			yaml: issuerConfig(`name: idp
    issuer: "https://idp.example.com"
    jwks: {}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "uri is required",
		},
		{
			name: "jwks over plain http",
			yaml: issuerConfig(`name: idp
    issuer: "https://idp.example.com"
    jwks: {uri: "http://idp.example.com/keys"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "must use https",
		},
		{
			name: "jwks with all options",
			yaml: issuerConfig(`name: idp
    issuer: "https://idp.example.com"
    jwks:
      uri: "https://idp.example.com/keys"
      caBundlePath: /etc/cwe/idp-ca.crt
      bearerTokenPath: /var/run/secrets/idp/token
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
		},
	})
}

// The signing-algorithm allow-list.
func TestLoadValidationAlgorithms(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name: "algorithms absent",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    admit: {serviceAccounts: [{namespace: nke-clients}]}`),
			wantErr: "algorithms: is required",
		},
		{
			name:    "algorithms empty",
			yaml:    algorithmsConfig(`[]`),
			wantErr: "algorithms: is required",
		},
		{
			name:    "algorithm none",
			yaml:    algorithmsConfig(`[none]`),
			wantErr: "unsigned token",
		},
		{
			name:    "algorithm nOnE mixed case",
			yaml:    algorithmsConfig(`[nOnE]`),
			wantErr: "unsigned token",
		},
		{
			name:    "algorithm HS256",
			yaml:    algorithmsConfig(`[HS256]`),
			wantErr: "HMAC is symmetric",
		},
		{
			name:    "algorithm hs512 lower case",
			yaml:    algorithmsConfig(`[hs512]`),
			wantErr: "HMAC is symmetric",
		},
		{
			name:    "algorithm with non-canonical casing",
			yaml:    algorithmsConfig(`[rs256]`),
			wantErr: "not a permitted asymmetric algorithm",
		},
		{
			name:    "unrecognized algorithm",
			yaml:    algorithmsConfig(`[RSA-OAEP]`),
			wantErr: "not a permitted asymmetric algorithm",
		},
		{
			name:    "empty algorithm entry",
			yaml:    algorithmsConfig(`[RS256, ""]`),
			wantErr: "[1] must not be empty",
		},
		{name: "RSA PKCS1 algorithms", yaml: algorithmsConfig(`[RS256, RS384, RS512]`)},
		{name: "RSA PSS algorithms", yaml: algorithmsConfig(`[PS256, PS384, PS512]`)},
		{name: "ECDSA algorithms", yaml: algorithmsConfig(`[ES256, ES384, ES512]`)},
		{name: "EdDSA", yaml: algorithmsConfig(`[EdDSA]`)},
	})
}

// The admission rules. An absent or empty admit block is a startup failure.
func TestLoadValidationAdmit(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name: "admit absent",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]`),
			wantErr: "admit is required",
		},
		{
			name: "admit empty block",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {}`),
			wantErr: "must contain at least one rule",
		},
		{
			name: "admit with an empty serviceAccounts list",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: []}`),
			wantErr: "must contain at least one rule",
		},
		{
			name: "serviceAccounts rule without a namespace",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{name: runner}]}`),
			wantErr: "namespace is required",
		},
		{
			name: "serviceAccounts rule with an empty name",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients, name: ""}]}`),
			wantErr: "name is present but empty",
		},
		{
			name: "serviceAccounts rule naming a specific account",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients, name: runner}]}`),
		},
		{
			// Either match admits, so an issuer carrying both restricts nothing.
			name: "admit with both serviceAccounts and spiffe is a startup failure",
			yaml: issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit:
      serviceAccounts: [{namespace: nke-clients}]
      spiffe: {trustDomain: example.org, pathPrefixes: ["/nke-service/"]}`),
			wantErr: "must not set more than one of serviceAccounts, spiffe, and groups",
		},
		{
			name: "an empty serviceAccounts list beside a spiffe rule is not \"both\"",
			yaml: issuerConfig(`name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit:
      serviceAccounts: []
      spiffe: {trustDomain: example.org, pathPrefixes: ["/nke-service/"]}`),
		},
		{
			name: "admit.groups",
			yaml: issuerConfig(`name: keycloak
    issuer: "http://keycloak.nke.test:8180/realms/nke"
    jwks: {uri: "https://keycloak.nke.test:8180/realms/nke/protocol/openid-connect/certs"}
    algorithms: [RS256]
    admit: {groups: [nke-operators]}`),
		},
		{
			name: "admit.groups with an empty name",
			yaml: issuerConfig(`name: keycloak
    issuer: "http://keycloak.nke.test:8180/realms/nke"
    jwks: {uri: "https://keycloak.nke.test:8180/realms/nke/protocol/openid-connect/certs"}
    algorithms: [RS256]
    admit: {groups: [""]}`),
			wantErr: "groups[0] is required and must not be empty",
		},
		{
			name: "admit with both groups and serviceAccounts is a startup failure",
			yaml: issuerConfig(`name: keycloak
    issuer: "http://keycloak.nke.test:8180/realms/nke"
    jwks: {uri: "https://keycloak.nke.test:8180/realms/nke/protocol/openid-connect/certs"}
    algorithms: [RS256]
    admit:
      groups: [nke-operators]
      serviceAccounts: [{namespace: nke-clients}]`),
			wantErr: "must not set more than one of serviceAccounts, spiffe, and groups",
		},

		// --- admit.spiffe ---
		{
			name:    "spiffe without a trust domain",
			yaml:    spiffeConfig(`{pathPrefixes: ["/nke-service/"]}`),
			wantErr: "trustDomain is required",
		},
		{
			name:    "spiffe with an empty trust domain",
			yaml:    spiffeConfig(`{trustDomain: "", pathPrefixes: ["/nke-service/"]}`),
			wantErr: "trustDomain is required",
		},
		{
			name:    "spiffe without path prefixes",
			yaml:    spiffeConfig(`{trustDomain: example.org}`),
			wantErr: "pathPrefixes is required",
		},
		{
			name:    "spiffe with an empty path prefix list",
			yaml:    spiffeConfig(`{trustDomain: example.org, pathPrefixes: []}`),
			wantErr: "pathPrefixes is required",
		},
		{
			name:    "spiffe with an empty path prefix entry",
			yaml:    spiffeConfig(`{trustDomain: example.org, pathPrefixes: ["/nke-service/", ""]}`),
			wantErr: "pathPrefixes[1] must not be empty",
		},
		{
			name:    "spiffe path prefix without a leading slash",
			yaml:    spiffeConfig(`{trustDomain: example.org, pathPrefixes: ["nke-service/"]}`),
			wantErr: "must start with",
		},
		{
			name:    "spiffe trust domain given as a full SPIFFE ID",
			yaml:    spiffeConfig(`{trustDomain: "spiffe://example.org/nke-service", pathPrefixes: ["/nke-service/"]}`),
			wantErr: "bare trust domain",
		},

		// --- admit.spiffe.trustDomain must parse, not merely be non-empty ---
		// This passes a "non-empty and no slash" check; the rest of the invalid
		// spellings are go-spiffe's own rules, not ours.
		{
			name:    "spiffe trust domain in uppercase",
			yaml:    spiffeConfig(`{trustDomain: "EXAMPLE.ORG", pathPrefixes: ["/nke-service/"]}`),
			wantErr: "is not a valid SPIFFE trust domain",
		},

		// --- admit.spiffe.pathPrefixes: the all-slash prefixes ---
		// pathHasPrefix reads an empty result as "admit the whole trust domain".
		// Exactly "/" says that deliberately; "//" says it by accident.
		{
			name:    "a path prefix of two slashes is refused",
			yaml:    spiffeConfig(`{trustDomain: example.org, pathPrefixes: ["//"]}`),
			wantErr: "nothing but slashes",
		},
		{
			name:    "an all-slash prefix hiding behind a real one is refused",
			yaml:    spiffeConfig(`{trustDomain: example.org, pathPrefixes: ["/nke-service/", "//"]}`),
			wantErr: "pathPrefixes[1]",
		},
		{
			// The deliberate spelling still works.
			name: "a path prefix of exactly one slash is still accepted",
			yaml: spiffeConfig(`{trustDomain: example.org, pathPrefixes: ["/"]}`),
		},
	})
}

func TestIssuerEffectiveAudiences(t *testing.T) {
	t.Parallel()

	override := []string{"spiffe://example.org/nke-service/cloud-workflow-engine"}
	deployment := []string{"https://cwe.example.com", "https://cwe.example.net"}

	tests := []struct {
		name       string
		issuer     auth.Issuer
		deployment []string
		want       []string
	}{
		{
			name:       "no override falls back to the deployment-wide list",
			issuer:     auth.Issuer{},
			deployment: deployment,
			want:       deployment,
		},
		{
			// Superseding is not merging: one override value does not inherit the
			// rest of the deployment-wide list.
			name:       "an override supersedes the deployment-wide list entirely",
			issuer:     auth.Issuer{Audiences: override},
			deployment: deployment,
			want:       override,
		},
		{
			name:       "an override of several values supersedes as a whole",
			issuer:     auth.Issuer{Audiences: []string{"a", "b"}},
			deployment: deployment,
			want:       []string{"a", "b"},
		},
		{
			// Surfacing an empty override as empty is what lets Validate refuse it;
			// falling back would make `audiences: []` a silent inherit.
			name:       "an explicitly empty override does not fall back",
			issuer:     auth.Issuer{Audiences: []string{}},
			deployment: deployment,
			want:       []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.issuer.EffectiveAudiences(tt.deployment))
		})
	}
}

// Validate must be safe to call on a value the caller still owns, and twice.
func TestValidateDoesNotMutate(t *testing.T) {
	t.Parallel()

	cfg, err := auth.Load(writeConfig(t, exampleConfig))
	require.NoError(t, err)

	before := *cfg.Issuers[1].Admit.SPIFFE
	require.NoError(t, cfg.Validate())
	assert.Equal(t, before, *cfg.Issuers[1].Admit.SPIFFE, "Validate rewrote the operator's rule")
}

func TestValidateOnNilConfig(t *testing.T) {
	t.Parallel()

	var cfg *auth.Config
	require.Error(t, cfg.Validate())
}
