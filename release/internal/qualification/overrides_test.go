// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/release/internal/resolve"
)

var errNotLocked = errors.New("1.36.2 is not locked")

func TestParseOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string
	}{
		{"nothing is not an error", "", map[string]string{}},
		{"one pair", "controller=v0.1.1-rc.12", map[string]string{"controller": "v0.1.1-rc.12"}},
		{
			"several pairs",
			"controller=v0.1.1-rc.12,kube=1.36.2",
			map[string]string{"controller": "v0.1.1-rc.12", "kube": "1.36.2"},
		},
		{"whitespace is trimmed", " kube = 1.36.2 ", map[string]string{"kube": "1.36.2"}},
		{"only the first equals splits", "os-image=https://h/p?a=b", map[string]string{"os-image": "https://h/p?a=b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOverrides(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseOverridesRejectsMalformedPairs(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no equals", "kube", `"kube" is not name=version`},
		{"empty value", "kube=", `"kube=" is not name=version`},
		{"empty name", "=1.36.2", `"=1.36.2" is not name=version`},
		{"a trailing comma", "kube=1.36.2,", `"" is not name=version`},
		{"a duplicate", "kube=1.36.2,kube=1.36.3", `"kube" appears twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOverrides(tc.in)
			wantError(t, err, tc.want)
		})
	}
}

func TestPlanOverridesReplaceWhatResolves(t *testing.T) {
	r := newResolvers()
	// The overridden component reports its own ref, as a git tag strategy does,
	// so a later strategy can be seen reading it.
	r["os-image"] = &fake{answer: func(in map[string]string) (map[string]string, error) {
		v := in[resolve.InputOverrideKey]
		if v == "" {
			v = "v0.1.0"
		}
		return map[string]string{resolve.ResultVersionKey: v, resolve.ResultRefKey: "os-image/" + v}, nil
	}}
	// Strategies return a version they were asked for, as the real ones do.
	asked := func(otherwise string) *fake {
		return &fake{answer: func(in map[string]string) (map[string]string, error) {
			if v := in[resolve.InputOverrideKey]; v != "" {
				return map[string]string{resolve.ResultVersionKey: v}, nil
			}
			return map[string]string{resolve.ResultVersionKey: otherwise}, nil
		}}
	}
	r["kube"] = asked("1.34.9")
	r["kind"] = asked("v0.32.0")
	p := newPlanner(t, testSubject, r)
	p.Overrides = map[string]string{"os-image": "v0.1.0-rc.3", "kube": "1.34.2", "kind": "v0.33.0"}

	got, err := p.Plan("nke", "", "active")
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got.Overrides, []string{"kind", "kube", "os-image"}) {
		t.Errorf("overrides are %v, want the three names in order", got.Overrides)
	}

	c := got.Combinations[0]
	if g := versionsOf(c.Components); g != "os-image=v0.1.0-rc.3 cli=v0.1.0-rc.7" {
		t.Errorf("components are %q", g)
	}
	if c.ID != "cli-v0.1.0-rc.7_os-image-v0.1.0-rc.3" {
		t.Errorf("id is %q, which should carry the overridden version", c.ID)
	}
	if g := versionsOf(c.Dependencies); g != "cni-ovn=1.4.0 kind=v0.33.0" {
		t.Errorf("combination dependencies are %q", g)
	}
	for _, s := range c.Streams {
		if g := versionsOf(s.Dependencies); g != "kube=1.34.2" {
			t.Errorf("%s dependencies are %q, want the override on every stream", s.Name, g)
		}
	}

	// The override goes to the resolver as the version asked for, so the
	// strategy can check it, and what the overridden component returned is what
	// later strategies read.
	if in := r["os-image"].calls[0]; in[resolve.InputOverrideKey] != "v0.1.0-rc.3" {
		t.Errorf("os-image was given %v", in)
	}
	if in := r["kube"].calls[0]; in[resolve.InputOverrideKey] != "1.34.2" || in["os-image.ref"] != "os-image/v0.1.0-rc.3" {
		t.Errorf("kube was given %v", in)
	}
	if in := r["cni-ovn"].calls[0]; in[resolve.InputOverrideKey] != "" {
		t.Errorf("cni-ovn was not overridden but was given %v", in)
	}
}

func TestPlanWithoutOverridesHasNone(t *testing.T) {
	got, err := newPlanner(t, testSubject, newResolvers()).Plan("nke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if got.Overrides != nil {
		t.Errorf("overrides are %v, want none", got.Overrides)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "overrides") {
		t.Errorf("a plan with no overrides should not carry the field: %s", b)
	}
}

func TestPlanRejectsOverridesTheSubjectDoesNotList(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			"an unknown name",
			map[string]string{"helm": "v4.0.0"},
			`nke has no component or dependency "helm", it lists os-image, cli, kube, cni-ovn, kind`,
		},
		{"the subject itself", map[string]string{"nke": "v0.22.0-beta.1"}, "nke is the subject, so its version is named with --version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t, testSubject, newResolvers())
			p.Overrides = tc.overrides
			_, err := p.Plan("nke", "", "full")
			wantError(t, err, tc.want)
		})
	}
}

// A resolver that refuses an override stops the plan, with the stream when the
// dependency is resolved per stream.
func TestPlanSurfacesAnOverrideTheStrategyRefuses(t *testing.T) {
	r := newResolvers()
	r["kube"] = &fake{answer: func(map[string]string) (map[string]string, error) {
		return nil, errNotLocked
	}}
	p := newPlanner(t, testSubject, r)
	p.Overrides = map[string]string{"kube": "1.36.2"}

	_, err := p.Plan("nke", "", "active")
	wantError(t, err, "stream amd64-1.34: resolving kube for nke: 1.36.2 is not locked")
}
