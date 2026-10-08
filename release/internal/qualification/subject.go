// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
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

// SubjectDependency is one component resolved around the combination.
type SubjectDependency struct {
	Name string `json:"name"`

	// Constraint narrows what the strategy may resolve to. Empty means the
	// strategy decides alone.
	Constraint *Constraint `json:"constraint,omitempty"`
}

// Constraint is either a literal or a reference to a stream capability. A
// reference makes the dependency per stream, because its value is the
// stream's. A literal is the same for every stream.
type Constraint struct {
	Literal    string
	Capability string
}

// UnmarshalJSON reads a string as a literal and {capability: name} as a
// reference, and rejects anything else.
func (c *Constraint) UnmarshalJSON(b []byte) error {
	var literal string
	if err := json.Unmarshal(b, &literal); err == nil {
		if literal == "" {
			return fmt.Errorf("a constraint is empty")
		}
		*c = Constraint{Literal: literal}
		return nil
	}

	var ref struct {
		Capability string `json:"capability"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil {
		return fmt.Errorf("a constraint is a string or {capability: <name>}: %w", err)
	}
	if ref.Capability == "" {
		return fmt.Errorf("a constraint names no capability")
	}
	*c = Constraint{Capability: ref.Capability}
	return nil
}

// MarshalJSON writes the form UnmarshalJSON reads.
func (c Constraint) MarshalJSON() ([]byte, error) {
	if c.Capability != "" {
		return json.Marshal(struct {
			Capability string `json:"capability"`
		}{c.Capability})
	}
	return json.Marshal(c.Literal)
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
	}
	return nil
}
