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
kind: QualificationRegistry
spec:
  components:
    - {name: controller, resolve: {version: {strategy: gitTag, with: {tagPrefix: "controller/"}}}}
    - {name: kind, resolve: {version: {strategy: makeVar, with: {var: KIND_VERSION}}}}
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
	if c.Resolve.Version.Strategy != "gitTag" || c.Resolve.Version.With["tagPrefix"] != "controller/" {
		t.Errorf("controller resolves as %+v", c.Resolve.Version)
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
			validRegistry + "    - {name: kind, resolve: {version: {strategy: makeVar, with: {var: X}}}}\n",
			"listed twice",
		},
		{
			"a version naming neither a strategy nor a from",
			strings.Replace(validRegistry, "strategy: makeVar, ", "", 1),
			"names neither a strategy nor a component to take one from",
		},
		{
			"a version naming both",
			strings.Replace(validRegistry, "strategy: makeVar,", "strategy: makeVar, from: controller,", 1),
			"two answers to one question",
		},
		{
			"an entry naming neither half",
			strings.Replace(validRegistry, "resolve: {version: {strategy: makeVar, with: {var: KIND_VERSION}}}", "resolve: {}", 1),
			"resolves neither a version nor a location",
		},
		{
			"a location taking a from",
			strings.Replace(validRegistry, "{name: kind, resolve: {version: {strategy: makeVar, with: {var: KIND_VERSION}}}}",
				"{name: kind, resolve: {location: {strategy: url, from: controller}}}", 1),
			"which a location does not take",
		},
		{
			"a location naming no strategy",
			strings.Replace(validRegistry, "{name: kind, resolve: {version: {strategy: makeVar, with: {var: KIND_VERSION}}}}",
				"{name: kind, resolve: {location: {with: {base: https://x.example}}}}", 1),
			"has a location that names no strategy",
		},
		{"no name", strings.Replace(validRegistry, "{name: kind,", "{name: \"\",", 1), "has no name"},
		{
			"no components",
			"apiVersion: nvke.nvidia.com/v1alpha1\nkind: QualificationRegistry\nspec:\n  components: []\n",
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
kind: QualificationRegistry
spec:
  sources:
    - {name: artifacts, base: "https://cdn.example"}
  components:
    - {name: os-image, resolve: {version: {strategy: gitTag, with: {tagPrefix: "os-image/"}}}}
    - {name: image, resolve: {version: {from: os-image}, location: {strategy: osImage, with: {source: artifacts, path: p, arch: amd64, variant: non-fde}}}}
    - {name: loader, resolve: {location: {strategy: url, with: {source: artifacts}}}}
    - {name: kind, resolve: {version: {strategy: makeVar, with: {var: KIND_VERSION}}}}
    - {name: bare, resolve: {version: {strategy: gitTag}}}
`

// A source is stated once and expanded into the flat base the strategies take.
func TestRegistryExpandsASourceIntoABase(t *testing.T) {
	r, err := LoadRegistry(write(t, "registry.yaml", sourcedRegistry))
	if err != nil {
		t.Fatal(err)
	}

	// Either half may name a source, so each is expanded where it is declared.
	for _, tc := range []struct {
		name string
		half func(Component) *Rule
		want map[string]string
	}{
		{
			"image", func(c Component) *Rule { return c.Resolve.Location },
			map[string]string{"base": "https://cdn.example", "path": "p", "arch": "amd64", "variant": "non-fde"},
		},
		{
			"loader", func(c Component) *Rule { return c.Resolve.Location },
			map[string]string{"base": "https://cdn.example"},
		},
		{
			"kind", func(c Component) *Rule { return c.Resolve.Version },
			map[string]string{"var": "KIND_VERSION"},
		},
		{"bare", func(c Component) *Rule { return c.Resolve.Version }, nil},
		{"image", func(c Component) *Rule { return c.Resolve.Version }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := r.Component(tc.name)
			got := r.StrategyConfig(tc.half(c))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("config is %v, want %v", got, tc.want)
			}
			if tc.name == "loader" {
				if _, ok := c.Resolve.Location.With["source"]; !ok {
					t.Error("expanding changed the registry's own entry")
				}
			}
		})
	}
}

func TestLoadRegistryRejectsBadSources(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{
			"a duplicate source",
			`    - {name: artifacts, base: "https://cdn.example"}`,
			"    - {name: artifacts, base: \"https://cdn.example\"}\n    - {name: artifacts, base: \"https://other.example\"}",
			`source "artifacts" is listed twice`,
		},
		{"a source with no name", `{name: artifacts, base: "https://cdn.example"}`, `{name: "", base: "https://cdn.example"}`, "a source has no name"},
		{"a source with no scheme", `base: "https://cdn.example"`, `base: "cdn.example"`, `has base "cdn.example", which names no scheme`},
		{"a source with a scheme nothing can check", `base: "https://cdn.example"`, `base: "ftp://cdn.example"`, `names no scheme`},
		{"a component naming a source that is not listed", "{source: artifacts}", "{source: elsewhere}", `names source "elsewhere", which is not listed`},
		{"a source beside a base", "{source: artifacts}", "{source: artifacts, base: https://x.example}", "names both a source and a base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRegistry(write(t, "registry.yaml", strings.Replace(sourcedRegistry, tc.from, tc.to, 1)))
			wantError(t, err, tc.want)
		})
	}
}

// A registry is a location like any other, so a source can name one and the
// components published there resolve against it.
func TestLoadRegistryAcceptsAnOCISource(t *testing.T) {
	yaml := strings.Replace(sourcedRegistry, `base: "https://cdn.example"`, `base: "oci://registry.example/org"`, 1)
	if _, err := LoadRegistry(write(t, "registry.yaml", yaml)); err != nil {
		t.Errorf("an oci source is a location: %v", err)
	}
}
