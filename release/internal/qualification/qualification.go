// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package qualification reads the files that say what is qualified and turns
// them into a plan.
//
// Three files under release/ each carry one concern. The registry says how each
// component resolves to a version. The policy says which streams exist and how
// much of them a run covers. A subject says what one qualification moves
// together and what is resolved around it. The Planner reads all three and
// resolves a subject over a scope into a Plan, and the same inputs always
// produce the same plan in the same order.
package qualification

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// APIVersion is the group and version every kind carries. There is one, and a
// file naming another is rejected rather than coerced: the alternative is
// reading a future schema with today's structs and silently dropping the
// fields that were the reason for the bump.
const APIVersion = "nke.nvidia.com/v1alpha1"

// Kinds.
const (
	KindRegistry            = "Registry"
	KindQualificationPolicy = "QualificationPolicy"
	KindQualification       = "Qualification"
)

// Metadata names a file and says what it is for.
type Metadata struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// readStrict unmarshals YAML and rejects fields the structs do not declare. A
// misspelled key would otherwise read as an omitted one, and every field here
// is optional-looking enough that the result would be a plan quietly missing
// whatever the typo was meant to say.
func readStrict(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(b, into); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func checkTypeMeta(path, apiVersion, kind, wantKind string) error {
	if apiVersion != APIVersion {
		return fmt.Errorf("%s: apiVersion is %q, want %q", path, apiVersion, APIVersion)
	}
	if kind != wantKind {
		return fmt.Errorf("%s: kind is %q, want %q", path, kind, wantKind)
	}
	return nil
}
