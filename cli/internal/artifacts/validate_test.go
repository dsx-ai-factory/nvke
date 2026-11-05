// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/version"
)

func TestVersionRangeContains(t *testing.T) {
	r := versions("0.30.1", "0.31.0")
	cases := map[string]struct {
		v    string
		want bool
	}{
		"min":               {"0.30.1", true},
		"inside":            {"0.30.9", true},
		"prerelease inside": {"0.30.2-rc.1", true},
		"prerelease of min": {"0.30.1-rc.1", false},
		"prerelease of max": {"0.31.0-rc.1", true},
		"build metadata":    {"0.30.1+build.7", true},
		"below min":         {"0.30.0", false},
		"max":               {"0.31.0", false},
		"above max":         {"1.0.0", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, r.contains(version.MustParseSemantic(c.v)))
		})
	}
}

// Every chart row has a non-empty range; an image row has none.
func TestExpectedRanges(t *testing.T) {
	for _, e := range Expected {
		if e.Kind == KindChart {
			assert.True(t, e.ChartVersions.Min.LessThan(e.ChartVersions.Max), e.Name)
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
		require.ErrorContains(t, Validate(rel, "linux/amd64"), `artifact "spire": chartVersion "0.30" is not a semantic version`)
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
