// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"reflect"
	"strings"
	"testing"
)

const validRegistry = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Registry
spec:
  components:
    - {name: controller, resolve: {strategy: gitTag, with: {tagPrefix: "controller/"}}}
    - {name: kind, resolve: {strategy: makeVar, with: {var: KIND_VERSION}}}
`

func TestLoadRegistryAccepts(t *testing.T) {
	r, err := LoadRegistry(write(t, "registry.yaml", validRegistry))
	if err != nil {
		t.Fatal(err)
	}

	c, ok := r.Component("controller")
	if !ok {
		t.Fatal("controller is not in the registry")
	}
	if c.Resolve.Strategy != "gitTag" || c.Resolve.With["tagPrefix"] != "controller/" {
		t.Errorf("controller resolves as %+v", c.Resolve)
	}
	if _, ok := r.Component("agent"); ok {
		t.Error("agent is not registered, but Component found it")
	}
}

func TestLoadRegistryRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"wrong kind", strings.Replace(validRegistry, "Registry", "Widget", 1), "kind"},
		{"unknown field", strings.Replace(validRegistry, "{name: kind,", "{name: kind, e2eKey: kind,", 1), "unknown field"},
		{
			"duplicate name",
			validRegistry + "    - {name: kind, resolve: {strategy: makeVar, with: {var: X}}}\n",
			"listed twice",
		},
		{
			"no strategy",
			strings.Replace(validRegistry, "strategy: makeVar, ", "", 1),
			"names no strategy",
		},
		{"no name", strings.Replace(validRegistry, "{name: kind,", "{name: \"\",", 1), "has no name"},
		{
			"no components",
			"apiVersion: nvke.nvidia.com/v1alpha1\nkind: Registry\nspec:\n  components: []\n",
			"spec.components is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRegistry(write(t, "registry.yaml", tc.body))
			wantError(t, err, tc.want)
		})
	}
}

const sourcedRegistry = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Registry
spec:
  sources:
    - {name: artifacts, url: "https://cdn.example"}
  components:
    - {name: image, resolve: {strategy: osImage, with: {source: artifacts, path: p, from: os-image, arch: amd64, variant: non-fde}}}
    - {name: loader, resolve: {strategy: url, with: {source: artifacts}}}
    - {name: kind, resolve: {strategy: makeVar, with: {var: KIND_VERSION}}}
    - {name: bare, resolve: {strategy: gitTag}}
`

// A source is stated once and expanded into the flat base the strategies take.
func TestRegistryExpandsASourceIntoABase(t *testing.T) {
	r, err := LoadRegistry(write(t, "registry.yaml", sourcedRegistry))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		want map[string]string
	}{
		{"image", map[string]string{"base": "https://cdn.example", "path": "p", "from": "os-image", "arch": "amd64", "variant": "non-fde"}},
		{"loader", map[string]string{"base": "https://cdn.example"}},
		{"kind", map[string]string{"var": "KIND_VERSION"}},
		{"bare", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := r.Component(tc.name)
			got := r.StrategyConfig(c)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("config is %v, want %v", got, tc.want)
			}
			if _, ok := c.Resolve.With["source"]; tc.name == "loader" && !ok {
				t.Error("expanding changed the registry's own entry")
			}
		})
	}
}

func TestLoadRegistryRejectsBadSources(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{
			"a duplicate source",
			`    - {name: artifacts, url: "https://cdn.example"}`,
			"    - {name: artifacts, url: \"https://cdn.example\"}\n    - {name: artifacts, url: \"https://other.example\"}",
			`source "artifacts" is listed twice`,
		},
		{"a source with no name", `{name: artifacts, url: "https://cdn.example"}`, `{name: "", url: "https://cdn.example"}`, "a source has no name"},
		{"a source that is not a URL", `url: "https://cdn.example"`, `url: "cdn.example"`, `has url "cdn.example", which is not a URL`},
		{"a component naming a source that is not listed", "{source: artifacts}", "{source: elsewhere}", `names source "elsewhere", which is not listed`},
		{"a source beside a base", "{source: artifacts}", "{source: artifacts, base: https://x.example}", "names both a source and a base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRegistry(write(t, "registry.yaml", strings.Replace(sourcedRegistry, tc.from, tc.to, 1)))
			wantError(t, err, tc.want)
		})
	}
}
