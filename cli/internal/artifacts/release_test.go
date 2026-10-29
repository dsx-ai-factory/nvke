// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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

		assert.Nil(t, f.Output("cwe"))
	})

	t.Run("executables", func(t *testing.T) {
		f := decodeRelease(t, "nvkr/exec-export.json")
		require.Len(t, f.Outputs, 4)

		alpha := f.Output("alpha")
		require.NotNil(t, alpha)
		assert.Equal(t, "executable", alpha.Kind)
		assert.Equal(t, []string{"linux/amd64"}, alpha.Platforms())
		assert.Equal(t, []string{"linux/arm64"}, f.Output("beta").Platforms())
	})
}

func TestDecodeValid(t *testing.T) {
	f := decodeRelease(t, "valid.json")
	require.Len(t, f.Outputs, len(Expected))
	for _, want := range Expected {
		o := f.Output(want.Name)
		require.NotNil(t, o, want.Name)
		assert.Equal(t, want.Kind, o.Kind, want.Name)
		if want.Kind == KindChart {
			assert.NotEmpty(t, o.ChartVersion, want.Name)
			assert.Nil(t, o.Platforms(), want.Name)
		} else {
			assert.Empty(t, o.ChartVersion, want.Name)
			assert.NotEmpty(t, o.Platforms(), want.Name)
		}
	}

	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Output("cwe").Platforms())
	assert.Equal(t, "0.30.1", f.Output("spire").ChartVersion)
}

func TestDecodeMinimal(t *testing.T) {
	f := decodeRelease(t, "cases/minimal-hand-written.json")
	assert.Empty(t, f.RootDigest)
	require.NotNil(t, f.Output("cwe"))
	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Output("cwe").Platforms())
	assert.Equal(t, "0.30.1", f.Output("spire").ChartVersion)
}

func TestDecodeCases(t *testing.T) {
	decodeErrors := map[string]string{
		"not-json.json":         "not valid JSON",
		"null.json":             "not a JSON object",
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

func TestDecodeReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Decode(io.MultiReader(strings.NewReader("{}"), iotest.ErrReader(boom)))
	require.ErrorIs(t, err, boom)
	assert.NotContains(t, err.Error(), "content after")
}

func TestPlatforms(t *testing.T) {
	index := &ImageIndex{Children: []ImageIndexChild{{Platform: "linux/amd64"}, {Platform: "linux/arm64"}}}
	cases := map[string]struct {
		out  Output
		want []string
	}{
		"index":              {Output{ImageIndex: index}, []string{"linux/amd64", "linux/arm64"}},
		"index wins over os": {Output{ImageIndex: index, OS: "linux", Arch: "amd64"}, []string{"linux/amd64", "linux/arm64"}},
		"empty index":        {Output{ImageIndex: &ImageIndex{}}, nil},
		"os and arch":        {Output{OS: "linux", Arch: "arm64"}, []string{"linux/arm64"}},
		"os only":            {Output{OS: "linux"}, nil},
		"arch only":          {Output{Arch: "arm64"}, nil},
		"chart":              {Output{Kind: KindChart, ChartVersion: "1.0.0"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, c.out.Platforms())
		})
	}
}

// Every case file is listed in cases.yaml and every listed file exists.
func TestCasesListed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cases.yaml"))
	require.NoError(t, err)
	var cases []struct {
		File     string `yaml:"file"`
		Platform string `yaml:"platform"`
		Expect   string `yaml:"expect"`
	}
	require.NoError(t, yaml.Unmarshal(data, &cases))

	listed := map[string]bool{}
	for _, c := range cases {
		assert.NotEmpty(t, c.Expect, c.File)
		_, err := os.Stat(filepath.Join("testdata", c.File))
		assert.NoError(t, err, c.File)
		listed[c.File] = true
	}
	for _, dir := range []string{"cases", "nvkr"} {
		files, err := filepath.Glob(filepath.Join("testdata", dir, "*.json"))
		require.NoError(t, err)
		for _, f := range files {
			rel, _ := filepath.Rel("testdata", f)
			assert.True(t, listed[filepath.ToSlash(rel)], "%s is not in cases.yaml", rel)
		}
	}
	assert.True(t, listed["valid.json"])
}
