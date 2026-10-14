// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

func TestBuildResolversBuildsOnePerComponent(t *testing.T) {
	reg := &qualification.Registry{Spec: qualification.RegistrySpec{Components: []qualification.Component{
		{Name: "controller", Resolve: qualification.Resolve{Strategy: "gitTag", With: map[string]string{"tagPrefix": "controller/"}}},
		{Name: "kind", Resolve: qualification.Resolve{Strategy: "makeVar", With: map[string]string{"var": "KIND_VERSION"}}},
	}}}

	got, err := buildResolvers(reg, ".", resolve.Options{})
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

	_, err := buildResolvers(reg, ".", resolve.Options{})
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
		Sources: []qualification.Source{{Name: "artifacts", Base: cdn.URL}},
		Components: []qualification.Component{
			{Name: "loader", Resolve: qualification.Resolve{Strategy: "url", With: map[string]string{"source": "artifacts"}}},
		},
	}}

	got, err := buildResolvers(reg, ".", resolve.Options{})
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

// The option reaches the strategies that can act on it, so a plan made with
// --check-exists asks and one made without does not.
func TestBuildResolversPassesCheckExists(t *testing.T) {
	// Answers 403 for everything, which is what the private bucket does for an
	// object that is not there.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(cdn.Close)

	reg := &qualification.Registry{Spec: qualification.RegistrySpec{
		Sources: []qualification.Source{{Name: "artifacts", Base: cdn.URL}},
		Components: []qualification.Component{
			{Name: "loader", Resolve: qualification.Resolve{Strategy: "url", With: map[string]string{"source": "artifacts"}}},
		},
	}}
	inputs := map[string]string{"value": "efi/gone.efi"}

	off, err := buildResolvers(reg, ".", resolve.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off["loader"].Resolve(inputs); err != nil {
		t.Errorf("resolving asked whether the artifact exists: %v", err)
	}

	on, err := buildResolvers(reg, ".", resolve.Options{CheckExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := on["loader"].Resolve(inputs); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v, want it to report the 403", err)
	}
}
