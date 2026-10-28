// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// PR jobs include reusable workflows reached through their job graph. Their
// Go build caches contain product objects, so they must stay runner-local.
func TestPRWorkflowsKeepProductBuildCachesLocal(t *testing.T) {
	type step struct {
		Uses string            `yaml:"uses"`
		With map[string]string `yaml:"with"`
	}
	type job struct {
		Uses  string `yaml:"uses"`
		Steps []step `yaml:"steps"`
	}
	type workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]job       `yaml:"jobs"`
	}
	root := "../../../.github/workflows"
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	all := make(map[string]workflow)
	var pending []string
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".yml" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(root, file.Name()))
		require.NoError(t, readErr)
		var spec workflow
		require.NoError(t, yaml.Unmarshal(raw, &spec), file.Name())
		all[file.Name()] = spec
		_, directPR := spec.On["pull_request"]
		var push struct {
			Branches []string `yaml:"branches"`
		}
		if node, exists := spec.On["push"]; exists {
			require.NoError(t, node.Decode(&push), file.Name())
		}
		for _, branch := range push.Branches {
			directPR = directPR || strings.HasPrefix(branch, "pull-request/")
		}
		if directPR {
			pending = append(pending, file.Name())
		}
	}
	seen := make(map[string]bool)
	for len(pending) > 0 {
		file := pending[0]
		pending = pending[1:]
		if seen[file] {
			continue
		}
		seen[file] = true
		spec, exists := all[file]
		require.True(t, exists, "referenced workflow %s is missing", file)
		for name, j := range spec.Jobs {
			if strings.HasPrefix(j.Uses, "./.github/workflows/") {
				pending = append(pending, strings.TrimPrefix(j.Uses, "./.github/workflows/"))
			}
			for _, s := range j.Steps {
				if strings.HasPrefix(s.Uses, "actions/setup-go@") {
					assert.Equal(t, "false", s.With["cache"], "%s/%s publishes Go product build objects", file, name)
				}
				if strings.HasPrefix(s.Uses, "actions/cache") {
					assert.NotContains(t, s.With["path"], "go-build", "%s/%s publishes Go product build objects", file, name)
				}
			}
		}
	}
	require.True(t, seen["go-build.yml"] && seen["gofail-e2e.yml"] && seen["backend-e2e.yml"], "required/reusable PR build lanes were not inspected")
}
