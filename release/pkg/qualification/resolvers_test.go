// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/pkg/resolve"
)

func TestBuildResolversBuildsOnePerHalf(t *testing.T) {
	reg := &Registry{Spec: RegistrySpec{
		Sources: []Source{{Name: "images", Base: "oci://registry.example/org"}},
		Components: []Component{
			{Name: "controller", Resolve: Resolve{
				Version:  &Rule{Strategy: "gitTag", With: map[string]string{"tagPrefix": "controller/"}},
				Location: &Rule{Strategy: "ociArtifact", With: map[string]string{"source": "images", "repository": "nvke-controller"}},
			}},
			{Name: "kind", Resolve: Resolve{
				Version: &Rule{Strategy: "makeVar", With: map[string]string{"var": "KIND_VERSION"}},
			}},
			// A version taken from another entry names no strategy, so there is
			// nothing to build for that half.
			{Name: "cli-amd64", Resolve: Resolve{
				Version:  &Rule{From: "cli"},
				Location: &Rule{Strategy: "ociArtifact", With: map[string]string{"source": "images", "repository": "nvkectl", "arch": "amd64"}},
			}},
		},
	}}

	version, location, err := BuildResolvers(reg, ".", resolve.Options{}, Strategies{})
	if err != nil {
		t.Fatal(err)
	}
	if len(version) != 2 || version["controller"] == nil || version["kind"] == nil {
		t.Errorf("version resolvers are %v, want controller and kind", version)
	}
	if len(location) != 2 || location["controller"] == nil || location["cli-amd64"] == nil {
		t.Errorf("location resolvers are %v, want controller and cli-amd64", location)
	}
}

// A component the registry names but no strategy can build is reported by name,
// so the line to fix is the one in the registry.
func TestBuildResolversNamesTheComponentThatFails(t *testing.T) {
	reg := &Registry{Spec: RegistrySpec{Components: []Component{
		{Name: "kube", Resolve: Resolve{Version: &Rule{Strategy: "debPackage"}}},
	}}}

	_, _, err := BuildResolvers(reg, ".", resolve.Options{}, Strategies{})
	if err == nil || !strings.Contains(err.Error(), `component kube, version: "debPackage" is not one of`) {
		t.Errorf("err = %v, want it to name the component", err)
	}
}

// A component that names a source is built with the base the source stands for,
// and one that names a source nobody listed is a registry error before this.
func TestBuildResolversExpandsSources(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(cdn.Close)

	reg := &Registry{Spec: RegistrySpec{
		Sources: []Source{{Name: "artifacts", Base: cdn.URL}},
		Components: []Component{
			{Name: "loader", Resolve: Resolve{
				Location: &Rule{Strategy: "url", With: map[string]string{"source": "artifacts"}},
			}},
		},
	}}

	_, got, err := BuildResolvers(reg, ".", resolve.Options{}, Strategies{})
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

	reg := &Registry{Spec: RegistrySpec{
		Sources: []Source{{Name: "artifacts", Base: cdn.URL}},
		Components: []Component{
			{Name: "loader", Resolve: Resolve{
				Location: &Rule{Strategy: "url", With: map[string]string{"source": "artifacts"}},
			}},
		},
	}}
	inputs := map[string]string{"value": "efi/gone.efi"}

	_, off, err := BuildResolvers(reg, ".", resolve.Options{}, Strategies{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off["loader"].Resolve(inputs); err != nil {
		t.Errorf("resolving asked whether the artifact exists: %v", err)
	}

	_, on, err := BuildResolvers(reg, ".", resolve.Options{CheckExists: true}, Strategies{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := on["loader"].Resolve(inputs); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v, want it to report the 403", err)
	}
}

// addedStrategiesRegistry declares one component whose halves both name a
// strategy the caller adds, beside one that names a built-in.
func addedStrategiesRegistry() *Registry {
	return &Registry{Spec: RegistrySpec{
		Sources: []Source{{Name: "images", Base: "oci://registry.example/org"}},
		Components: []Component{
			{Name: "controller", Resolve: Resolve{
				Version:  &Rule{Strategy: "releaseVersion", With: map[string]string{"output": "cwe"}},
				Location: &Rule{Strategy: "releaseLocation", With: map[string]string{"source": "images", "output": "cwe"}},
			}},
			{Name: "kind", Resolve: Resolve{
				Version: &Rule{Strategy: "makeVar", With: map[string]string{"var": "KIND_VERSION"}},
			}},
		},
	}}
}

// A strategy the caller adds is built by the caller's factory from the
// configuration the registry gives it, and sits beside the built-in ones.
func TestBuildResolversUsesAnAddedStrategy(t *testing.T) {
	versionFake, locationFake := fixed("v1", ""), fixed("", "oci://x")
	var gotWith map[string]string
	var gotRepo string
	var gotLocationWith map[string]string
	var gotOpts resolve.Options

	extra := Strategies{
		Version: map[string]VersionFactory{
			"releaseVersion": func(with map[string]string, repo string) (resolve.Resolver, error) {
				gotWith, gotRepo = with, repo
				return versionFake, nil
			},
		},
		Location: map[string]LocationFactory{
			"releaseLocation": func(with map[string]string, opts resolve.Options) (resolve.Resolver, error) {
				gotLocationWith, gotOpts = with, opts
				return locationFake, nil
			},
		},
	}

	version, location, err := BuildResolvers(addedStrategiesRegistry(), "tree", resolve.Options{CheckExists: true}, extra)
	if err != nil {
		t.Fatal(err)
	}

	if version["controller"] != resolve.Resolver(versionFake) || location["controller"] != resolve.Resolver(locationFake) {
		t.Errorf("the planner is not handed what the factories built: %v, %v", version["controller"], location["controller"])
	}
	if gotRepo != "tree" || len(gotWith) != 1 || gotWith["output"] != "cwe" {
		t.Errorf("the version factory got %v over %q, want the registry's configuration over the tree", gotWith, gotRepo)
	}
	// The source is expanded to its base before the factory sees it, as it is
	// for a built-in strategy.
	if gotLocationWith["base"] != "oci://registry.example/org" || gotLocationWith["output"] != "cwe" || gotLocationWith["source"] != "" {
		t.Errorf("the location factory got %v, want the source expanded to its base", gotLocationWith)
	}
	if !gotOpts.CheckExists {
		t.Error("the location factory was not handed the options")
	}
	if version["kind"] == nil {
		t.Error("a built-in strategy beside an added one was not built")
	}
}

// An addition that cannot be used is refused before anything is built, so a
// caller learns of it from its own wiring and not from a registry.
func TestBuildResolversRejectsABadAddition(t *testing.T) {
	version := func(map[string]string, string) (resolve.Resolver, error) { return fixed("v1", ""), nil }
	location := func(map[string]string, resolve.Options) (resolve.Resolver, error) { return fixed("", "x"), nil }

	tests := []struct {
		name  string
		extra Strategies
		want  string
	}{
		{"shadows a built-in version strategy", Strategies{Version: map[string]VersionFactory{"gitTag": version}}, `version strategy "gitTag" is built in`},
		{"shadows a built-in location strategy", Strategies{Location: map[string]LocationFactory{"url": location}}, `location strategy "url" is built in`},
		{"takes a name the other half's built-ins use", Strategies{Version: map[string]VersionFactory{"ociArtifact": version}}, `version strategy "ociArtifact" is built in`},
		{"has an empty version name", Strategies{Version: map[string]VersionFactory{"": version}}, "a version strategy is added with an empty name"},
		{"has an empty location name", Strategies{Location: map[string]LocationFactory{"": location}}, "a location strategy is added with an empty name"},
		{"has no version factory", Strategies{Version: map[string]VersionFactory{"releaseVersion": nil}}, `version strategy "releaseVersion" has no factory`},
		{"has no location factory", Strategies{Location: map[string]LocationFactory{"releaseLocation": nil}}, `location strategy "releaseLocation" has no factory`},
		{
			"is added to both halves",
			Strategies{Version: map[string]VersionFactory{"release": version}, Location: map[string]LocationFactory{"release": location}},
			`strategy "release" is added as both`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := BuildResolvers(&Registry{}, ".", resolve.Options{}, tc.extra)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A factory that fails is reported against the component and half whose line
// in the registry named the strategy, and the failure stays inspectable.
func TestBuildResolversNamesTheComponentWhoseAddedStrategyFails(t *testing.T) {
	boom := errors.New("no release description")
	extra := Strategies{Version: map[string]VersionFactory{
		"releaseVersion": func(map[string]string, string) (resolve.Resolver, error) { return nil, boom },
	}}

	_, _, err := BuildResolvers(addedStrategiesRegistry(), ".", resolve.Options{}, extra)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "component controller, version:") {
		t.Errorf("err = %v, want the factory's error under the component and half", err)
	}
}

// A factory that returns neither a resolver nor an error would leave the
// planner reading a missing entry as a component with no such half.
func TestBuildResolversRejectsAFactoryThatBuildsNothing(t *testing.T) {
	extra := Strategies{Location: map[string]LocationFactory{
		"releaseLocation": func(map[string]string, resolve.Options) (resolve.Resolver, error) { return nil, nil },
	}}
	extra.Version = map[string]VersionFactory{
		"releaseVersion": func(map[string]string, string) (resolve.Resolver, error) { return fixed("v1", ""), nil },
	}

	_, _, err := BuildResolvers(addedStrategiesRegistry(), ".", resolve.Options{}, extra)
	if err == nil || !strings.Contains(err.Error(), `component controller, location: strategy "releaseLocation" built no resolver`) {
		t.Errorf("err = %v, want it to say the strategy built no resolver", err)
	}
}

// Adding a strategy does not make every unknown name acceptable: one the caller
// did not add is refused as before.
func TestBuildResolversStillRefusesAnUnaddedStrategy(t *testing.T) {
	_, _, err := BuildResolvers(addedStrategiesRegistry(), ".", resolve.Options{}, Strategies{})
	if err == nil || !strings.Contains(err.Error(), `component controller, version: "releaseVersion" is not one of`) {
		t.Errorf("err = %v, want the unknown strategy refused", err)
	}
}
