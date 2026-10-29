// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

var credentialEnvs = []string{
	envDatabasePassword,
	envNicoToken,
	sitespec.NicoCredentialsEnv,
	"NVKECTL_NODE_USER",
	"NVKECTL_NODE_PASSWORD",
}

// clearEnv empties every credential variable under its current and previous name,
// so the developer's shell cannot feed a test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, env := range credentialEnvs {
		t.Setenv(env, "")
		t.Setenv(envalias.Old(env), "")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestReadCredentialsSite(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDatabasePassword, " site-db-pw-1 ")
	t.Setenv(envNicoToken, "site-token-1\n")
	t.Setenv("NVKECTL_NODE_USER", "site-user-1")
	t.Setenv("NVKECTL_NODE_PASSWORD", "site-node-pw-1")

	c, err := ReadCredentials(Site, sitespec.Spire{})

	require.NoError(t, err)
	assert.Equal(t, Credentials{
		DatabasePassword: "site-db-pw-1",
		NicoToken:        "site-token-1",
		NodeUser:         "site-user-1",
		NodePassword:     "site-node-pw-1",
	}, c)
}

func TestReadCredentialsSeed(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDatabasePassword, "x")
	t.Setenv(sitespec.NicoCredentialsEnv, writeFile(t, "token", "seed-token-1\n"))
	t.Setenv("NVKECTL_NODE_USER", "seed-user-1")
	t.Setenv("NVKECTL_NODE_PASSWORD", "seed-node-pw-1")

	first, err := ReadCredentials(Seed, sitespec.Spire{})
	require.NoError(t, err)
	second, err := ReadCredentials(Seed, sitespec.Spire{})
	require.NoError(t, err)

	assert.NotEmpty(t, first.DatabasePassword)
	assert.NotEqual(t, "x", first.DatabasePassword)
	assert.NotEqual(t, first.DatabasePassword, second.DatabasePassword)
	assert.Equal(t, "seed-token-1", first.NicoToken)
}

func TestReadCredentialsMissing(t *testing.T) {
	tests := []struct {
		name  string
		kind  TargetKind
		unset []string
	}{
		{"site database password", Site, []string{envDatabasePassword}},
		{"site token", Site, []string{envNicoToken}},
		{"seed token file", Seed, []string{sitespec.NicoCredentialsEnv}},
		{"node user", Site, []string{"NVKECTL_NODE_USER"}},
		{"node password", Seed, []string{"NVKECTL_NODE_PASSWORD"}},
		{"site, all", Site, []string{envDatabasePassword, envNicoToken, "NVKECTL_NODE_USER", "NVKECTL_NODE_PASSWORD"}},
		{"seed, all", Seed, []string{sitespec.NicoCredentialsEnv, "NVKECTL_NODE_USER", "NVKECTL_NODE_PASSWORD"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envDatabasePassword, "missing-db-pw")
			t.Setenv(envNicoToken, "missing-token")
			t.Setenv(sitespec.NicoCredentialsEnv, writeFile(t, "token", "missing-file-token"))
			t.Setenv("NVKECTL_NODE_USER", "missing-user")
			t.Setenv("NVKECTL_NODE_PASSWORD", "missing-node-pw")
			for _, env := range tt.unset {
				t.Setenv(env, "")
			}

			_, err := ReadCredentials(tt.kind, sitespec.Spire{})

			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			require.Len(t, lines, len(tt.unset))
			for i, env := range tt.unset {
				assert.Contains(t, lines[i], env)
			}
		})
	}
}

func TestReadCredentialsErrorsCarryNoValue(t *testing.T) {
	clearEnv(t)
	t.Setenv(sitespec.NicoCredentialsEnv, writeFile(t, "token", "leak-tok leak-en"))
	t.Setenv("NVKECTL_NODE_USER", "leak-user")
	t.Setenv("NVKECTL_NODE_PASSWORD", `leak"node-pw`)
	spire := sitespec.Spire{
		UpstreamCA: true,
		CACertFile: writeFile(t, "ca.crt", "leak-cert-body"),
		CAKeyFile:  writeFile(t, "ca.key", " \n"),
	}

	_, err := ReadCredentials(Seed, spire)

	require.Error(t, err)
	for _, v := range []string{"leak-tok", "leak-en", `leak"node-pw`, "leak-cert-body"} {
		assert.NotContains(t, err.Error(), v)
	}
	assert.Contains(t, err.Error(), sitespec.EnvSpireCAKeyFile)
}

func TestReadCredentialsSpireCA(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDatabasePassword, "ca-db-pw")
	t.Setenv(envNicoToken, "ca-token")
	t.Setenv("NVKECTL_NODE_USER", "ca-user")
	t.Setenv("NVKECTL_NODE_PASSWORD", "ca-node-pw")
	spire := sitespec.Spire{
		UpstreamCA: true,
		CACertFile: writeFile(t, "ca.crt", "ca-cert-pem\n"),
		CAKeyFile:  writeFile(t, "ca.key", "ca-key-pem\n"),
	}

	c, err := ReadCredentials(Site, spire)
	require.NoError(t, err)
	assert.Equal(t, []byte("ca-cert-pem\n"), c.SpireCACert)
	assert.Equal(t, []byte("ca-key-pem\n"), c.SpireCAKey)

	spire.CAKeyFile = writeFile(t, "empty.key", "")
	_, err = ReadCredentials(Site, spire)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sitespec.EnvSpireCAKeyFile)
	assert.NotContains(t, err.Error(), sitespec.EnvSpireCACertFile)
}

func TestReadCredentialsRegistersWithRedact(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDatabasePassword, "redact-db-pw-7f3a")
	t.Setenv(envNicoToken, "redact-token-7f3a")
	t.Setenv("NVKECTL_NODE_USER", "redact-user-7f3a")
	t.Setenv("NVKECTL_NODE_PASSWORD", "redact-node-pw-7f3a")
	spire := sitespec.Spire{
		UpstreamCA: true,
		CACertFile: writeFile(t, "ca.crt", "redact-cert-7f3a"),
		CAKeyFile:  writeFile(t, "ca.key", "redact-key-7f3a"),
	}

	_, err := ReadCredentials(Site, spire)
	require.NoError(t, err)

	for _, v := range []string{"redact-db-pw-7f3a", "redact-token-7f3a", "redact-node-pw-7f3a", "redact-key-7f3a"} {
		assert.Equal(t, redact.Placeholder, redact.Text(v), v)
		encoded := base64.StdEncoding.EncodeToString([]byte(v))
		assert.Equal(t, redact.Placeholder, redact.Text(encoded), encoded)
	}
	assert.Equal(t, "redact-user-7f3a", redact.Text("redact-user-7f3a"))
	assert.Equal(t, "redact-cert-7f3a", redact.Text("redact-cert-7f3a"))
}

func TestReadCredentialsUnknownKind(t *testing.T) {
	clearEnv(t)
	t.Setenv("NVKECTL_NODE_USER", "kind-user")
	t.Setenv("NVKECTL_NODE_PASSWORD", "kind-node-pw")

	_, err := ReadCredentials("cloud", sitespec.Spire{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"cloud"`)
}
