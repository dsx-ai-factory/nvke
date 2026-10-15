// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// imageSubject lists the os-image artifacts as dependencies. The images depend on
// the stream's ubuntu series, so they are per stream, and the bootloaders depend
// on nothing, so they are per combination. The anchor states the releases once.
const imageSubject = `
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
    - {name: os-image-amd64, constraintFrom: {capability: ubuntu}, releases: &ubuntu ["24.04.5", "24.04.10", "26.04.1"]}
    - {name: os-image-arm64, constraintFrom: {capability: ubuntu}, releases: *ubuntu}
    - {name: os-image-bootloader-amd64, value: efi/x86_64/qcow-imager.efi}
    - {name: os-image-bootloader-arm64, value: efi/aarch64/qcow-imager.efi}
`

// imagePolicy has two series, so a stream's ubuntu capability chooses the point
// release and the two streams differ in it.
func imagePolicy() *Policy {
	p := testPolicy()
	p.Spec.Streams = []StreamDefinition{
		{Name: "amd64-24", Capabilities: map[string]string{"arch": "amd64", "kube": "1.34", "ubuntu": "24.04"}},
		{Name: "arm64-26", Capabilities: map[string]string{"arch": "arm64", "kube": "1.34", "ubuntu": "26.04"}},
	}
	return p
}

func imageRegistry() *Registry {
	r := testRegistry()
	image := func(arch string) Resolve {
		return Resolve{Strategy: resolve.StrategyOSImage, With: map[string]string{
			"base": "https://cdn.example", "from": "os-image", "arch": arch, "variant": "non-fde",
		}}
	}
	bootloader := Resolve{Strategy: resolve.StrategyURL, With: map[string]string{"base": "https://cdn.example"}}
	r.Spec.Components = append(r.Spec.Components,
		Component{Name: "os-image-amd64", Resolve: image("amd64")},
		Component{Name: "os-image-arm64", Resolve: image("arm64")},
		Component{Name: "os-image-bootloader-amd64", Resolve: bootloader},
		Component{Name: "os-image-bootloader-arm64", Resolve: bootloader},
	)
	return r
}

// strategies builds the real resolver for each registry component whose strategy
// validates its inputs, which is what Check asks. The planner tests use fakes, so
// these are only for the tests of what Check holds a subject to.
func strategies(t *testing.T, r *Registry) map[string]resolve.Resolver {
	t.Helper()
	out := map[string]resolve.Resolver{}
	for _, c := range r.Spec.Components {
		if c.Resolve.Strategy == "fake" {
			continue
		}
		built, err := resolve.New(c.Resolve.Strategy, r.StrategyConfig(c), ".")
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		out[c.Name] = built
	}
	return out
}

// imageResolvers stands in for the strategies. An image answers with the release
// tag as its version and a URL naming the tag, the point release the series picks
// from the releases it is given, and its architecture. A bootloader answers with
// its value as a URL and no version. Both take an override as the whole URL.
func imageResolvers() resolvers {
	r := newResolvers()
	image := func(arch string) *fake {
		return &fake{answer: func(in map[string]string) (map[string]string, error) {
			if v := in[resolve.InputOverrideKey]; v != "" {
				return map[string]string{resolve.ResultURLKey: v}, nil
			}
			release, ok := resolve.HighestMatching(strings.Split(in[resolve.InputReleasesKey], ","), in[resolve.InputConstraintKey])
			if !ok {
				return nil, fmt.Errorf("no release matches %s", in[resolve.InputConstraintKey])
			}
			return map[string]string{
				resolve.ResultVersionKey: in["os-image"],
				resolve.ResultURLKey:     fmt.Sprintf("https://cdn.example/%s/%s/%s", in["os-image"], release, arch),
			}, nil
		}}
	}
	bootloader := func() *fake {
		return &fake{answer: func(in map[string]string) (map[string]string, error) {
			if v := in[resolve.InputOverrideKey]; v != "" {
				return map[string]string{resolve.ResultURLKey: v}, nil
			}
			return map[string]string{resolve.ResultURLKey: "https://cdn.example/" + in[resolve.InputValueKey]}, nil
		}}
	}
	r["os-image-amd64"] = image("amd64")
	r["os-image-arm64"] = image("arm64")
	r["os-image-bootloader-amd64"] = bootloader()
	r["os-image-bootloader-arm64"] = bootloader()
	return r
}

func imagePlanner(t *testing.T, r resolvers) *Planner {
	t.Helper()
	p := newPlanner(t, imageSubject, r)
	p.Policy = imagePolicy()
	p.Registry = imageRegistry()
	return p
}

func TestLoadSubjectAnchorsAndReleases(t *testing.T) {
	s, err := LoadSubject(write(t, "qualification.yaml", imageSubject))
	if err != nil {
		t.Fatalf("an anchor and its alias should load under strict decoding: %v", err)
	}

	amd64, arm64, bootloader := s.Spec.Dependencies[0], s.Spec.Dependencies[1], s.Spec.Dependencies[2]
	want := []string{"24.04.5", "24.04.10", "26.04.1"}
	if amd64.ConstraintFrom.Capability != "ubuntu" || !reflect.DeepEqual(amd64.Releases, want) {
		t.Errorf("os-image-amd64 takes %+v", amd64)
	}
	if !reflect.DeepEqual(arm64.Releases, amd64.Releases) {
		t.Errorf("the alias should give the same releases, got %v", arm64.Releases)
	}
	if !amd64.perStream() || bootloader.perStream() {
		t.Error("an image should be per stream and a bootloader per combination")
	}
	if bootloader.Value != "efi/x86_64/qcow-imager.efi" {
		t.Errorf("the bootloader's value is %q", bootloader.Value)
	}
}

func TestPlanResolvesImagesPerStreamAndBootloadersPerCombination(t *testing.T) {
	r := imageResolvers()
	got, err := imagePlanner(t, r).Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	c := got.Combinations[0]

	// The bootloaders are the same for every stream, so they are on the
	// combination, once, and carry a location and no version.
	if g := versionsOf(c.Dependencies); !strings.Contains(g, "os-image-bootloader-amd64=https://cdn.example/efi/x86_64/qcow-imager.efi") ||
		!strings.Contains(g, "os-image-bootloader-arm64=https://cdn.example/efi/aarch64/qcow-imager.efi") {
		t.Errorf("combination dependencies are %q", g)
	}
	for _, d := range c.Dependencies {
		if strings.HasPrefix(d.Name, "os-image-bootloader") && d.Version != "" {
			t.Errorf("%s has version %q, a bootloader has only a location", d.Name, d.Version)
		}
	}
	if len(r["os-image-bootloader-amd64"].calls) != 1 {
		t.Errorf("a bootloader was resolved %d times, want once", len(r["os-image-bootloader-amd64"].calls))
	}

	// An image depends on the stream's series, which picks the point release
	// from the list, so each stream gets the one for its own series, for both
	// architectures. The highest release of the series is chosen, numerically.
	for _, s := range c.Streams {
		release := map[string]string{"amd64-24": "24.04.10", "arm64-26": "26.04.1"}[s.Name]
		want := fmt.Sprintf("os-image-amd64=https://cdn.example/v0.1.0/%s/amd64 os-image-arm64=https://cdn.example/v0.1.0/%s/arm64", release, release)
		images := s.Dependencies[len(s.Dependencies)-2:]
		if g := versionsOf(images); g != want {
			t.Errorf("%s images are %q, want %q", s.Name, g, want)
		}
		for _, d := range images {
			if d.Version != "v0.1.0" {
				t.Errorf("%s in %s has version %q, want the release tag", d.Name, s.Name, d.Version)
			}
		}
	}
	if len(r["os-image-amd64"].calls) != 2 {
		t.Errorf("an image was resolved %d times, want once for each stream", len(r["os-image-amd64"].calls))
	}
}

// A strategy is given the inputs its dependency has and no others.
func TestPlanGivesEachStrategyItsInputs(t *testing.T) {
	r := imageResolvers()
	if _, err := imagePlanner(t, r).Plan("nvke", "", "full"); err != nil {
		t.Fatal(err)
	}

	image := r["os-image-amd64"].calls[0]
	if image[resolve.InputConstraintKey] != "24.04" || image[resolve.InputReleasesKey] != "24.04.5,24.04.10,26.04.1" {
		t.Errorf("an image was given %v, want the series and the releases", image)
	}
	if _, ok := image[resolve.InputValueKey]; ok {
		t.Errorf("an image was given a value: %v", image)
	}

	bootloader := r["os-image-bootloader-amd64"].calls[0]
	if bootloader[resolve.InputValueKey] != "efi/x86_64/qcow-imager.efi" {
		t.Errorf("a bootloader was given %v, want its value", bootloader)
	}
	for _, k := range []string{resolve.InputConstraintKey, resolve.InputReleasesKey} {
		if _, ok := bootloader[k]; ok {
			t.Errorf("a bootloader was given %s: %v", k, bootloader)
		}
	}
}

// An image or a bootloader is a dependency, so it is overridden by name like any
// other, with a URL. The override carries no release, so the entry has no version.
func TestPlanOverridesAnImageByName(t *testing.T) {
	r := imageResolvers()
	p := imagePlanner(t, r)
	p.Overrides = map[string]string{"os-image-arm64": "https://mine.example/custom.qcow2"}

	got, err := p.Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Overrides, []string{"os-image-arm64"}) {
		t.Errorf("overrides are %v", got.Overrides)
	}
	for _, s := range got.Combinations[0].Streams {
		g := versionsOf(s.Dependencies)
		if !strings.Contains(g, "os-image-arm64=https://mine.example/custom.qcow2") {
			t.Errorf("%s did not take the override: %q", s.Name, g)
		}
		if strings.Contains(g, "os-image-amd64=https://mine.example") {
			t.Errorf("%s overrode the amd64 image too: %q", s.Name, g)
		}
	}
}

func TestCheckRejectsAStreamNoReleaseMatches(t *testing.T) {
	p := imagePolicy()
	p.Spec.Streams[1].Capabilities["ubuntu"] = "22.04"
	s, err := LoadSubject(write(t, "qualification.yaml", imageSubject))
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, Check(p, imageRegistry(), s, strategies(t, imageRegistry())), `stream "arm64-26": none of the releases 24.04.5, 24.04.10, 26.04.1 matches Ubuntu 22.04`)
}

func TestCheckReleasesNeedACapabilityEveryStreamHas(t *testing.T) {
	p := imagePolicy()
	delete(p.Spec.Streams[0].Capabilities, "ubuntu")
	s, err := LoadSubject(write(t, "qualification.yaml", imageSubject))
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, Check(p, imageRegistry(), s, strategies(t, imageRegistry())), `stream amd64-24 does not declare capability "ubuntu"`)
}

// A literal constraint is held to the releases too, since it would fail at plan
// time for every combination.
func TestCheckRejectsALiteralNoReleaseMatches(t *testing.T) {
	bad := strings.Replace(imageSubject,
		`{name: os-image-arm64, constraintFrom: {capability: ubuntu}, releases: *ubuntu}`,
		`{name: os-image-arm64, constraint: "22.04", releases: *ubuntu}`, 1)
	s, err := LoadSubject(write(t, "qualification.yaml", bad))
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, Check(imagePolicy(), imageRegistry(), s, strategies(t, imageRegistry())), `none of the releases 24.04.5, 24.04.10, 26.04.1 matches Ubuntu 22.04`)
}

// Each strategy takes the inputs it takes. One handed an input it ignores would
// resolve by a rule nobody wrote, and one missing an input it needs would only
// fail when a plan is made.
func TestCheckHoldsInputsToTheStrategy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(string) string
		want   string
	}{
		{
			"releases on a strategy that reads its versions",
			func(s string) string {
				return strings.Replace(s, "{name: os-image-bootloader-amd64, value: efi/x86_64/qcow-imager.efi}",
					`{name: os-image-bootloader-amd64, releases: ["1.0"]}`, 1)
			},
			"takes no releases",
		},
		{
			"a value on an image",
			func(s string) string {
				return strings.Replace(s, "{name: os-image-arm64, constraintFrom: {capability: ubuntu}, releases: *ubuntu}",
					"{name: os-image-arm64, value: x}", 1)
			},
			"takes no value",
		},
		{
			"an image with no releases",
			func(s string) string {
				return strings.Replace(s, "{name: os-image-arm64, constraintFrom: {capability: ubuntu}, releases: *ubuntu}",
					"{name: os-image-arm64, constraintFrom: {capability: ubuntu}}", 1)
			},
			"needs releases",
		},
		{
			"an image with no constraint",
			func(s string) string {
				return strings.Replace(s, "{name: os-image-arm64, constraintFrom: {capability: ubuntu}, releases: *ubuntu}",
					"{name: os-image-arm64, releases: *ubuntu}", 1)
			},
			"needs constraint",
		},
		{
			"a bootloader with no value",
			func(s string) string {
				return strings.Replace(s, "{name: os-image-bootloader-amd64, value: efi/x86_64/qcow-imager.efi}",
					"{name: os-image-bootloader-amd64}", 1)
			},
			"needs value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := LoadSubject(write(t, "qualification.yaml", tc.change(imageSubject)))
			if err != nil {
				t.Fatal(err)
			}
			wantError(t, Check(imagePolicy(), imageRegistry(), s, strategies(t, imageRegistry())), tc.want)
		})
	}

	t.Run("a locked package with no constraint", func(t *testing.T) {
		s, err := LoadSubject(write(t, "qualification.yaml", imageSubject))
		if err != nil {
			t.Fatal(err)
		}
		s.Spec.Dependencies = append(s.Spec.Dependencies, SubjectDependency{Name: "kube"})
		r := imageRegistry()
		for i := range r.Spec.Components {
			if r.Spec.Components[i].Name == "kube" {
				r.Spec.Components[i].Resolve = Resolve{
					Strategy: resolve.StrategyLockedPackage,
					With:     map[string]string{"lockDir": "os-image/packages/kubeadm-distro/lock", "from": "os-image"},
				}
			}
		}
		wantError(t, Check(imagePolicy(), r, s, strategies(t, r)), "needs constraint")
	})
}
