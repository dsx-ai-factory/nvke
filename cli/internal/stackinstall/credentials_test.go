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

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// testSources writes one file per credential, each value prefixed so a leak
// into an error is recognisable.
func testSources(t *testing.T, prefix string) CredentialSources {
	t.Helper()
	return CredentialSources{
		DatabasePasswordFile: writeFile(t, "db", " "+prefix+"-db-pw\n"),
		NicoTokenFile:        writeFile(t, "token", prefix+"-token\n"),
		NodeUser:             prefix + "-user",
		NodePasswordFile:     writeFile(t, "node", prefix+"-node-pw"),
	}
}

func withCAFiles(t *testing.T, src CredentialSources, prefix string) CredentialSources {
	t.Helper()
	src.SpireCACertFile = writeFile(t, "ca.crt", prefix+"-cert\n")
	src.SpireCAKeyFile = writeFile(t, "ca.key", prefix+"-key\n")
	return src
}

func TestReadCredentialsSite(t *testing.T) {
	c, err := ReadCredentials(Site, false, testSources(t, "site"))

	require.NoError(t, err)
	assert.Equal(t, Credentials{DatabasePassword: "site-db-pw", NicoToken: "site-token", NodeUser: "site-user", NodePassword: "site-node-pw"}, c)
}

func TestReadCredentialsSeed(t *testing.T) {
	src := testSources(t, "seed")
	src.DatabasePasswordFile = ""

	first, err := ReadCredentials(Seed, false, src)
	require.NoError(t, err)
	second, err := ReadCredentials(Seed, false, src)
	require.NoError(t, err)

	assert.NotEmpty(t, first.DatabasePassword)
	assert.NotEqual(t, first.DatabasePassword, second.DatabasePassword, "generated per run")
	assert.Equal(t, "seed-token", first.NicoToken)

	src.DatabasePasswordFile = writeFile(t, "db", "ignored")
	_, err = ReadCredentials(Seed, false, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "databasePasswordFile is not read on the seed")
}

func TestReadCredentialsMissing(t *testing.T) {
	tests := []struct {
		name  string
		kind  TargetKind
		unset func(*CredentialSources)
		want  string
	}{
		{"database password file", Site, func(s *CredentialSources) { s.DatabasePasswordFile = "" }, "credentials.databasePasswordFile is not set"},
		{"token file", Site, func(s *CredentialSources) { s.NicoTokenFile = "" }, "credentials.nicoTokenFile is not set"},
		{"node user", Seed, func(s *CredentialSources) { s.NodeUser = " " }, "credentials.nodeUser is not set"},
		{"node password file", Seed, func(s *CredentialSources) { s.NodePasswordFile = "" }, "credentials.nodePasswordFile is not set"},
		{"missing file", Site, func(s *CredentialSources) { s.NodePasswordFile = filepath.Join(t.TempDir(), "absent") }, "credentials.nodePasswordFile: no such file"},
		{"empty file", Site, func(s *CredentialSources) { s.DatabasePasswordFile = writeFile(t, "db", " \n") }, "credentials.databasePasswordFile names an empty file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := testSources(t, "missing")
			if tt.kind == Seed {
				src.DatabasePasswordFile = ""
			}
			tt.unset(&src)

			_, err := ReadCredentials(tt.kind, false, src)

			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tt.want)
		})
	}
}

func TestReadCredentialsAllMissing(t *testing.T) {
	_, err := ReadCredentials(Site, false, CredentialSources{})

	require.Error(t, err)
	assert.Len(t, strings.Split(err.Error(), "\n"), 4)
}

func TestReadCredentialsErrorsCarryNoValueOrPath(t *testing.T) {
	src := testSources(t, "leak")
	src.NicoTokenFile = writeFile(t, "token", "leak-tok leak-en")
	src.NodeUser = `leak"user`
	src.NodePasswordFile = writeFile(t, "node", `leak"node-pw`)
	src = withCAFiles(t, src, "leak")
	src.SpireCAKeyFile = "-----BEGIN PRIVATE KEY-----leak-pasted-key"

	_, err := ReadCredentials(Site, true, src)

	require.Error(t, err)
	for _, v := range []string{"leak-tok", "leak-en", `leak"user`, `leak"node-pw`, "leak-pasted-key", "leak-cert", src.NicoTokenFile, src.NodePasswordFile, src.SpireCACertFile} {
		assert.NotContains(t, err.Error(), v)
	}
	for _, key := range []string{"credentials.nicoTokenFile", "credentials.nodeUser", "credentials.nodePasswordFile", "credentials.spireCAKeyFile"} {
		assert.Contains(t, err.Error(), key)
	}
}

func TestReadCredentialsSpireCA(t *testing.T) {
	src := withCAFiles(t, testSources(t, "ca"), "ca")

	c, err := ReadCredentials(Site, true, src)
	require.NoError(t, err)
	assert.Equal(t, "ca-cert\n", string(c.SpireCACert))
	assert.Equal(t, "ca-key\n", string(c.SpireCAKey))

	_, err = ReadCredentials(Site, false, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not set spire.upstreamCA")

	src.SpireCAKeyFile = ""
	_, err = ReadCredentials(Site, true, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentials.spireCAKeyFile is not set")
}

func TestReadCredentialsRegistersWithRedact(t *testing.T) {
	src := withCAFiles(t, testSources(t, "redact-7f3a"), "redact-7f3a")

	_, err := ReadCredentials(Site, true, src)
	require.NoError(t, err)

	for _, v := range []string{"redact-7f3a-db-pw", "redact-7f3a-token", "redact-7f3a-node-pw", "redact-7f3a-key\n"} {
		assert.Equal(t, redact.Placeholder, redact.Text(v), v)
		encoded := base64.StdEncoding.EncodeToString([]byte(v))
		assert.Equal(t, redact.Placeholder, redact.Text(encoded), encoded)
	}
	assert.Equal(t, "redact-7f3a-user", redact.Text("redact-7f3a-user"))
	assert.Equal(t, "redact-7f3a-cert", redact.Text("redact-7f3a-cert"))
}

func TestReadCredentialsUnknownKind(t *testing.T) {
	_, err := ReadCredentials(TargetKind("other"), false, testSources(t, "kind"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown install target "other"`)
}
