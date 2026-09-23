// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/release/internal/policy"
)

// fakeTags answers from a map instead of git.
type fakeTags map[string][]string

func (f fakeTags) Versions(component string) ([]string, error) { return f[component], nil }

// fakeHistory records which cells have settled and which versions are
// qualified, so a test can put the planner in a state the registry would
// otherwise have to be in.
type fakeHistory struct {
	qualified map[string]bool    // "component@version"
	outcomes  map[string]Outcome // "component@version/combination/stream"
}

func (f fakeHistory) Qualified(component, version string) (bool, error) {
	return f.qualified[component+"@"+version], nil
}

func (f fakeHistory) Outcome(component, version, combination, stream string) (Outcome, error) {
	return f.outcomes[component+"@"+version+"/"+combination+"/"+stream], nil
}

// testRepo writes component specs into a temp directory laid out like the
// repository, because that is what the planner reads.
func testRepo(t *testing.T, specs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for component, body := range specs {
		dir := filepath.Join(root, component)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "qualification.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testPolicy() *policy.Policy {
	return &policy.Policy{
		APIVersion: policy.APIVersion,
		Kind:       policy.KindQualificationPolicy,
		Spec: policy.Spec{
			Components: []string{"controller", "cli", "os-image"},
			Scopes: map[string]policy.Scope{
				"full":   {},
				"active": {Selector: map[string]string{"kube": "1.34"}},
			},
			Streams: []policy.Stream{
				{Name: "amd64-1.34", Capabilities: map[string]string{"arch": "amd64", "kube": "1.34"}},
				{Name: "arm64-1.34", Capabilities: map[string]string{"arch": "arm64", "kube": "1.34"}},
				{Name: "amd64-1.35", Capabilities: map[string]string{"arch": "amd64", "kube": "1.35"}},
			},
		},
	}
}

const controllerSpec = `
apiVersion: nke.nvidia.com/v1alpha1
kind: ComponentQualification
metadata: {name: controller}
spec:
  component: controller
  dependencies:
    - name: os-image
      selectors: [latest]
    - name: cli
      selectors: [latest]
`

func newPlanner(t *testing.T, spec string, tags fakeTags, history History) *Planner {
	t.Helper()
	if history == nil {
		history = NoHistory{}
	}
	return &Planner{
		Policy:  testPolicy(),
		Root:    testRepo(t, map[string]string{"controller": spec}),
		Tags:    tags,
		History: history,
	}
}

func TestPlanResolvesAndExpands(t *testing.T) {
	p := newPlanner(t, controllerSpec, fakeTags{
		"controller": {"v0.1.1-rc.8", "v0.1.0"},
		"os-image":   {"v0.1.0-rc.4", "v0.1.0-rc.2"},
		"cli":        {"v0.1.0-rc.7"},
	}, nil)

	got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Combinations) != 1 {
		t.Fatalf("got %d combinations, want 1", len(got.Combinations))
	}
	c := got.Combinations[0]

	// Dependencies keep declaration order, and the id is built from it, so
	// the same plan always names the same combination the same way.
	if c.ID != "os-image-v0.1.0-rc.4_cli-v0.1.0-rc.7" {
		t.Errorf("id is %q", c.ID)
	}
	if len(c.Streams) != 3 {
		t.Errorf("full should select all three streams, got %d", len(c.Streams))
	}
	for _, s := range c.Streams {
		if s.Skip != OutcomeNone {
			t.Errorf("%s is skipped, but nothing has run", s.Name)
		}
	}
}

func TestPlanScopeNarrows(t *testing.T) {
	p := newPlanner(t, controllerSpec, fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}, nil)

	got, err := p.Plan("controller", "v0.1.1-rc.8", "active")
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, s := range got.Combinations[0].Streams {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "amd64-1.34,arm64-1.34" {
		t.Errorf("active selected %v", names)
	}
}

func TestPlanKeepsSkippedStreamsInTheMatrix(t *testing.T) {
	const id = "os-image-v0.1.0-rc.4_cli-v0.1.0-rc.7"
	p := newPlanner(t, controllerSpec, fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}, fakeHistory{
		outcomes: map[string]Outcome{
			"controller@v0.1.1-rc.8/" + id + "/amd64-1.34": OutcomePassed,
			"controller@v0.1.1-rc.8/" + id + "/arm64-1.34": OutcomeError,
		},
	})

	got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
	if err != nil {
		t.Fatal(err)
	}

	// All three stay in the file. A run covering two of three should say so
	// rather than look like a smaller matrix.
	streams := got.Combinations[0].Streams
	if len(streams) != 3 {
		t.Fatalf("got %d streams, want 3", len(streams))
	}
	want := map[string]Outcome{
		"amd64-1.34": OutcomePassed, // settled, so skipped
		"arm64-1.34": OutcomeNone,   // errored, so still unknown
		"amd64-1.35": OutcomeNone,   // never ran
	}
	for _, s := range streams {
		if s.Skip != want[s.Name] {
			t.Errorf("%s skip = %q, want %q", s.Name, s.Skip, want[s.Name])
		}
	}
}

func TestPlanUsesQualifiedDependency(t *testing.T) {
	// rc.4 is higher but unqualified; rc.2 has passed. Tier 2 takes rc.2,
	// so a controller bump is held against something that was tested.
	p := newPlanner(t, controllerSpec, fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4", "v0.1.0-rc.2"},
		"cli":        {"v0.1.0-rc.7"},
	}, fakeHistory{qualified: map[string]bool{"os-image@v0.1.0-rc.2": true}})

	got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Combinations[0].Dependencies[0].Version; v != "v0.1.0-rc.2" {
		t.Errorf("os-image resolved to %q, want the qualified v0.1.0-rc.2", v)
	}
}

func TestPlanAbsentVersionTakesLatestCandidate(t *testing.T) {
	// v0.2.0 is higher, but a release has already been qualified on its way
	// to becoming one. The candidate is what there is to prove.
	p := newPlanner(t, controllerSpec, fakeTags{
		"controller": {"v0.2.0", "v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}, nil)

	got, err := p.Plan("controller", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v0.1.1-rc.8" {
		t.Errorf("got %q, want v0.1.1-rc.8", got.Version)
	}
}

func TestPlanNarrowsToComponentStreams(t *testing.T) {
	spec := controllerSpec + "  streams: [amd64-1.34, amd64-1.35]\n"
	p := newPlanner(t, spec, fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}, nil)

	got, err := p.Plan("controller", "v0.1.1-rc.8", "active")
	if err != nil {
		t.Fatal(err)
	}
	// The intersection of the component's two and the scope's two.
	streams := got.Combinations[0].Streams
	if len(streams) != 1 || streams[0].Name != "amd64-1.34" {
		t.Errorf("got %v, want just amd64-1.34", streams)
	}
}

func TestPlanRepeatedSelectorDoesNotDuplicate(t *testing.T) {
	// selectors is a list so it can widen later. Two `latest` entries still
	// resolve to one version, so the product must not double.
	spec := strings.Replace(controllerSpec,
		"    - name: cli\n      selectors: [latest]",
		"    - name: cli\n      selectors: [latest, latest]", 1)
	p := newPlanner(t, spec, fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}, nil)

	got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Combinations) != 1 {
		t.Errorf("got %d combinations, want 1", len(got.Combinations))
	}
}

func TestPlanFailsEarly(t *testing.T) {
	tags := fakeTags{
		"controller": {"v0.1.1-rc.8"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}

	for _, tc := range []struct {
		name      string
		spec      string
		component string
		version   string
		scope     string
		tags      fakeTags
		want      string
	}{
		{
			name: "component not in the policy", spec: controllerSpec,
			component: "agent", scope: "full", tags: tags,
			want: "is not a component in the qualification policy",
		},
		{
			name: "scope not in the policy", spec: controllerSpec,
			component: "controller", scope: "nightly", tags: tags,
			want: "is not a scope in the qualification policy",
		},
		{
			name: "unknown selector", component: "controller", scope: "full", tags: tags,
			spec: strings.Replace(controllerSpec, "selectors: [latest]", "selectors: [tip]", 1),
			want: "the only selector is",
		},
		{
			name: "stream the policy does not define", component: "controller", scope: "full", tags: tags,
			spec: controllerSpec + "  streams: [amd64-9.99]\n",
			want: "which the policy does not define",
		},
		{
			name: "constraint nothing satisfies", component: "controller", scope: "full", tags: tags,
			spec: strings.Replace(controllerSpec,
				"    - name: os-image\n      selectors: [latest]",
				"    - name: os-image\n      versionConstraint: \">=9.0.0\"\n      selectors: [latest]", 1),
			want: "no version of os-image satisfies",
		},
		{
			name: "dependency with no tags at all", spec: controllerSpec,
			component: "controller", scope: "full",
			tags: fakeTags{"controller": {"v0.1.1-rc.8"}, "cli": {"v0.1.0-rc.7"}},
			want: "os-image has no versions at all",
		},
		{
			name: "no candidate to qualify", spec: controllerSpec,
			component: "controller", scope: "full",
			tags: fakeTags{"controller": {"v0.1.0"}, "os-image": {"v0.1.0-rc.4"}, "cli": {"v0.1.0-rc.7"}},
			want: "has no release candidate to qualify",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t, tc.spec, tc.tags, nil)
			_, err := p.Plan(tc.component, tc.version, tc.scope)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestPlanChecksTheNamedCandidate(t *testing.T) {
	tags := fakeTags{
		"controller": {"v0.1.1-rc.8", "v0.1.0"},
		"os-image":   {"v0.1.0-rc.4"},
		"cli":        {"v0.1.0-rc.7"},
	}

	t.Run("a version that exists is accepted", func(t *testing.T) {
		p := newPlanner(t, controllerSpec, tags, nil)
		got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != "v0.1.1-rc.8" {
			t.Errorf("got %q", got.Version)
		}
	})

	t.Run("the leading v stays optional", func(t *testing.T) {
		p := newPlanner(t, controllerSpec, tags, nil)
		got, err := p.Plan("controller", "0.1.1-rc.8", "full")
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != "v0.1.1-rc.8" {
			t.Errorf("got %q", got.Version)
		}
	})

	// A release is a legitimate thing to re-qualify, so only existence is
	// required here, not candidate-ness.
	t.Run("a release is accepted", func(t *testing.T) {
		p := newPlanner(t, controllerSpec, tags, nil)
		if _, err := p.Plan("controller", "v0.1.0", "full"); err != nil {
			t.Fatal(err)
		}
	})

	for _, tc := range []struct{ name, version, want string }{
		{"not a version", "garbage", "is not a version"},
		// rc8 is legal semver -- a prerelease identifier need not be
		// numeric -- so this is caught by existence rather than by syntax,
		// and the message shows the correct spelling next to the wrong one.
		// release.yml enforces the dot when the tag is pushed; duplicating
		// that rule here would be a second place for it to drift.
		{"the missing rc dot", "v0.1.1-rc8", "has no version v0.1.1-rc8"},
		{"a version nobody cut", "v9.9.9", "has no version v9.9.9"},
		{"a near miss on the rc number", "v0.1.1-rc.9", "has no version v0.1.1-rc.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t, controllerSpec, tags, nil)
			_, err := p.Plan("controller", tc.version, "full")
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// The message has to say what does exist, or the reader has no next step.
	t.Run("the error names what is available", func(t *testing.T) {
		p := newPlanner(t, controllerSpec, tags, nil)
		_, err := p.Plan("controller", "v9.9.9", "full")
		if err == nil || !strings.Contains(err.Error(), "v0.1.1-rc.8") {
			t.Errorf("error %v does not name an available version", err)
		}
	})
}

func TestPlanIsDeterministic(t *testing.T) {
	build := func() *Plan {
		p := newPlanner(t, controllerSpec, fakeTags{
			"controller": {"v0.1.1-rc.8"},
			"os-image":   {"v0.1.0-rc.2", "v0.1.0-rc.4", "v0.1.0-rc.3"},
			"cli":        {"v0.1.0-rc.7"},
		}, nil)
		got, err := p.Plan("controller", "v0.1.1-rc.8", "full")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first, second := build(), build()
	if first.Combinations[0].ID != second.Combinations[0].ID {
		t.Error("two runs of the same inputs produced different combinations")
	}
	for i := range first.Combinations[0].Streams {
		if first.Combinations[0].Streams[i] != second.Combinations[0].Streams[i] {
			t.Error("two runs of the same inputs produced different stream order")
		}
	}
}
