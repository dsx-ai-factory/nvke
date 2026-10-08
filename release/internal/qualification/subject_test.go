// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const validSubject = `
apiVersion: nke.nvidia.com/v1alpha1
kind: Qualification
metadata:
  name: nke
spec:
  subject: nke
  components:
    - {name: controller, selectors: [latest]}
    - {name: cli, selectors: [latest]}
  dependencies:
    - {name: kube, constraint: {capability: kube}}
    - {name: nke-cni-ovn, constraint: "1.4"}
    - {name: kind}
`

func TestLoadSubjectAccepts(t *testing.T) {
	s, err := LoadSubject(write(t, "qualification.yaml", validSubject))
	if err != nil {
		t.Fatal(err)
	}

	if got := len(s.Spec.Components); got != 2 {
		t.Errorf("got %d components, want 2", got)
	}

	kube, ovn, kind := s.Spec.Dependencies[0], s.Spec.Dependencies[1], s.Spec.Dependencies[2]
	if kube.Constraint == nil || kube.Constraint.Capability != "kube" || !kube.perStream() {
		t.Errorf("kube takes %+v, want the kube capability", kube.Constraint)
	}
	if ovn.Constraint == nil || ovn.Constraint.Literal != "1.4" || ovn.perStream() {
		t.Errorf("ovn takes %+v, want the literal 1.4", ovn.Constraint)
	}
	if kind.Constraint != nil || kind.perStream() {
		t.Errorf("kind takes %+v, want no constraint", kind.Constraint)
	}
}

func TestLoadSubjectRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"wrong kind", strings.Replace(validSubject, "kind: Qualification", "kind: ComponentQualification", 1), "kind"},
		{"unknown field", strings.Replace(validSubject, "  subject: nke", "  subjekt: nke", 1), "unknown field"},
		{"the old component field", strings.Replace(validSubject, "  subject: nke", "  component: nke", 1), "unknown field"},
		{"no subject", strings.Replace(validSubject, "  subject: nke\n", "", 1), "spec.subject is empty"},
		{
			"metadata disagrees with spec",
			strings.Replace(validSubject, "  name: nke", "  name: cli", 1),
			"but spec.subject is",
		},
		{
			"selectors belong to components only",
			strings.Replace(validSubject, "    - {name: kind}", "    - {name: kind, selectors: [latest]}", 1),
			"unknown field",
		},
		{
			"component with no selectors",
			strings.Replace(validSubject, "{name: cli, selectors: [latest]}", "{name: cli, selectors: []}", 1),
			"has no selectors",
		},
		{
			"lists itself",
			strings.Replace(validSubject, "{name: cli, selectors", "{name: nke, selectors", 1),
			"lists itself",
		},
		{
			"listed twice across both lists",
			strings.Replace(validSubject, "    - {name: kind}", "    - {name: cli}", 1),
			"listed twice",
		},
		{
			"no components",
			"apiVersion: nke.nvidia.com/v1alpha1\nkind: Qualification\nspec:\n  subject: nke\n  components: []\n",
			"the combination has no key",
		},
		{
			"constraint naming no capability",
			strings.Replace(validSubject, "{capability: kube}", "{capability: \"\"}", 1),
			"names no capability",
		},
		{
			"constraint with another key",
			strings.Replace(validSubject, "{capability: kube}", "{cap: kube}", 1),
			"a string or {capability: <name>}",
		},
		{
			"empty literal constraint",
			strings.Replace(validSubject, `constraint: "1.4"`, `constraint: ""`, 1),
			"a constraint is empty",
		},
		{
			"constraint that is a number",
			strings.Replace(validSubject, `constraint: "1.4"`, `constraint: 1.4`, 1),
			"a string or {capability: <name>}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSubject(write(t, "qualification.yaml", tc.body))
			wantError(t, err, tc.want)
		})
	}
}

// A subject written back out reads back as the same subject, so a record that
// carries one is not changed by passing through JSON.
func TestConstraintRoundTrips(t *testing.T) {
	for _, c := range []Constraint{{Literal: "1.4"}, {Capability: "kube"}} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var got Constraint
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if got != c {
			t.Errorf("%s read back as %+v, want %+v", b, got, c)
		}
	}
}

func TestSubjectPath(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"nke", filepath.Join("root", "release", "qualification.yaml")},
		{"controller", filepath.Join("root", "controller", "qualification.yaml")},
	} {
		if got := SubjectPath("root", tc.subject); got != tc.want {
			t.Errorf("SubjectPath(%q) = %q, want %q", tc.subject, got, tc.want)
		}
	}
}
