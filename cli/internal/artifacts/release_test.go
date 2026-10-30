// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"cmp"
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
		require.Len(t, f.Artifacts, 2)

		chart := f.Artifact("gpu-runtime")
		require.NotNil(t, chart)
		assert.Equal(t, KindChart, chart.Kind)
		assert.Equal(t, "1.0.0", chart.ChartVersion)
		assert.True(t, strings.HasSuffix(chart.Reference, "@sha256:3c5490c74f24352b9dc8da0bb032dea92b5c531ef4fa7a640ecbd08f7cbe8888"))
		assert.Nil(t, chart.Platforms())

		apt := f.Artifact("worker-apt")
		require.NotNil(t, apt)
		assert.Equal(t, "apt-repository", apt.Kind)

		assert.Nil(t, f.Artifact("cwe"))
	})

	t.Run("executables", func(t *testing.T) {
		f := decodeRelease(t, "nvkr/exec-export.json")
		require.Len(t, f.Artifacts, 4)

		alpha := f.Artifact("alpha")
		require.NotNil(t, alpha)
		assert.Equal(t, "executable", alpha.Kind)
		assert.Equal(t, []string{"linux/amd64"}, alpha.Platforms())
		assert.Equal(t, []string{"linux/arm64"}, f.Artifact("beta").Platforms())
	})
}

func TestDecodeValid(t *testing.T) {
	f := decodeRelease(t, "valid.json")
	require.Len(t, f.Artifacts, len(Expected))
	for _, want := range Expected {
		o := f.Artifact(want.Name)
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

	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Artifact("cwe").Platforms())
	assert.Equal(t, "0.30.1", f.Artifact("spire").ChartVersion)
}

func TestDecodeMinimal(t *testing.T) {
	f := decodeRelease(t, "cases/minimal-hand-written.json")
	assert.Empty(t, f.RootDigest)
	require.NotNil(t, f.Artifact("cwe"))
	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Artifact("cwe").Platforms())
	assert.Equal(t, "0.30.1", f.Artifact("spire").ChartVersion)
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
		out  Artifact
		want []string
	}{
		"index":              {Artifact{ImageIndex: index}, []string{"linux/amd64", "linux/arm64"}},
		"index wins over os": {Artifact{ImageIndex: index, OS: "linux", Arch: "amd64"}, []string{"linux/amd64", "linux/arm64"}},
		"empty index":        {Artifact{ImageIndex: &ImageIndex{}}, nil},
		"os and arch":        {Artifact{OS: "linux", Arch: "arm64"}, []string{"linux/arm64"}},
		"os only":            {Artifact{OS: "linux"}, nil},
		"arch only":          {Artifact{Arch: "arm64"}, nil},
		"chart":              {Artifact{Kind: KindChart, ChartVersion: "1.0.0"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, c.out.Platforms())
		})
	}
}

type validationCase struct {
	File     string `yaml:"file"`
	Platform string `yaml:"platform"`
	Expect   string `yaml:"expect"`
}

func readCases(t *testing.T) []validationCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "cases.yaml"))
	require.NoError(t, err)
	var cases []validationCase
	require.NoError(t, yaml.Unmarshal(data, &cases))
	require.NotEmpty(t, cases)
	return cases
}

func TestValidateCases(t *testing.T) {
	for _, c := range readCases(t) {
		platform := cmp.Or(c.Platform, "linux/amd64")
		t.Run(c.File+" "+platform, func(t *testing.T) {
			r, err := os.Open(filepath.Join("testdata", c.File))
			require.NoError(t, err)
			defer r.Close()
			rel, err := Decode(r)
			if err == nil {
				err = Validate(rel, platform)
			}
			if c.Expect == "ok" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, c.Expect)
		})
	}
}

// Every case file is listed in cases.yaml and every listed file exists.
func TestCasesListed(t *testing.T) {
	listed := map[string]bool{}
	for _, c := range readCases(t) {
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
