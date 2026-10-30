// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the expected files")

func TestRenderWritesTree(t *testing.T) {
	p := mustPlan(t, testInputs(t, Site, "linux/amd64", true))
	dir := filepath.Join(t.TempDir(), "out")
	// An existing empty directory is tightened to 0700.
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, p.Render(dir))
	dirInfo, statErr := os.Stat(dir)
	require.NoError(t, statErr)
	assert.Equal(t, fs.FileMode(0o700), dirInfo.Mode().Perm())

	want := map[string]bool{}
	for _, rel := range p.Releases {
		for _, file := range rel.ValuesFiles {
			want[file] = true
		}
		if rel.Overlay != nil {
			want["overlays/"+rel.Name+".yaml"] = true
		}
	}
	for _, m := range p.Manifests {
		want[m.Path] = true
	}

	got := map[string]bool{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			assert.Equal(t, fs.FileMode(0o700), info.Mode().Perm(), path)
			return nil
		}
		assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm(), path)
		assert.False(t, strings.HasSuffix(path, ".tmpl"), path)
		rel, err := filepath.Rel(dir, path)
		got[filepath.ToSlash(rel)] = true
		return err
	}))
	assert.Equal(t, want, got)
	assert.True(t, got["overlays/nvke-controller.yaml"])
	assert.True(t, got["overlays/spire.yaml"])
	assert.False(t, got["overlays/postgres.yaml"], "no overlay without a mirror")

	err := p.Render(dir)
	require.ErrorContains(t, err, "is not empty")
}

// expectedCases are the plans whose overlays and rendered templates are pinned.
var expectedCases = map[string]func(t *testing.T) Inputs{
	"site-amd64": func(t *testing.T) Inputs {
		return testInputs(t, Site, "linux/amd64", true)
	},
	"site-arm64-mirror-upstreamca": func(t *testing.T) Inputs {
		in := testInputs(t, Site, "linux/arm64", true)
		in.Stack.Mirror = "mirror.example.internal:5000/nvke"
		in.Site.Spire.UpstreamCA = true
		return in
	},
	"seed-nico": func(t *testing.T) Inputs {
		in := testInputs(t, Seed, "linux/arm64", true)
		in.Site.Spire.UpstreamCA = true
		return in
	},
	"seed-dns-address": func(t *testing.T) Inputs {
		in := testInputs(t, Seed, "linux/amd64", false)
		in.Site.Spire.ExternalAddress = "seed.example.internal"
		return in
	},
}

// expectedFiles returns the overlays and rendered templates under dir, except
// the CAPNICo manifest, which is CAPNICo's own release file.
func expectedFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		_, tmplErr := fs.Stat(stackFS, rel+".tmpl")
		templated := tmplErr == nil && rel != "manifests/capnico.yaml"
		if !strings.HasPrefix(rel, "overlays/") && !templated {
			return nil
		}
		data, err := os.ReadFile(path)
		files[rel] = data
		return err
	})
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return files
}

func TestExpected(t *testing.T) {
	names := make([]string, 0, len(expectedCases))
	for name := range expectedCases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			require.NoError(t, mustPlan(t, expectedCases[name](t)).Render(out))
			got := expectedFiles(t, out)
			expectedDir := filepath.Join("testdata", "expected", name)
			if *update {
				require.NoError(t, os.RemoveAll(expectedDir))
				for rel, data := range got {
					path := filepath.Join(expectedDir, filepath.FromSlash(rel))
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, data, 0o644))
				}
			}
			want := expectedFiles(t, expectedDir)
			require.NotEmpty(t, want)
			assert.ElementsMatch(t, keys(want), keys(got), "the generated file set")
			for rel, data := range want {
				assert.Equal(t, string(data), string(got[rel]), rel)
			}
		})
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
