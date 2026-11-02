// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const planted = "e2eplantedvalue0123456789abcdef"

// tree writes files, keyed by their path under the returned directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

func TestSecretSetAdd(t *testing.T) {
	set := &SecretSet{}
	set.Add("")
	set.Add("a")
	set.Add("a")
	set.Add("b")
	assert.Equal(t, 2, set.Len(), "an empty value would match every file, and a repeat adds nothing")
}

func TestSecretSetFindIn(t *testing.T) {
	set := &SecretSet{}
	set.Add(planted)
	set.Add("x-tenant=" + planted[:10])

	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "a clean tree", files: map[string]string{"log": "msg=nothing to see", "diag/all.txt": "ok"}},
		{name: "the value in a file", files: map[string]string{"log": "msg=x value=" + planted}, want: []string{"log"}},
		{
			name:  "the value in a nested file",
			files: map[string]string{"diag/nvke/controller/logs.txt": "v=" + planted, "diag/other.txt": "ok"},
			want:  []string{"diag/nvke/controller/logs.txt"},
		},
		{
			name:  "the encoded form of the token",
			files: map[string]string{"diag/all.txt": "OTEL_EXPORTER_OTLP_HEADERS: authorization=Bearer%20" + planted},
			want:  []string{"diag/all.txt"},
		},
		{
			name:  "the tenant as a pair",
			files: map[string]string{"diag/all.txt": "OTEL_EXPORTER_OTLP_HEADERS: x-tenant=" + planted[:10]},
			want:  []string{"diag/all.txt"},
		},
		{name: "a bare tenant value, which nvkectl masks only as a pair", files: map[string]string{"log": planted[:10]}},
		{name: "a hit in each of two files", files: map[string]string{"b": planted, "a": planted}, want: []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tree(t, tt.files)
			got, err := set.FindIn(dir)
			require.NoError(t, err)
			var rel []string
			for _, path := range got {
				r, err := filepath.Rel(dir, path)
				require.NoError(t, err)
				rel = append(rel, r)
			}
			assert.Equal(t, tt.want, rel)
		})
	}
}

func TestSecretSetFindInPaths(t *testing.T) {
	set := &SecretSet{}
	set.Add(planted)
	dir := tree(t, map[string]string{"leak.log": planted, "clean.log": "ok"})

	t.Run("a file given by name", func(t *testing.T) {
		got, err := set.FindIn(filepath.Join(dir, "leak.log"))
		require.NoError(t, err)
		assert.Equal(t, []string{filepath.Join(dir, "leak.log")}, got)
	})
	t.Run("a path that does not exist, and an empty one, are skipped", func(t *testing.T) {
		got, err := set.FindIn(filepath.Join(dir, "missing"), "", filepath.Join(dir, "clean.log"))
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("several paths", func(t *testing.T) {
		other := tree(t, map[string]string{"also.log": "x " + planted})
		got, err := set.FindIn(dir, other)
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})
	t.Run("an empty set finds nothing", func(t *testing.T) {
		got, err := (&SecretSet{}).FindIn(dir)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestSecretSetFindInUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	set := &SecretSet{}
	set.Add(planted)
	dir := tree(t, map[string]string{"locked.log": planted})
	require.NoError(t, os.Chmod(filepath.Join(dir, "locked.log"), 0))

	_, err := set.FindIn(dir)
	require.Error(t, err, "a file that was not searched proves nothing")
	assert.NotContains(t, err.Error(), planted)
}

func TestSecretSetFile(t *testing.T) {
	set := &SecretSet{}
	set.Add(planted)
	set.Add("with\nnewline and \"quotes\"")
	path := filepath.Join(t.TempDir(), "known.json")

	require.NoError(t, set.WriteFile(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the values are secrets")

	loaded, err := LoadSecretSet(path)
	require.NoError(t, err)
	assert.Equal(t, 2, loaded.Len())
	dir := tree(t, map[string]string{"log": "x " + planted})
	got, err := loaded.FindIn(dir)
	require.NoError(t, err)
	assert.Len(t, got, 1, "a loaded set searches for the same values")

	assert.Empty(t, loaded.Incomplete())
	set.MarkIncomplete("the Postgres password: could not be read")
	require.NoError(t, set.WriteFile(path))
	loaded, err = LoadSecretSet(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"the Postgres password: could not be read"}, loaded.Incomplete(), "incompleteness survives the file")

	_, err = LoadSecretSet(filepath.Join(t.TempDir(), "missing.json"))
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
	_, err = LoadSecretSet(path)
	assert.Error(t, err)
}

func TestOTLPSettings(t *testing.T) {
	a, err := NewOTLPSettings()
	require.NoError(t, err)
	b, err := NewOTLPSettings()
	require.NoError(t, err)

	assert.NotEqual(t, a.Token, b.Token, "random per run, so none sits in the repository")
	assert.NotEqual(t, a.Token, a.Tenant)
	assert.Equal(t, "authorization=Bearer "+a.Token+",x-tenant="+a.Tenant, a.Headers())
	assert.Equal(t, "authorization=Bearer%20"+a.Token+",x-tenant="+a.Tenant, a.ControllerHeaders(),
		"the controller holds the header with its space encoded")
	assert.Equal(t, []string{a.Token, "x-tenant=" + a.Tenant}, a.Secrets())
	assert.Equal(t, []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + a.Endpoint,
		"OTEL_EXPORTER_OTLP_HEADERS=" + a.Headers(),
	}, a.Env())

	u, err := url.Parse(a.Endpoint)
	require.NoError(t, err)
	assert.Equal(t, "http", u.Scheme)
	assert.False(t, strings.HasPrefix(u.Hostname(), "127.") || u.Hostname() == "localhost",
		"a loopback endpoint would keep bootstrap site from passing the headers to the controller")
}

func TestSecretSetFindInVanishedFile(t *testing.T) {
	set := &SecretSet{}
	set.Add(planted)
	dir := tree(t, map[string]string{"a.log": "ok", "b.log": "v=" + planted, "c.log": "ok"})

	t.Run("a file that vanishes mid-walk does not end the search of the rest", func(t *testing.T) {
		read := func(path string) ([]byte, error) {
			if filepath.Base(path) == "a.log" {
				return nil, fs.ErrNotExist
			}
			return os.ReadFile(path)
		}
		got, err := set.findIn(read, dir)
		require.NoError(t, err)
		assert.Equal(t, []string{filepath.Join(dir, "b.log")}, got, "b.log comes after the file that vanished")
	})
	t.Run("any other read error still fails the search", func(t *testing.T) {
		read := func(path string) ([]byte, error) {
			if filepath.Base(path) == "a.log" {
				return nil, fs.ErrPermission
			}
			return os.ReadFile(path)
		}
		_, err := set.findIn(read, dir)
		assert.ErrorIs(t, err, fs.ErrPermission)
	})
}
