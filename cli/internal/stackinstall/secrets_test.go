// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

var (
	testCredentials = Credentials{
		DatabasePassword: "secrets-db-pw",
		NicoToken:        "secrets-token",
		NodeUser:         "secrets-user",
		NodePassword:     "secrets-node-pw",
	}
	testNICo = NICoAPI{Endpoint: "https://nico.example.com", OrgID: "example-org", APIName: "forge"}
)

func withCA(c Credentials) Credentials {
	c.SpireCACert = []byte("secrets-ca-cert")
	c.SpireCAKey = []byte("secrets-ca-key")
	return c
}

// layout lists each Secret as namespace/name and its sorted keys.
func layout(secrets []*corev1.Secret) []string {
	var out []string
	for _, s := range secrets {
		keys := slices.Sorted(maps.Keys(s.Data))
		out = append(out, s.Namespace+"/"+s.Name+" {"+strings.Join(keys, ", ")+"}")
	}
	return out
}

func find(t *testing.T, secrets []*corev1.Secret, namespace, name string) *corev1.Secret {
	t.Helper()
	i := slices.IndexFunc(secrets, func(s *corev1.Secret) bool { return s.Namespace == namespace && s.Name == name })
	require.NotEqualf(t, -1, i, "no Secret %s/%s", namespace, name)
	return secrets[i]
}

func TestCredentialSecretsSite(t *testing.T) {
	base := []string{
		"postgres/nvke-database {postgres-password}",
		"temporal/nvke-database {postgres-password}",
		"nvke-controller/nvke-database {database-connection-string}",
		"nvke-controller/dgxc-tenant-credentials {password, user}",
		"capnico-system/nico-credentials {apiName, endpoint, orgID, token}",
	}
	tests := []struct {
		name  string
		creds Credentials
		want  []string
	}{
		{"without CA", testCredentials, base},
		{"with CA", withCA(testCredentials), append(slices.Clone(base), "spire-server/spire-upstream-ca {bundle.crt, tls.crt, tls.key}")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets, err := CredentialSecrets(tt.creds, testNICo)

			require.NoError(t, err)
			assert.Equal(t, tt.want, layout(secrets))
			for _, s := range secrets {
				assert.Equal(t, "v1", s.APIVersion)
				assert.Equal(t, "Secret", s.Kind)
				assert.Equal(t, corev1.SecretTypeOpaque, s.Type)
			}
			assert.Equal(t, []byte("secrets-db-pw"), find(t, secrets, "postgres", "nvke-database").Data["postgres-password"])
			tenant := find(t, secrets, "nvke-controller", "dgxc-tenant-credentials")
			assert.Equal(t, []byte("secrets-user"), tenant.Data["user"])
			assert.Equal(t, []byte("secrets-node-pw"), tenant.Data["password"])
			nico := find(t, secrets, "capnico-system", "nico-credentials")
			assert.Equal(t, map[string][]byte{
				"endpoint": []byte(testNICo.Endpoint), "orgID": []byte(testNICo.OrgID), "apiName": []byte(testNICo.APIName), "token": []byte("secrets-token"),
			}, nico.Data)
			if tt.creds.SpireCACert != nil {
				ca := find(t, secrets, "spire-server", "spire-upstream-ca")
				assert.Equal(t, ca.Data["tls.crt"], ca.Data["bundle.crt"])
				assert.Equal(t, []byte("secrets-ca-key"), ca.Data["tls.key"])
			}
		})
	}
}

func TestCredentialSecretsTemporalKey(t *testing.T) {
	secrets, err := CredentialSecrets(testCredentials, testNICo)
	require.NoError(t, err)

	temporal := find(t, secrets, "temporal", "nvke-database")

	assert.Equal(t, map[string][]byte{"postgres-password": []byte("secrets-db-pw")}, temporal.Data)
	assert.NotContains(t, temporal.Data, "password")
}

func TestCredentialSecretsUseData(t *testing.T) {
	secrets, err := CredentialSecrets(withCA(testCredentials), testNICo)
	require.NoError(t, err)

	for _, s := range secrets {
		out, err := yaml.Marshal(s)
		require.NoError(t, err)
		assert.Contains(t, string(out), "\ndata:\n", s.Name)
		assert.NotContains(t, string(out), "stringData", s.Name)
	}
}

func TestConnectionStringEscapes(t *testing.T) {
	password := "p@ss:w/o?r#d%"

	r, err := readSecretRefs()
	require.NoError(t, err)
	u, err := url.Parse(r.connectionString(password))

	require.NoError(t, err)
	got, ok := u.User.Password()
	assert.True(t, ok)
	assert.Equal(t, password, got)
	assert.Equal(t, "postgres", u.User.Username())
	assert.Equal(t, "nvke-postgresql.postgres.svc.cluster.local:5432", u.Host)
	assert.Equal(t, "/nvke-controller", u.Path)
	assert.Equal(t, "disable", u.Query().Get("sslmode"))
}

func TestCredentialSecretsRefusesPartialCredentials(t *testing.T) {
	tests := []struct {
		field string
		creds Credentials
	}{
		{"DatabasePassword", Credentials{NicoToken: "t", NodeUser: "u", NodePassword: "p"}},
		{"NicoToken", Credentials{DatabasePassword: "d", NodeUser: "u", NodePassword: "p"}},
		{"NodeUser", Credentials{DatabasePassword: "d", NicoToken: "t", NodePassword: "p"}},
		{"NodePassword", Credentials{DatabasePassword: "d", NicoToken: "t", NodeUser: "u"}},
		{"SpireCAKey", Credentials{DatabasePassword: "d", NicoToken: "t", NodeUser: "u", NodePassword: "p", SpireCACert: []byte("c")}},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			_, err := CredentialSecrets(tt.creds, testNICo)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.field)
		})
	}
}

func TestCredentialSecretsRefusesEmptyNICoAPI(t *testing.T) {
	tests := []struct {
		field string
		nico  NICoAPI
	}{
		{"Endpoint", NICoAPI{OrgID: "o", APIName: "a"}},
		{"OrgID", NICoAPI{Endpoint: "e", APIName: "a"}},
		{"APIName", NICoAPI{Endpoint: "e", OrgID: "o"}},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			_, err := CredentialSecrets(testCredentials, tt.nico)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "NICoAPI."+tt.field)
		})
	}
}

// TestCredentialSecretsNamespacesExist checks every Secret lands in a
// namespace that manifests/namespaces.yaml creates.
func TestCredentialSecretsNamespacesExist(t *testing.T) {
	secrets, err := CredentialSecrets(withCA(testCredentials), testNICo)
	require.NoError(t, err)
	namespaces := manifestNamespaces(t)
	for _, s := range secrets {
		assert.Contains(t, namespaces, s.Namespace, s.Name)
	}
}

func TestCredentialSecretsRegisterConnectionString(t *testing.T) {
	_, err := CredentialSecrets(testCredentials, testNICo)
	require.NoError(t, err)
	r, err := readSecretRefs()
	require.NoError(t, err)
	conn := r.connectionString(testCredentials.DatabasePassword)
	assert.Equal(t, redact.Placeholder, redact.Text(conn))
	assert.Equal(t, redact.Placeholder, redact.Text(base64.StdEncoding.EncodeToString([]byte(conn))))
}

func manifestNamespaces(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("stack", "manifests", "namespaces.yaml"))
	require.NoError(t, err)
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var names []string
	for {
		var doc struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return names
		}
		require.NoError(t, err)
		if doc.Kind == "Namespace" {
			names = append(names, doc.Metadata.Name)
		}
	}
}
