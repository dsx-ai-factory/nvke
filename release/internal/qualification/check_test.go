// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		want    string
	}{
		{name: "a consistent set passes", subject: testSubject},
		{
			name:    "the subject is not registered",
			subject: strings.NewReplacer("  name: nvke", "  name: agent", "subject: nvke", "subject: agent").Replace(testSubject),
			want:    `subject "agent" is not a component in the registry`,
		},
		{
			name:    "a component is not registered",
			subject: strings.Replace(testSubject, "{name: cli, selectors", "{name: agent, selectors", 1),
			want:    `lists "agent", which is not a component in the registry`,
		},
		{
			name:    "a dependency is not registered",
			subject: strings.Replace(testSubject, "{name: kind}", "{name: helm}", 1),
			want:    `depends on "helm", which is not a component in the registry`,
		},
		{
			name:    "an unknown selector",
			subject: strings.Replace(testSubject, "selectors: [latest]", "selectors: [tip]", 1),
			want:    "the only selector is",
		},
		{
			name:    "a capability a stream lacks",
			subject: strings.Replace(testSubject, "{capability: kube}", "{capability: ubuntu}", 1),
			want:    `stream amd64-1.34 does not declare capability "ubuntu"`,
		},
		{
			name:    "a stream the policy does not define",
			subject: testSubject + "  streams: [amd64-9.99]\n",
			want:    "which the policy does not define",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := LoadSubject(write(t, "qualification.yaml", tc.subject))
			if err != nil {
				t.Fatal(err)
			}
			err = Check(testPolicy(), testRegistry(), s, nil, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantError(t, err, tc.want)
		})
	}
}

// A version can be taken only from a component. Components resolve first and
// every one is offered to every dependency, which a dependency's own result is
// not, so a dependency naming another would borrow nothing.
func TestCheckRejectsAVersionTakenFromADependency(t *testing.T) {
	s, err := LoadSubject(write(t, "qualification.yaml", testSubject))
	if err != nil {
		t.Fatal(err)
	}
	s.Spec.Dependencies = append(s.Spec.Dependencies, SubjectDependency{Name: "cni-ovn"})

	r := testRegistry()
	for i := range r.Spec.Components {
		if r.Spec.Components[i].Name == "cni-ovn" {
			r.Spec.Components[i].Resolve = Resolve{
				Version:  &Rule{From: "kube"},
				Location: &Rule{Strategy: "fake"},
			}
		}
	}
	wantError(t, Check(testPolicy(), r, s, nil, nil),
		`takes its version from "kube", which the subject does not list as a component`)
}

// A component is resolved before anything it could take a version from, so one
// that tries is a registry error rather than an empty version at plan time.
func TestCheckRejectsAComponentTakingAVersion(t *testing.T) {
	s, err := LoadSubject(write(t, "qualification.yaml", testSubject))
	if err != nil {
		t.Fatal(err)
	}

	r := testRegistry()
	for i := range r.Spec.Components {
		if r.Spec.Components[i].Name == "cli" {
			r.Spec.Components[i].Resolve = Resolve{Version: &Rule{From: "controller"}}
		}
	}
	wantError(t, Check(testPolicy(), r, s, nil, nil),
		`it takes its version from "controller", and a component is resolved before`)
}

// A component that resolves only a location has no version to give, which is a
// different mistake from naming something the subject does not list, and the
// error says which.
func TestCheckRejectsAVersionTakenFromALocationOnlyComponent(t *testing.T) {
	s, err := LoadSubject(write(t, "qualification.yaml", testSubject))
	if err != nil {
		t.Fatal(err)
	}
	s.Spec.Dependencies = append(s.Spec.Dependencies, SubjectDependency{Name: "cni-ovn"})

	r := testRegistry()
	for i := range r.Spec.Components {
		switch r.Spec.Components[i].Name {
		case "cli":
			r.Spec.Components[i].Resolve = Resolve{Location: &Rule{Strategy: "fake"}}
		case "cni-ovn":
			r.Spec.Components[i].Resolve = Resolve{
				Version:  &Rule{From: "cli"},
				Location: &Rule{Strategy: "fake"},
			}
		}
	}
	wantError(t, Check(testPolicy(), r, s, nil, nil),
		`takes its version from "cli", which resolves only a location, so it has no version to give`)
}
