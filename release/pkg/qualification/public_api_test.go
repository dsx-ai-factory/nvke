// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/pkg/qualification"
	"github.com/dsx-ai-factory/nvke/release/pkg/resolve"
)

// resolverFunc is a resolver written as a function, which is what a caller's
// own strategy returns.
type resolverFunc func(inputs map[string]string) (map[string]string, error)

func (f resolverFunc) Resolve(inputs map[string]string) (map[string]string, error) { return f(inputs) }

const demoSubject = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Qualification
metadata:
  name: demo
spec:
  subject: demo
  components:
    - {name: widget, selectors: [latest]}
`

// A module outside this one builds resolvers from strategies of its own and
// plans with them, using only what this package exports. The plan carries what
// those strategies answered and the names the registry gave them.
func TestAddedStrategiesPlanThroughTheExportedAPI(t *testing.T) {
	var locationInputs []map[string]string
	extra := qualification.Strategies{
		Version: map[string]qualification.VersionFactory{
			"releaseVersion": func(with map[string]string, _ string) (resolve.Resolver, error) {
				return resolverFunc(func(map[string]string) (map[string]string, error) {
					return map[string]string{resolve.ResultVersionKey: with["version"]}, nil
				}), nil
			},
		},
		Location: map[string]qualification.LocationFactory{
			"releaseLocation": func(with map[string]string, _ resolve.Options) (resolve.Resolver, error) {
				return resolverFunc(func(inputs map[string]string) (map[string]string, error) {
					locationInputs = append(locationInputs, inputs)
					return map[string]string{resolve.ResultURLKey: with["base"] + "/" + with["output"]}, nil
				}), nil
			},
		},
	}

	reg := &qualification.Registry{
		APIVersion: qualification.APIVersion,
		Kind:       qualification.KindRegistry,
		Spec: qualification.RegistrySpec{
			Sources: []qualification.Source{{Name: "images", Base: "oci://registry.example/org"}},
			Components: []qualification.Component{
				{Name: "demo", Resolve: qualification.Resolve{
					Version: &qualification.Rule{Strategy: "releaseVersion", With: map[string]string{"version": "v1.2.3"}},
				}},
				{Name: "widget", Resolve: qualification.Resolve{
					Version: &qualification.Rule{Strategy: "releaseVersion", With: map[string]string{"version": "v0.4.0"}},
					Location: &qualification.Rule{
						Strategy: "releaseLocation",
						With:     map[string]string{"source": "images", "output": "widget"},
					},
				}},
			},
		},
	}
	policy := &qualification.Policy{
		APIVersion: qualification.APIVersion,
		Kind:       qualification.KindQualificationPolicy,
		Spec: qualification.PolicySpec{
			Scopes:  map[string]qualification.Scope{"full": {}},
			Streams: []qualification.StreamDefinition{{Name: "amd64", Capabilities: map[string]string{"arch": "amd64"}}},
		},
	}

	version, location, err := qualification.BuildResolvers(reg, t.TempDir(), resolve.Options{}, extra)
	if err != nil {
		t.Fatal(err)
	}

	spec := filepath.Join(t.TempDir(), "qualification.yaml")
	if err := os.WriteFile(spec, []byte(demoSubject), 0o600); err != nil {
		t.Fatal(err)
	}
	planner := &qualification.Planner{
		Policy: policy, Registry: reg,
		VersionResolvers: version, LocationResolvers: location,
		Root: t.TempDir(), SubjectFile: spec,
	}

	got, err := planner.Plan("demo", "", "full")
	if err != nil {
		t.Fatal(err)
	}

	if got.Subject != "demo" || got.Version != "v1.2.3" {
		t.Errorf("plan is %s %s, want demo v1.2.3", got.Subject, got.Version)
	}
	if len(got.Combinations) != 1 || len(got.Combinations[0].Components) != 1 {
		t.Fatalf("plan has combinations %+v, want one holding the widget", got.Combinations)
	}
	widget := got.Combinations[0].Components[0]
	want := qualification.ComponentVersion{
		Name:             "widget",
		Version:          "v0.4.0",
		URL:              "oci://registry.example/org/widget",
		VersionStrategy:  "releaseVersion",
		LocationStrategy: "releaseLocation",
	}
	if widget != want {
		t.Errorf("widget is %+v, want %+v", widget, want)
	}
	if len(locationInputs) == 0 {
		t.Error("the location strategy the caller added was never asked")
	}
}
