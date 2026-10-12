// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// fake is a resolver that answers from a function and remembers what it was
// asked, so a test can see the inputs the planner built.
type fake struct {
	answer func(inputs map[string]string) (map[string]string, error)
	calls  []map[string]string
}

func (f *fake) Resolve(inputs map[string]string) (map[string]string, error) {
	f.calls = append(f.calls, maps.Clone(inputs))
	return f.answer(inputs)
}

// fixed always resolves to one version, read from one ref.
func fixed(version, ref string) *fake {
	return &fake{answer: func(map[string]string) (map[string]string, error) {
		return map[string]string{resolve.ResultVersionKey: version, resolve.ResultRefKey: ref}, nil
	}}
}

// byConstraint resolves to the constraint with a patch number on the end.
func byConstraint(patch string) *fake {
	return &fake{answer: func(in map[string]string) (map[string]string, error) {
		return map[string]string{resolve.ResultVersionKey: in[resolve.InputConstraintKey] + "." + patch}, nil
	}}
}

// subjectFake resolves the subject the way the git tag strategy does: the
// named version if there is one, else the highest version.
func subjectFake() *fake {
	return &fake{answer: func(in map[string]string) (map[string]string, error) {
		if v := in[resolve.InputOverrideKey]; v != "" {
			return map[string]string{resolve.ResultVersionKey: v}, nil
		}
		if in[resolve.InputSelectKey] != "" {
			return nil, fmt.Errorf("asked for a selection, the subject takes the default")
		}
		return map[string]string{resolve.ResultVersionKey: "v0.22.0-beta.1"}, nil
	}}
}

func testPolicy() *Policy {
	return &Policy{
		APIVersion: APIVersion,
		Kind:       KindQualificationPolicy,
		Spec: PolicySpec{
			Scopes: map[string]Scope{
				"full":   {},
				"active": {Selector: map[string]string{"kube": "1.34"}},
			},
			Streams: []StreamDefinition{
				{Name: "amd64-1.34", Capabilities: map[string]string{"arch": "amd64", "kube": "1.34"}},
				{Name: "arm64-1.34", Capabilities: map[string]string{"arch": "arm64", "kube": "1.34"}},
				{Name: "amd64-1.35", Capabilities: map[string]string{"arch": "amd64", "kube": "1.35"}},
			},
		},
	}
}

func testRegistry() *Registry {
	r := &Registry{APIVersion: APIVersion, Kind: KindRegistry}
	for _, name := range []string{"nvke", "os-image", "cli", "controller", "kube", "cni-ovn", "kind"} {
		r.Spec.Components = append(r.Spec.Components, Component{Name: name, Resolve: Resolve{Strategy: "fake"}})
	}
	return r
}

const testSubject = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Qualification
metadata:
  name: nvke
spec:
  subject: nvke
  components:
    - {name: os-image, selectors: [latest]}
    - {name: cli, selectors: [latest]}
  dependencies:
    - {name: kube, constraintFrom: {capability: kube}}
    - {name: cni-ovn, constraint: "1.4"}
    - {name: kind}
`

// resolvers is the set of fakes a planner is built from, kept by name so a test
// can read the calls each one received.
type resolvers map[string]*fake

func newResolvers() resolvers {
	return resolvers{
		"nvke":     subjectFake(),
		"os-image": fixed("v0.1.0", "os-image/v0.1.0"),
		"cli":      fixed("v0.1.0-rc.7", "cli/v0.1.0-rc.7"),
		"kube":     byConstraint("2"),
		"cni-ovn": {answer: func(in map[string]string) (map[string]string, error) {
			return map[string]string{resolve.ResultVersionKey: in[resolve.InputConstraintKey] + ".0"}, nil
		}},
		"kind": fixed("v0.32.0", ""),
	}
}

func newPlanner(t *testing.T, subject string, r resolvers) *Planner {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "release"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SubjectPath(root, SubjectNVKE), []byte(subject), 0o600); err != nil {
		t.Fatal(err)
	}

	rs := make(map[string]resolve.Resolver, len(r))
	for name, f := range r {
		rs[name] = f
	}
	return &Planner{Policy: testPolicy(), Registry: testRegistry(), Resolvers: rs, Root: root}
}

func versionsOf(es []ComponentVersion) string {
	parts := make([]string, len(es))
	for i, e := range es {
		// A location is what a run is handed, so it stands in for the version
		// where there is one.
		parts[i] = e.Name + "=" + cmpOr(e.URL, e.Version)
	}
	return strings.Join(parts, " ")
}

func TestPlanResolvesAndExpands(t *testing.T) {
	p := newPlanner(t, testSubject, newResolvers())

	got, err := p.Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}

	if got.Subject != "nvke" || got.Version != "v0.22.0-beta.1" || got.Scope != "full" {
		t.Errorf("plan is %s %s over %s", got.Subject, got.Version, got.Scope)
	}
	if len(got.Combinations) != 1 {
		t.Fatalf("got %d combinations, want 1", len(got.Combinations))
	}
	c := got.Combinations[0]

	// Components keep declaration order. The id is sorted, so it does not
	// depend on that order.
	if g := versionsOf(c.Components); g != "os-image=v0.1.0 cli=v0.1.0-rc.7" {
		t.Errorf("components are %q", g)
	}
	if c.ID != "cli-v0.1.0-rc.7_os-image-v0.1.0" {
		t.Errorf("id is %q", c.ID)
	}

	// A literal constraint and no constraint are the same for every stream, so
	// they are resolved once and recorded on the combination.
	if g := versionsOf(c.Dependencies); g != "cni-ovn=1.4.0 kind=v0.32.0" {
		t.Errorf("combination dependencies are %q", g)
	}

	// A constraint that names a capability is the stream's own value.
	want := map[string]string{
		"amd64-1.34": "kube=1.34.2",
		"arm64-1.34": "kube=1.34.2",
		"amd64-1.35": "kube=1.35.2",
	}
	if len(c.Streams) != 3 {
		t.Fatalf("full should select all three streams, got %d", len(c.Streams))
	}
	for _, s := range c.Streams {
		if g := versionsOf(s.Dependencies); g != want[s.Name] {
			t.Errorf("%s dependencies are %q, want %q", s.Name, g, want[s.Name])
		}
	}
}

// Whether a dependency is resolved once or once per stream follows from
// whether the subject binds it to a capability, and from nothing else.
func TestPlanResolvesPerStreamOnlyThroughACapability(t *testing.T) {
	r := newResolvers()
	p := newPlanner(t, testSubject, r)

	if _, err := p.Plan("nvke", "", "full"); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{"cni-ovn": 1, "kind": 1, "kube": 3} {
		if got := len(r[name].calls); got != want {
			t.Errorf("%s was resolved %d times, want %d", name, got, want)
		}
	}
}

// A strategy reads the components already resolved through plain inputs: each
// one's version under its name, and anything else its strategy returned under
// "<name>.<key>".
func TestPlanHandsLaterStrategiesWhatEarlierOnesResolved(t *testing.T) {
	r := newResolvers()
	p := newPlanner(t, testSubject, r)

	if _, err := p.Plan("nvke", "", "active"); err != nil {
		t.Fatal(err)
	}

	got := r["kube"].calls[0]
	want := map[string]string{
		resolve.InputConstraintKey: "1.34",
		"os-image":                 "v0.1.0",
		"os-image.ref":             "os-image/v0.1.0",
		"cli":                      "v0.1.0-rc.7",
		"cli.ref":                  "cli/v0.1.0-rc.7",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kube was given %v, want %v", got, want)
	}

	// kind has no constraint, so it is not handed one.
	if _, ok := r["kind"].calls[0][resolve.InputConstraintKey]; ok {
		t.Error("kind was handed a constraint it does not have")
	}
}

func TestPlanCarriesStreamCapabilities(t *testing.T) {
	p := newPlanner(t, testSubject, newResolvers())

	got, err := p.Plan("nvke", "", "active")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]map[string]string{
		"amd64-1.34": {"arch": "amd64", "kube": "1.34"},
		"arm64-1.34": {"arch": "arm64", "kube": "1.34"},
	}
	for _, s := range got.Combinations[0].Streams {
		if !reflect.DeepEqual(s.Capabilities, want[s.Name]) {
			t.Errorf("%s carries %v, want %v", s.Name, s.Capabilities, want[s.Name])
		}
	}
}

func TestPlanScopeNarrows(t *testing.T) {
	p := newPlanner(t, testSubject, newResolvers())

	got, err := p.Plan("nvke", "", "active")
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

func TestPlanNarrowsToSubjectStreams(t *testing.T) {
	p := newPlanner(t, testSubject+"  streams: [amd64-1.34, amd64-1.35]\n", newResolvers())

	got, err := p.Plan("nvke", "", "active")
	if err != nil {
		t.Fatal(err)
	}

	// The intersection of the subject's two and the scope's two.
	streams := got.Combinations[0].Streams
	if len(streams) != 1 || streams[0].Name != "amd64-1.34" {
		t.Errorf("got %v, want just amd64-1.34", streams)
	}
}

func TestPlanSubjectVersion(t *testing.T) {
	t.Run("absent means the highest version, as for any component", func(t *testing.T) {
		r := newResolvers()
		got, err := newPlanner(t, testSubject, r).Plan("nvke", "", "full")
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != "v0.22.0-beta.1" {
			t.Errorf("got %q", got.Version)
		}
		if in := r["nvke"].calls[0]; in[resolve.InputSelectKey] != "" || in[resolve.InputOverrideKey] != "" {
			t.Errorf("the subject was asked for %v", in)
		}
	})

	t.Run("a named version is passed to be checked", func(t *testing.T) {
		r := newResolvers()
		got, err := newPlanner(t, testSubject, r).Plan("nvke", "v0.22.0-beta.3", "full")
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != "v0.22.0-beta.3" {
			t.Errorf("got %q", got.Version)
		}
		if in := r["nvke"].calls[0]; in[resolve.InputOverrideKey] != "v0.22.0-beta.3" {
			t.Errorf("the subject was asked for %v", in)
		}
	})

	t.Run("a subject that cannot resolve is named", func(t *testing.T) {
		r := newResolvers()
		r["nvke"] = &fake{answer: func(map[string]string) (map[string]string, error) {
			return nil, fmt.Errorf("the repository has no tagged versions")
		}}
		_, err := newPlanner(t, testSubject, r).Plan("nvke", "", "full")
		wantError(t, err, "resolving nvke: the repository has no tagged versions")
	})

	t.Run("a named version that does not exist names both", func(t *testing.T) {
		r := newResolvers()
		r["nvke"] = &fake{answer: func(map[string]string) (map[string]string, error) {
			return nil, fmt.Errorf("the repository has no version v9.9.9")
		}}
		_, err := newPlanner(t, testSubject, r).Plan("nvke", "v9.9.9", "full")
		wantError(t, err, "checking nvke v9.9.9")
	})
}

func TestPlanRepeatedSelectorDoesNotDuplicate(t *testing.T) {
	// selectors is a list so it can widen later. Two `latest` entries still
	// resolve to one version, so the product must not double.
	subject := strings.Replace(testSubject, "{name: cli, selectors: [latest]}", "{name: cli, selectors: [latest, latest]}", 1)

	got, err := newPlanner(t, subject, newResolvers()).Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Combinations) != 1 {
		t.Errorf("got %d combinations, want 1", len(got.Combinations))
	}
}

// Selectors that resolve to different versions multiply into combinations,
// each with its own id. Nothing selects this way yet, so it is exercised with a
// resolver that answers differently the second time.
func TestPlanSelectorsThatDifferMakeCombinations(t *testing.T) {
	r := newResolvers()
	n := 0
	r["cli"] = &fake{answer: func(map[string]string) (map[string]string, error) {
		n++
		return map[string]string{resolve.ResultVersionKey: fmt.Sprintf("v0.1.%d", n)}, nil
	}}
	subject := strings.Replace(testSubject, "{name: cli, selectors: [latest]}", "{name: cli, selectors: [latest, latest]}", 1)

	got, err := newPlanner(t, subject, r).Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, c := range got.Combinations {
		ids = append(ids, c.ID)
	}
	want := []string{"cli-v0.1.1_os-image-v0.1.0", "cli-v0.1.2_os-image-v0.1.0"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids are %v, want %v", ids, want)
	}
}

func TestPlanFailsEarly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		scope   string
		change  func(resolvers)
		want    string
	}{
		{name: "scope not in the policy", subject: testSubject, scope: "nightly", want: "is not a scope in the qualification policy"},
		{
			name: "unknown selector", scope: "full",
			subject: strings.Replace(testSubject, "selectors: [latest]", "selectors: [tip]", 1),
			want:    "the only selector is",
		},
		{
			name: "stream the policy does not define", scope: "full",
			subject: testSubject + "  streams: [amd64-9.99]\n",
			want:    "which the policy does not define",
		},
		{
			name: "narrowing to streams the scope does not select", scope: "active",
			subject: testSubject + "  streams: [amd64-1.35]\n",
			want:    "narrows to streams the scope does not select",
		},
		{
			name: "a component that cannot resolve", subject: testSubject, scope: "full",
			change: func(r resolvers) {
				r["os-image"] = &fake{answer: func(map[string]string) (map[string]string, error) {
					return nil, fmt.Errorf("os-image has no tagged versions")
				}}
			},
			want: "resolving os-image for nvke: os-image has no tagged versions",
		},
		{
			name: "a dependency that cannot resolve", subject: testSubject, scope: "full",
			change: func(r resolvers) {
				r["cni-ovn"] = &fake{answer: func(map[string]string) (map[string]string, error) {
					return nil, fmt.Errorf("locks no version matching 1.4")
				}}
			},
			want: "resolving cni-ovn for nvke: locks no version matching 1.4",
		},
		{
			name: "a per-stream dependency that cannot resolve names the stream", subject: testSubject, scope: "full",
			change: func(r resolvers) {
				r["kube"] = &fake{answer: func(map[string]string) (map[string]string, error) {
					return nil, fmt.Errorf("locks no version matching 1.34")
				}}
			},
			want: "stream amd64-1.34: resolving kube for nvke: locks no version matching 1.34",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newResolvers()
			if tc.change != nil {
				tc.change(r)
			}
			_, err := newPlanner(t, tc.subject, r).Plan("nvke", "", tc.scope)
			wantError(t, err, tc.want)
		})
	}
}

func TestPlanNamesAComponentWithNoResolver(t *testing.T) {
	r := newResolvers()
	delete(r, "kind")

	_, err := newPlanner(t, testSubject, r).Plan("nvke", "", "full")
	wantError(t, err, `no resolver was built for "kind"`)
}

func TestPlanRejectsASubjectFileForAnotherSubject(t *testing.T) {
	subject := strings.NewReplacer("  name: nvke\n", "  name: controller\n", "subject: nvke", "subject: controller").Replace(testSubject)

	// The file sits where nvke's does, so it is read as nvke's and has to say so.
	_, err := newPlanner(t, subject, newResolvers()).Plan("nvke", "", "full")
	wantError(t, err, `describes "controller", not "nvke"`)
}

func TestPlanIsDeterministic(t *testing.T) {
	build := func() *Plan {
		got, err := newPlanner(t, testSubject, newResolvers()).Plan("nvke", "", "full")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first, second := build(), build()
	if !reflect.DeepEqual(first, second) {
		t.Error("two runs of the same inputs produced different plans")
	}
}

func TestCombinationID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		components []ComponentVersion
		want       string
	}{
		{
			"sorted by name",
			[]ComponentVersion{{Name: "os-image", Version: "v0.1.1-rc.1"}, {Name: "controller", Version: "v0.1.1-rc.11"}, {Name: "cli", Version: "v0.1.0-rc.7"}, {Name: "capi-provisioned", Version: "v0.1.0"}},
			"capi-provisioned-v0.1.0_cli-v0.1.0-rc.7_controller-v0.1.1-rc.11_os-image-v0.1.1-rc.1",
		},
		{
			"the same set in another order",
			[]ComponentVersion{{Name: "capi-provisioned", Version: "v0.1.0"}, {Name: "cli", Version: "v0.1.0-rc.7"}, {Name: "os-image", Version: "v0.1.1-rc.1"}, {Name: "controller", Version: "v0.1.1-rc.11"}},
			"capi-provisioned-v0.1.0_cli-v0.1.0-rc.7_controller-v0.1.1-rc.11_os-image-v0.1.1-rc.1",
		},
		{"build metadata is made path-safe", []ComponentVersion{{Name: "cli", Version: "v1.0.0+build.5"}}, "cli-v1.0.0-build.5"},
		{"slashes cannot reach a path", []ComponentVersion{{Name: "a/b", Version: "v1"}}, "a-b-v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := combinationID(tc.components); got != tc.want {
				t.Errorf("id = %q, want %q", got, tc.want)
			}
		})
	}
}

// Reordering the subject's component list does not change what is planned.
func TestPlanIDDoesNotDependOnComponentOrder(t *testing.T) {
	swapped := strings.Replace(testSubject,
		"    - {name: os-image, selectors: [latest]}\n    - {name: cli, selectors: [latest]}\n",
		"    - {name: cli, selectors: [latest]}\n    - {name: os-image, selectors: [latest]}\n", 1)
	if swapped == testSubject {
		t.Fatal("the subject was not reordered")
	}

	a, err := newPlanner(t, testSubject, newResolvers()).Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPlanner(t, swapped, newResolvers()).Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if a.Combinations[0].ID != b.Combinations[0].ID {
		t.Errorf("ids differ: %q and %q", a.Combinations[0].ID, b.Combinations[0].ID)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
