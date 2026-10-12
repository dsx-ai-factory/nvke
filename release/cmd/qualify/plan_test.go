// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func TestBuildResolversBuildsOnePerComponent(t *testing.T) {
	reg := &qualification.Registry{Spec: qualification.RegistrySpec{Components: []qualification.Component{
		{Name: "controller", Resolve: qualification.Resolve{Strategy: "gitTag", With: map[string]string{"tagPrefix": "controller/"}}},
		{Name: "kind", Resolve: qualification.Resolve{Strategy: "makeVar", With: map[string]string{"var": "KIND_VERSION"}}},
	}}}

	got, err := buildResolvers(reg, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["controller"] == nil || got["kind"] == nil {
		t.Errorf("built %v", got)
	}
}

// A component the registry names but no strategy can build is reported by name,
// so the line to fix is the one in the registry.
func TestBuildResolversNamesTheComponentThatFails(t *testing.T) {
	reg := &qualification.Registry{Spec: qualification.RegistrySpec{Components: []qualification.Component{
		{Name: "kube", Resolve: qualification.Resolve{Strategy: "aptPackage"}},
	}}}

	_, err := buildResolvers(reg, ".")
	if err == nil || !strings.Contains(err.Error(), "component kube: strategy") {
		t.Errorf("err = %v, want it to name the component", err)
	}
}

// A component that names a source is built with the base the source stands for,
// and one that names a source nobody listed is a registry error before this.
func TestBuildResolversExpandsSources(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(cdn.Close)

	reg := &qualification.Registry{Spec: qualification.RegistrySpec{
		Sources: []qualification.Source{{Name: "artifacts", URL: cdn.URL}},
		Components: []qualification.Component{
			{Name: "loader", Resolve: qualification.Resolve{Strategy: "url", With: map[string]string{"source": "artifacts"}}},
		},
	}}

	got, err := buildResolvers(reg, ".")
	if err != nil {
		t.Fatal(err)
	}
	out, err := got["loader"].Resolve(map[string]string{"value": "efi/x.efi"})
	if err != nil {
		t.Fatal(err)
	}
	if out["url"] != cdn.URL+"/efi/x.efi" {
		t.Errorf("the loader resolved to %q, want the path under the source", out["url"])
	}
}
