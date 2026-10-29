// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// The apiAuth section is the only source of the API authentication issuer
// registry, so Load's strict decoding is what stands between an operator's typo
// and an admission rule that silently means something else. These cases cover
// the authn.Config types reached through apiAuth.issuers.

// apiAuthOn wraps raw YAML for the issuers list in an enabled apiAuth section
// with one deployment-wide audience. issuers is indented to sit under
// apiAuth.issuers, each entry starting with "    - ".
func apiAuthOn(issuers string) string {
	return minimalConfigWithoutAPIAuth + `apiAuth:
  enabled: true
  audiences: ["https://cwe.example.com"]
  issuers:
` + issuers
}

// Strict decoding at every nesting depth. Strictness does not reach into
// map[string]any and does not survive a custom unmarshaler, so each case asserts
// something about the shape of the types. A decoder that stops rejecting is a
// regression, not a case to weaken.
func TestLoadAPIAuthRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			// The only unknown field at the top level of the section.
			name: "unknown top-level field",
			yaml: minimalConfigWithoutAPIAuth + `apiAuth:
  enabled: true
  audiences: ["https://cwe.example.com"]
  audiance: "typo"
  issuers:
    - name: k8s
      issuer: "https://kubernetes.default.svc.cluster.local"
      jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
      algorithms: [RS256]
      admit: {serviceAccounts: [{namespace: nke-clients}]}
`,
			wantErr: `unknown field "apiAuth.audiance"`,
		},
		{
			name: "unknown field inside an issuer",
			yaml: apiAuthOn(`    - name: k8s
      issuer: "https://kubernetes.default.svc.cluster.local"
      jwksURI: "https://elsewhere.example.com/keys"
      jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
      algorithms: [RS256]
      admit: {serviceAccounts: [{namespace: nke-clients}]}
`),
			wantErr: `unknown field "apiAuth.issuers[0].jwksURI"`,
		},
		{
			name: "unknown field inside jwks",
			yaml: apiAuthOn(`    - name: k8s
      issuer: "https://kubernetes.default.svc.cluster.local"
      jwks: {uri: "https://k8s.example.com/keys", oidc: {}}
      algorithms: [RS256]
      admit: {serviceAccounts: [{namespace: nke-clients}]}
`),
			wantErr: `unknown field "apiAuth.issuers[0].jwks.oidc"`,
		},
		{
			name: "unknown field inside admit",
			yaml: apiAuthOn(`    - name: k8s
      issuer: "https://kubernetes.default.svc.cluster.local"
      jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
      algorithms: [RS256]
      admit:
        serviceAccounts: [{namespace: nke-clients}]
        users: [alice]
`),
			wantErr: `unknown field "apiAuth.issuers[0].admit.users"`,
		},
		{
			name: "unknown field inside a serviceAccounts rule",
			yaml: apiAuthOn(`    - name: k8s
      issuer: "https://kubernetes.default.svc.cluster.local"
      jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
      algorithms: [RS256]
      admit:
        serviceAccounts: [{namespace: nke-clients, serviceAccountName: runner}]
`),
			wantErr: `unknown field "apiAuth.issuers[0].admit.serviceAccounts[0].serviceAccountName"`,
		},
		{
			name: "unknown field inside spiffe",
			yaml: apiAuthOn(`    - name: spire
      issuer: "https://spire.example.com"
      jwks: {uri: "https://spire.example.com/keys"}
      algorithms: [RS256]
      admit:
        spiffe:
          trustDomain: example.org
          pathPrefixes: ["/nke-service/"]
          paths: ["/anything/"]
`),
			wantErr: `unknown field "apiAuth.issuers[0].admit.spiffe.paths"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Case-sensitive key matching. Under a case-insensitive decoder a rule carrying
// both pathPrefixes and pathprefixes loads clean, the lowercase spelling wins,
// and the configuration reads as narrowly scoped while admitting the whole
// trust domain.
func TestLoadAPIAuthRejectsCaseVariantKeys(t *testing.T) {
	t.Parallel()

	spire := func(rule string) string {
		return apiAuthOn(`    - name: spire
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
			yaml: spire(`          pathPrefixes: ["/nke-service/"]
          pathprefixes: ["/"]
`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, tt.yaml))
			require.Error(t, err, "a case-variant key must not load")
			assert.Contains(t, err.Error(), "unknown field",
				"the error has to name the key so an operator can find the typo")
			assert.Contains(t, err.Error(), "pathprefixes")
		})
	}
}

// The exactly-cased duplicate, which the YAML layer catches before the JSON layer
// sees it.
func TestLoadAPIAuthRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	_, err := controllerconfig.Load(writeConfig(t, minimalConfigWithoutAPIAuth+`apiAuth:
  enabled: true
  audiences: ["https://cwe.example.com"]
  audiences: ["https://cwe.other.example.com"]
  issuers:
    - name: k8s
      issuer: "https://k8s.example.com"
      jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
      algorithms: [RS256]
      admit: {serviceAccounts: [{namespace: nke-clients}]}
`))
	require.Error(t, err, "a duplicate key must not resolve to last-wins")
	assert.Contains(t, err.Error(), "audiences", "the error has to name the duplicated key")
}
