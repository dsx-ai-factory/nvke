// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	cases := map[string]struct {
		in   string
		want Version
		ok   bool
	}{
		"plain":           {"18.5.15", Version{18, 5, 15}, true},
		"zero":            {"0.0.0", Version{}, true},
		"two parts":       {"1.2", Version{}, false},
		"four parts":      {"1.2.3.4", Version{}, false},
		"v prefix":        {"v1.2.3", Version{}, false},
		"prerelease":      {"1.2.3-rc.1", Version{}, false},
		"build metadata":  {"1.2.3+build", Version{}, false},
		"sign":            {"+1.2.3", Version{}, false},
		"letters":         {"a.b.c", Version{}, false},
		"empty":           {"", Version{}, false},
		"empty component": {"1..3", Version{}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseVersion(c.in)
			if !c.ok {
				require.ErrorContains(t, err, "is not MAJOR.MINOR.PATCH")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.in, got.String())
		})
	}
}

func TestVersionRangeContains(t *testing.T) {
	r := VersionRange{Version{0, 30, 1}, Version{0, 31, 0}}
	cases := map[string]struct {
		v    Version
		want bool
	}{
		"min":         {Version{0, 30, 1}, true},
		"inside":      {Version{0, 30, 9}, true},
		"below min":   {Version{0, 30, 0}, false},
		"max":         {Version{0, 31, 0}, false},
		"above max":   {Version{1, 0, 0}, false},
		"newer minor": {Version{0, 99, 99}, false},
		"older minor": {Version{0, 29, 99}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, r.contains(c.v))
		})
	}
}

// Every chart row has a non-empty range; an image row has none.
func TestExpectedRanges(t *testing.T) {
	for _, e := range Expected {
		if e.Kind == KindChart {
			assert.Negative(t, e.ChartVersions.Min.compare(e.ChartVersions.Max), e.Name)
		} else {
			assert.Zero(t, e.ChartVersions, e.Name)
		}
	}
}

func TestValidateEdges(t *testing.T) {
	t.Run("three of one name", func(t *testing.T) {
		rel := decodeRelease(t, "valid.json")
		cwe := *rel.Artifact("cwe")
		rel.Artifacts = append(rel.Artifacts, cwe, cwe)
		require.ErrorContains(t, Validate(rel, "linux/amd64"), `artifact "cwe": appears 3 times`)
	})

	t.Run("chart version not semver", func(t *testing.T) {
		rel := decodeRelease(t, "valid.json")
		rel.Artifact("spire").ChartVersion = "0.30"
		require.ErrorContains(t, Validate(rel, "linux/amd64"), `artifact "spire": chartVersion "0.30" is not MAJOR.MINOR.PATCH`)
	})

	t.Run("reference digest malformed", func(t *testing.T) {
		rel := decodeRelease(t, "valid.json")
		rel.Artifact("spire").Reference = "registry.example.invalid/nvke/charts/spire@sha256:abc"
		require.ErrorContains(t, Validate(rel, "linux/amd64"), `artifact "spire": reference has no digest`)
	})

	t.Run("platform the images do not serve", func(t *testing.T) {
		rel := decodeRelease(t, "valid.json")
		require.ErrorContains(t, Validate(rel, "linux/riscv64"), `artifact "cwe": no manifest for platform linux/riscv64`)
	})

	t.Run("every problem is reported in Expected order", func(t *testing.T) {
		rel := decodeRelease(t, "valid.json")
		rel.Artifact("cwe").Reference = ""
		rel.Artifact("temporal").ChartVersion = "0.75.0"
		err := Validate(rel, "linux/amd64")
		require.Error(t, err)
		assert.Equal(t, []string{
			`artifact "cwe": reference is missing`,
			`artifact "temporal": chartVersion "0.75.0" is outside the supported range >=0.74.0, <0.75.0`,
		}, strings.Split(err.Error(), "\n"))
	})
}
