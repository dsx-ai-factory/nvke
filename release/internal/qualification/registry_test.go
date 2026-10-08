// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"strings"
	"testing"
)

const validRegistry = `
apiVersion: nke.nvidia.com/v1alpha1
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
			"apiVersion: nke.nvidia.com/v1alpha1\nkind: Registry\nspec:\n  components: []\n",
			"spec.components is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRegistry(write(t, "registry.yaml", tc.body))
			wantError(t, err, tc.want)
		})
	}
}
