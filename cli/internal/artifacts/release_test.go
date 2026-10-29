// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeRelease(t *testing.T, path string) *Release {
	t.Helper()
	r, err := os.Open(filepath.Join("testdata", path))
	require.NoError(t, err)
	defer r.Close()
	f, err := Decode(r)
	require.NoError(t, err, path)
	return f
}

// The files under testdata/nvkr are unmodified nvkr release exports.
func TestDecodeNVKRExport(t *testing.T) {
	t.Run("helm chart and apt repository", func(t *testing.T) {
		f := decodeRelease(t, "nvkr/helm-export.json")
		assert.Equal(t, "sha256:3f92237e0617d36f56df83a0de80bcc76c3cee27ac2770b9ee119627400f58ac", f.RootDigest)
		require.Len(t, f.Outputs, 2)

		chart := f.Output("gpu-runtime")
		require.NotNil(t, chart)
		assert.Equal(t, KindChart, chart.Kind)
		assert.Equal(t, "1.0.0", chart.ChartVersion)
		assert.True(t, strings.HasSuffix(chart.Reference, "@sha256:3c5490c74f24352b9dc8da0bb032dea92b5c531ef4fa7a640ecbd08f7cbe8888"))
		assert.Nil(t, chart.Platforms())

		apt := f.Output("worker-apt")
		require.NotNil(t, apt)
		assert.Equal(t, "apt-repository", apt.Kind)

		require.Len(t, f.Evidence, 2)
		assert.Nil(t, f.Source("gpu-runtime"))
		assert.Nil(t, f.Output("cwe"))
	})

	t.Run("executables with per-output source", func(t *testing.T) {
		f := decodeRelease(t, "nvkr/exec-export.json")
		require.Len(t, f.Outputs, 4)

		alpha := f.Output("alpha")
		require.NotNil(t, alpha)
		assert.Equal(t, "executable", alpha.Kind)
		assert.Equal(t, []string{"linux/amd64"}, alpha.Platforms())

		require.NotNil(t, f.Source("alpha"))
		assert.Equal(t, "https://example.test/one", f.Source("alpha").Repo)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", f.Source("alpha").Commit)
		assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", f.Source("beta").Commit)
		assert.Nil(t, f.Source("omega"))
	})
}

func TestDecodeValid(t *testing.T) {
	f := decodeRelease(t, "valid.json")
	require.Len(t, f.Outputs, len(Expected))
	for name, want := range Expected {
		o := f.Output(name)
		require.NotNil(t, o, name)
		assert.Equal(t, want.Kind, o.Kind, name)
		if want.Kind == KindChart {
			assert.NotEmpty(t, o.ChartVersion, name)
			assert.Nil(t, o.Platforms(), name)
		} else {
			assert.Empty(t, o.ChartVersion, name)
			assert.NotEmpty(t, o.Platforms(), name)
		}
	}

	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Output("cwe").Platforms())
	assert.Equal(t, []string{"linux/amd64"}, f.Output("controller-profiles").Platforms())
	assert.Equal(t, "0.30.1", f.Output("spire").ChartVersion)
	require.NotNil(t, f.Source("cwe"))
	assert.Equal(t, "https://github.com/dsx-ai-factory/nvke", f.Source("cwe").Repo)
	assert.Nil(t, f.Source("spire"))
}

func TestDecodeMinimal(t *testing.T) {
	f := decodeRelease(t, "cases/minimal-hand-written.json")
	assert.Empty(t, f.RootDigest)
	assert.Empty(t, f.Evidence)
	assert.Nil(t, f.Source("cwe"))
	require.NotNil(t, f.Output("cwe"))
	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Output("cwe").Platforms())
	assert.Equal(t, []string{"linux/amd64"}, f.Output("controller-profiles").Platforms())
	assert.Equal(t, "0.30.1", f.Output("spire").ChartVersion)
}

func TestDecodeCases(t *testing.T) {
	decodeErrors := map[string]string{
		"not-json.json":         "not valid JSON",
		"trailing-content.json": "content after the JSON object",
	}
	files, err := filepath.Glob(filepath.Join("testdata", "cases", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			r, err := os.Open(path)
			require.NoError(t, err)
			defer r.Close()
			_, err = Decode(r)
			if want, ok := decodeErrors[name]; ok {
				require.ErrorContains(t, err, want)
				return
			}
			require.NoError(t, err)
		})
	}
}
