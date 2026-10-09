// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dsx-ai-factory/nke/release/internal/resolve"
)

// SubjectNKE is the subject that stands for the release as a whole. Its
// directory is the repository root, so its file sits beside the policy.
const SubjectNKE = "nke"

// Subject is a qualification.yaml: what one qualification moves together and
// what is resolved around it. The file at release/qualification.yaml is the
// subject for the release as a whole, and a component may carry its own
// beside its code.
type Subject struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata,omitempty"`
	Spec       SubjectSpec `json:"spec"`
}

// SubjectSpec is the body of a Qualification.
type SubjectSpec struct {
	// Subject names what is being qualified. Its version is the plan's
	// version.
	Subject string `json:"subject"`

	// Components move together in this qualification, and the combination is
	// keyed by them.
	Components []SubjectComponent `json:"components"`

	// Dependencies are resolved around the components, either once for the
	// combination or once per stream.
	Dependencies []SubjectDependency `json:"dependencies,omitempty"`

	// Streams narrows this subject to a subset of the scope's streams. Empty
	// means every stream the scope selects, which is the common case.
	Streams []string `json:"streams,omitempty"`
}

// SubjectComponent is one component the combination is keyed by, and how to
// pick its version.
type SubjectComponent struct {
	Name string `json:"name"`

	// Selectors is a list so that qualifying against the last N qualified
	// versions of a component stays a widening of what a selector expresses
	// rather than a redesign. Today the only selector is `latest`.
	Selectors []string `json:"selectors"`
}

// SubjectDependency is one component resolved around the combination. Its
// fields are the inputs its strategy is given, each with one meaning.
type SubjectDependency struct {
	Name string `json:"name"`

	// Constraint is a literal version constraint, such as a minor "1.4". Empty
	// means the strategy decides alone.
	Constraint string `json:"constraint,omitempty"`

	// ConstraintFrom takes the constraint from the stream instead. It makes the
	// dependency per stream, because its value is the stream's.
	ConstraintFrom *ConstraintFrom `json:"constraintFrom,omitempty"`

	// Releases are the versions a strategy that cannot list them itself may
	// choose from, such as the Ubuntu point releases an image is published for.
	// The strategy picks the highest one the constraint matches.
	Releases []string `json:"releases,omitempty"`

	// Value is a declared answer that nothing derives, such as the path of a
	// bootloader.
	Value string `json:"value,omitempty"`
}

// ConstraintFrom names the stream capability a constraint is taken from.
type ConstraintFrom struct {
	Capability string `json:"capability"`
}

// releasePattern is a dotted version of numbers, such as 24.04.5.
var releasePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

func (d SubjectDependency) validate(path string) error {
	switch {
	case d.Constraint != "" && d.ConstraintFrom != nil:
		return fmt.Errorf("%s: dependency %q has both constraint and constraintFrom", path, d.Name)
	case d.ConstraintFrom != nil && d.ConstraintFrom.Capability == "":
		return fmt.Errorf("%s: dependency %q has a constraintFrom that names no capability", path, d.Name)
	case d.Value != "" && (d.Constraint != "" || d.ConstraintFrom != nil || len(d.Releases) > 0):
		return fmt.Errorf("%s: dependency %q has a value, which is an answer and takes no constraint or releases", path, d.Name)
	}
	for _, r := range d.Releases {
		if !releasePattern.MatchString(r) {
			return fmt.Errorf("%s: dependency %q has release %q, which is not a dotted version such as 24.04.5", path, d.Name, r)
		}
	}
	return nil
}

// perStream reports whether the dependency's inputs come from a stream.
func (d SubjectDependency) perStream() bool { return d.ConstraintFrom != nil }

// inputs are what the dependency declares, as the plain values its strategy is
// given, for one stream. A literal ignores the stream, and a constraintFrom is
// the stream's value of the capability it names. A dependency that declares
// nothing has no inputs.
func (d SubjectDependency) inputs(stream *StreamDefinition) (map[string]string, error) {
	in := map[string]string{}
	switch {
	case d.ConstraintFrom != nil && stream == nil:
		return nil, fmt.Errorf("constraint comes from capability %q but there is no stream", d.ConstraintFrom.Capability)
	case d.ConstraintFrom != nil:
		v, ok := stream.Capabilities[d.ConstraintFrom.Capability]
		if !ok {
			return nil, fmt.Errorf("stream %s does not declare capability %q", stream.Name, d.ConstraintFrom.Capability)
		}
		in[resolve.InputConstraintKey] = v
	case d.Constraint != "":
		in[resolve.InputConstraintKey] = d.Constraint
	}
	if len(d.Releases) > 0 {
		in[resolve.InputReleasesKey] = strings.Join(d.Releases, ",")
	}
	if d.Value != "" {
		in[resolve.InputValueKey] = d.Value
	}
	return in, nil
}

// SubjectPath is where a subject's file lives. The release as a whole sits
// beside the policy, and a component's sits in its own directory.
func SubjectPath(root, subject string) string {
	if subject == SubjectNKE {
		return filepath.Join(root, "release", "qualification.yaml")
	}
	return filepath.Join(root, subject, "qualification.yaml")
}

// LoadSubject reads and validates a qualification.yaml.
func LoadSubject(path string) (*Subject, error) {
	var s Subject
	if err := readStrict(path, &s); err != nil {
		return nil, err
	}
	if err := checkTypeMeta(path, s.APIVersion, s.Kind, KindQualification); err != nil {
		return nil, err
	}
	if err := s.validate(path); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Subject) validate(path string) error {
	if s.Spec.Subject == "" {
		return fmt.Errorf("%s: spec.subject is empty", path)
	}
	if s.Metadata.Name != "" && s.Metadata.Name != s.Spec.Subject {
		return fmt.Errorf("%s: metadata.name is %q but spec.subject is %q",
			path, s.Metadata.Name, s.Spec.Subject)
	}
	if len(s.Spec.Components) == 0 {
		return fmt.Errorf("%s: spec.components is empty, so the combination has no key", path)
	}

	seen := map[string]bool{}
	claim := func(name, list string) error {
		switch {
		case name == "":
			return fmt.Errorf("%s: a %s entry has no name", path, list)
		case name == s.Spec.Subject:
			return fmt.Errorf("%s: %q lists itself", path, name)
		case seen[name]:
			return fmt.Errorf("%s: %q is listed twice", path, name)
		}
		seen[name] = true
		return nil
	}

	for _, c := range s.Spec.Components {
		if err := claim(c.Name, "components"); err != nil {
			return err
		}
		if len(c.Selectors) == 0 {
			return fmt.Errorf("%s: component %q has no selectors, so it resolves to nothing", path, c.Name)
		}
	}
	for _, d := range s.Spec.Dependencies {
		if err := claim(d.Name, "dependencies"); err != nil {
			return err
		}
		if err := d.validate(path); err != nil {
			return err
		}
	}
	return nil
}
